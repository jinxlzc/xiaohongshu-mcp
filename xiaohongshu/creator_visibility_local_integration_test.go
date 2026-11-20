//go:build integration

package xiaohongshu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
)

// localCreatorClient 把导航映射到本地 HTTP 服务，状态探测仍看到目标站点 URL。
// 不连接小红书，不加载任何已有浏览器配置或登录态。
type localCreatorClient struct {
	rod.CDPClient
	origin string
}

func (c *localCreatorClient) Call(ctx context.Context, sessionID, method string, params interface{}) ([]byte, error) {
	if method == "Page.navigate" {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		var nav map[string]any
		if err := json.Unmarshal(raw, &nav); err != nil {
			return nil, err
		}
		target, _ := nav["url"].(string)
		if strings.HasPrefix(target, "https://creator.xiaohongshu.com/") {
			nav["url"] = c.origin + strings.TrimPrefix(target, "https://creator.xiaohongshu.com")
		} else if target != "about:blank" && !strings.HasPrefix(target, c.origin+"/") {
			return nil, fmt.Errorf("fixture 禁止外部导航")
		}
		params = nav
	}
	result, err := c.CDPClient.Call(ctx, sessionID, method, params)
	if err == nil && method == "Target.getTargetInfo" {
		result = []byte(strings.ReplaceAll(string(result), c.origin, "https://creator.xiaohongshu.com"))
	}
	return result, err
}

func creatorFixture(t *testing.T, html func(string) string) *rod.Page {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html(r.URL.Path)))
	}))
	t.Cleanup(server.Close)
	b := newSearchFixtureBrowser(t, func(client rod.CDPClient) rod.CDPClient {
		return &localCreatorClient{CDPClient: client, origin: server.URL}
	})
	page := b.MustPage().Timeout(12 * time.Second)
	t.Cleanup(func() { _ = page.Close() })
	return page
}

func TestCreatorAgreementLocal(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"必要协议", "创作者服务协议", true},
		{"营销选项", "营销推广协议", false},
		{"付费选项", "付费会员服务协议", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := creatorFixture(t, func(string) string {
				return `<div role="dialog">` + tc.body + `<input type="checkbox"><button onclick="this.parentNode.remove();document.querySelector('a').style.display='block'">同意</button></div><a style="display:none" href="/new/note-manager">笔记管理</a>`
			})
			require.NoError(t, page.Navigate(urlOfCreatorHome))
			require.NoError(t, page.WaitLoad())
			state, err := creatorPageStatus(page)
			require.NoError(t, err)
			require.Equal(t, "agreement_required", state.Status)
			if tc.want {
				require.NoError(t, EnsureCreatorSession(page.GetContext(), page))
				state, err = creatorPageStatus(page)
				require.NoError(t, err)
				require.True(t, state.IsLoggedIn)
			} else {
				clicked, err := acceptCreatorAgreement(page)
				require.NoError(t, err)
				require.False(t, clicked)
				require.False(t, page.MustElement("input").MustProperty("checked").Bool())
			}
		})
	}
}

func TestCreatorQrcodeLocal(t *testing.T) {
	page := creatorFixture(t, func(string) string {
		return `<div class="login-container"><div class="qrcode"><canvas width="100" height="100"></canvas></div></div><script>setTimeout(()=>{const c=document.querySelector('canvas').getContext('2d');c.fillStyle='white';c.fillRect(0,0,100,100);c.fillStyle='black';for(let y=0;y<10;y++)for(let x=0;x<10;x++)if((x+y)%2)c.fillRect(x*10,y*10,10,10);document.body.dataset.ready='yes'},600)</script>`
	})
	img, loggedIn, err := FetchCreatorQrcodeImage(page.GetContext(), page)
	require.NoError(t, err)
	require.False(t, loggedIn)
	require.Equal(t, "yes", page.MustEval("() => document.body.dataset.ready").String())
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(img, "data:image/png;base64,"))
	require.NoError(t, err)
	require.Equal(t, []byte{137, 80, 78, 71, 13, 10, 26, 10}, data[:8])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, WaitForCreatorLogin(ctx, page))
}

