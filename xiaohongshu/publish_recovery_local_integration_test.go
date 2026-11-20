//go:build integration

package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/require"
)

// 导航在 CDP 边界映射为本地 DOM，真实执行选择、截图和点击，不发网络请求。
type publishFixtureClient struct {
	rod.CDPClient
	html    func(string) string
	mu      sync.Mutex
	targets map[string]string
	urls    map[string]string
}

func (c *publishFixtureClient) Call(ctx context.Context, sessionID, method string, params interface{}) ([]byte, error) {
	if method == "Page.navigate" {
		raw, _ := json.Marshal(params)
		var nav proto.PageNavigate
		if err := json.Unmarshal(raw, &nav); err != nil {
			return nil, err
		}
		u, err := url.Parse(nav.URL)
		if err != nil {
			return nil, err
		}
		if u.Host != "creator.xiaohongshu.com" {
			return nil, fmt.Errorf("fixture 禁止外部导航")
		}
		raw, err = c.CDPClient.Call(ctx, sessionID, "Page.getFrameTree", proto.PageGetFrameTree{})
		if err != nil {
			return nil, err
		}
		var tree proto.PageGetFrameTreeResult
		if err := json.Unmarshal(raw, &tree); err != nil {
			return nil, err
		}
		frame := tree.FrameTree.Frame.ID
		_, err = c.CDPClient.Call(ctx, sessionID, "Page.setDocumentContent", proto.PageSetDocumentContent{FrameID: frame, HTML: c.html(u.Path)})
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.urls[c.targets[sessionID]] = nav.URL
		c.mu.Unlock()
		return json.Marshal(proto.PageNavigateResult{FrameID: frame})
	}
	raw, err := c.CDPClient.Call(ctx, sessionID, method, params)
	if err != nil {
		return nil, err
	}
	if method == "Target.attachToTarget" {
		in, _ := json.Marshal(params)
		var target proto.TargetAttachToTarget
		var attached proto.TargetAttachToTargetResult
		if err := json.Unmarshal(in, &target); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &attached); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.targets[string(attached.SessionID)] = string(target.TargetID)
		c.mu.Unlock()
	}
	if method == "Target.getTargetInfo" {
		var info proto.TargetGetTargetInfoResult
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil, err
		}
		c.mu.Lock()
		u := c.urls[string(info.TargetInfo.TargetID)]
		c.mu.Unlock()
		if u != "" {
			info.TargetInfo.URL = u
			return json.Marshal(info)
		}
	}
	return raw, nil
}

func publishRecoveryFixture(t *testing.T, html func(string) string) *rod.Page {
	t.Helper()
	b := newSearchFixtureBrowser(t, func(client rod.CDPClient) rod.CDPClient {
		return &publishFixtureClient{CDPClient: client, html: html, targets: map[string]string{}, urls: map[string]string{}}
	})
	page := b.MustPage().Timeout(15 * time.Second)
	t.Cleanup(func() { _ = page.Close() })
	return page
}

const recoveryDraft = `<div class="d-input"><input value="原稿标题"></div><div class="img-preview-area"><div class="pr" style="width:80px;height:80px">图1</div><div class="pr" style="width:80px;height:80px">图2</div></div><div class="publish-page-publish-btn"><button class="bg-red" onclick="document.body.dataset.clicks=Number(document.body.dataset.clicks||0)+1">发布</button></div><button onclick="document.body.dataset.bad='true'">暂存离开</button>`

func TestPublishRecoveryDraftGuardLocal(t *testing.T) {
	for _, tc := range []struct {
		name, mutation string
		ready          bool
	}{
		{"完整稿件", "", true},
		{"空标题", `document.querySelector('input').value=''`, false},
		{"不同标题", `document.querySelector('input').value='其他稿件'`, false},
		{"缺图", `document.querySelector('.pr').remove()`, false},
		{"隐藏图片", `document.querySelector('.pr').style.display='none'`, false},
		{"按钮禁用", `document.querySelector('.bg-red').disabled=true`, false},
		{"验证层", `document.body.insertAdjacentHTML('beforeend','<div role="dialog">实名验证</div>')`, false},
		{"遮挡层", `document.body.insertAdjacentHTML('beforeend','<div style="position:fixed;inset:0;background:white">加载中</div>')`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := publishRecoveryFixture(t, func(string) string { return recoveryDraft })
			page.MustNavigate(urlOfPublic).MustWaitLoad()
			if tc.mutation != "" {
				page.MustEval(`() => {` + tc.mutation + `}`)
			}
			attempt := publishAttempt{Title: "原稿标题", ImageCount: 2, Started: time.Now()}
			state := observePublish(page)
			require.Equal(t, tc.ready, retryPublishReady(page, state, attempt))
			err := retryPublish(page, attempt)
			if tc.ready {
				require.NoError(t, err)
				require.Equal(t, "1", page.MustEval(`()=>document.body.dataset.clicks`).Str())
			} else {
				require.Error(t, err)
				require.Empty(t, page.MustEval(`()=>document.body.dataset.clicks||''`).Str())
			}
			require.Empty(t, page.MustEval(`()=>document.body.dataset.bad||''`).Str())
		})
	}
}

func TestPublishRecoverySecondScanLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := publishRecoveryFixture(t, func(string) string { return recoveryDraft + `<div role="alert">验证成功, 正在登录...</div>` })
	page.MustNavigate(urlOfPublic).MustWaitLoad()
	page.MustEval(`()=>{document.querySelector('.bg-red').onclick=()=>{document.body.dataset.clicks=Number(document.body.dataset.clicks||0)+1;document.body.insertAdjacentHTML('beforeend','<section role="dialog"><h3>扫码验证</h3><canvas width="200" height="200"></canvas><button onclick="document.body.dataset.bad=1">取消</button></section>')}}`)
	// 旧文件必须被第二轮二维码覆盖。
	require.NoError(t, os.WriteFile("verify_qr.png", []byte("old-qr"), 0600))
	attempt := publishAttempt{Title: "原稿标题", ImageCount: 2}
	ctx, cancel := context.WithTimeout(page.GetContext(), 4*time.Second)
	defer cancel()
	_, err := monitorPublish(ctx, time.Second, func(ctx context.Context) publishObservation {
		state := observePublish(page.Context(ctx))
		if retryPublishReady(page.Context(ctx), state, attempt) {
			state.retry = func(ctx context.Context) error { return retryPublish(page.Context(ctx), attempt) }
		}
		return state
	})
	require.ErrorContains(t, err, "需要账号本人扫码验证")
	data, err := os.ReadFile("verify_qr.png")
	require.NoError(t, err)
	require.NotEqual(t, "old-qr", string(data))
	require.FileExists(t, "verify_qr.json")
	require.Equal(t, "1", page.MustEval(`()=>document.body.dataset.clicks`).Str())
	require.Empty(t, page.MustEval(`()=>document.body.dataset.bad||''`).Str())
}

func TestPublishRecoveryLookupLocal(t *testing.T) {
	for _, mode := range []string{"match", "wrong-title", "old", "missing-time", "ambiguous-minute", "draft", "unreadable", "login"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			started := time.Now().Add(-5 * time.Second)
			title := "原稿标题"
			stamp := started.Add(time.Second).Format(time.RFC3339)
			if mode == "wrong-title" {
				title = "另一篇"
			}
			if mode == "old" {
				stamp = started.Add(-time.Hour).Format(time.RFC3339)
			}
			if mode == "missing-time" {
				stamp = "刚刚"
			}
			if mode == "ambiguous-minute" {
				stamp = started.Truncate(time.Minute).Format(time.RFC3339)
			}
			page := publishRecoveryFixture(t, func(path string) string {
				if path == "/new/note-manager" {
					if mode == "unreadable" {
						return `<h1>加载失败</h1>`
					}
					if mode == "login" {
						return `<div role="dialog">扫码登录</div>`
					}
					status := ""
					if mode == "draft" {
						status = `<div class="note-status">草稿</div>`
					}
					return fmt.Sprintf(`<div class="note-item" data-note-id="0123456789abcdef01234567">%s<div class="note-title">%s</div><time datetime="%s">%s</time><button onclick="document.body.dataset.bad=1">删除</button></div>`, status, title, stamp, stamp)
				}
				return recoveryDraft + `<div role="status">发布成功</div>`
			})
			page.MustNavigate(urlOfPublic).MustWaitLoad()
			// 限制回读等待，真实生产配置仍为45秒。
			ctx, cancel := context.WithTimeout(page.GetContext(), 2*time.Second)
			defer cancel()
			result, err := waitPublishResult(page.Context(ctx), time.Millisecond, publishAttempt{Title: "原稿标题", ImageCount: 2, Started: started})
			if mode == "match" {
				require.NoError(t, err)
				require.Equal(t, "0123456789abcdef01234567", result.NoteID)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				files, e := os.ReadDir("diag")
				require.NoError(t, e)
				require.NotEmpty(t, files)
			}
			require.True(t, strings.Contains(page.MustInfo().URL, "/publish/publish"), "回读保留原编辑页")
			require.Equal(t, "原稿标题", page.MustElement("input").MustProperty("value").Str())
			require.Empty(t, page.MustEval(`()=>document.body.dataset.clicks||''`).Str(), "没有验证不能补点")
		})
	}
}

func TestPublishRecoveryLandingIDLocal(t *testing.T) {
	page := publishRecoveryFixture(t, func(string) string { return `<h1>发布成功</h1>` })
	page.MustNavigate("https://creator.xiaohongshu.com/publish/success?note_id=0123456789abcdef01234567&token=secret").MustWaitLoad()
	result, err := waitPublishResult(page, time.Second, publishAttempt{Title: "原稿标题", Started: time.Now()})
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef01234567", result.NoteID)
}