func TestNoteVisibilityLocal(t *testing.T) {
	const id = "0123456789abcdef01234567"
	for _, tc := range []struct {
		name, initial, target string
		fail                  bool
	}{
		{"私密转公开", "仅自己可见", "公开可见", false},
		{"公开转私密", "公开可见", "仅自己可见", false},
		{"互关好友", "公开可见", "仅互关好友可见", false},
		{"幂等不提交", "仅自己可见", "仅自己可见", false},
		{"服务端未保存", "公开可见", "仅自己可见", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var current atomic.Value
			current.Store(tc.initial)
			var writes atomic.Int32
			page := creatorFixture(t, func(path string) string {
				if strings.HasPrefix(path, "/save/") {
					writes.Add(1)
					if !tc.fail {
						index, err := strconv.Atoi(strings.TrimPrefix(path, "/save/"))
						if err == nil && index >= 0 && index < 3 {
							current.Store([]string{"公开可见", "仅自己可见", "仅互关好友可见"}[index])
						}
					}
					return "ok"
				}
				if path != "/new/note-manager" {
					return `<a href="/new/note-manager">笔记管理</a>`
				}
				html := `<div class="d-options-wrapper"><label class="selected">公开可见</label><label>仅自己可见</label></div><div class="note-item" data-note-id="ffffffffffffffffffffffff"><button onclick="document.body.dataset.wrong='true'">权限设置</button></div><div class="note-item" data-note-id="` + id + `"><button onclick="document.querySelector('[role=dialog]').style.display='block'">权限设置</button></div><div role="dialog" style="display:none">`
				for i, v := range []string{"公开可见", "仅自己可见", "仅互关好友可见"} {
					html += fmt.Sprintf(`<label><input type="radio" name="visibility" value="%d" %s>%s</label>`, i, map[bool]string{true: "checked"}[current.Load().(string) == v], v)
				}
				return html + `<button onclick="fetch('/save/'+document.querySelector('input:checked').value).then(()=>this.parentNode.style.display='none')">保存</button></div>`
			})
			result, err := SetNoteVisibility(page.GetContext(), page, id, tc.target)
			if tc.fail {
				require.ErrorContains(t, err, "回读不符")
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.initial, result.Before)
				require.Equal(t, tc.target, result.After)
				if tc.initial == tc.target {
					require.Equal(t, "skipped", result.Status)
					require.Zero(t, writes.Load())
				} else {
					require.Equal(t, "updated", result.Status)
					require.EqualValues(t, 1, writes.Load())
				}
			}
			require.Empty(t, page.MustEval("() => document.body.dataset.wrong || ''").String())
		})
	}
}

func TestNotePermissionAmbiguousLocal(t *testing.T) {
	page := creatorFixture(t, func(string) string {
		return `<div class="note-item"><button onclick="document.querySelectorAll('[role=dialog]').forEach(el=>el.style.display='block')">权限设置</button></div><div role="dialog" style="display:none"><label>公开可见</label><label>仅自己可见</label></div><div role="dialog" style="display:none"><label>公开可见</label><label>仅自己可见</label></div>`
	})
	require.NoError(t, page.Navigate(urlOfNoteManager))
	require.NoError(t, page.WaitLoad())
	_, err := openNotePermission(page, page.MustElement(".note-item"))
	require.ErrorContains(t, err, "多个权限面板")
}

func TestManagedNotePaginationLocal(t *testing.T) {
	const id = "0123456789abcdef01234567"
	page := creatorFixture(t, func(string) string {
		return `<div class="note-item" data-note-id="ffffffffffffffffffffffff"></div><button aria-label="下一页" onclick="document.querySelector('.note-item').dataset.noteId='` + id + `';this.disabled=true">下一页</button>`
	})
	require.NoError(t, page.Navigate(urlOfNoteManager))
	require.NoError(t, page.WaitLoad())
	card, err := findManagedNote(page, id)
	require.NoError(t, err)
	got, err := managedNoteID(card)
	require.NoError(t, err)
	require.Equal(t, id, got)
	_, err = findManagedNote(page, "aaaaaaaaaaaaaaaaaaaaaaaa")
	require.ErrorContains(t, err, "未找到笔记")
}

func TestCreatorLoginRedirectLocal(t *testing.T) {
	page := creatorFixture(t, func(path string) string {
		if path == "/ready" {
			return `<a href="/new/note-manager">笔记管理</a>`
		}
		return `<div class="login-container">扫码登录</div><script>setTimeout(()=>location.href='/ready',300)</script>`
	})
	require.NoError(t, page.Navigate(urlOfCreatorHome))
	require.True(t, WaitForCreatorLogin(page.GetContext(), page))
}
