//go:build integration

package xiaohongshu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
)

// 直接装载本地 DOM，不依赖导航代理或外网。
func publishResultFixture(t *testing.T, html func(string) string) *rod.Page {
	t.Helper()
	b := newMentionFixtureBrowser(t)
	page := b.MustPage("about:blank").Timeout(12 * time.Second)
	t.Cleanup(func() { _ = page.Close() })
	require.NoError(t, page.SetDocumentContent(html("/publish/publish")))
	return page
}

func TestPublishResultLocal(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		safe       bool
	}{
		{"简单确认", "确认发布笔记？", true},
		{"AI声明", "是否 AI 生成？请声明 AI 内容", false},
		{"新协议", "请同意新版创作者服务协议", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := publishResultFixture(t, func(path string) string {
				if path == "/new/note-manager" {
					return `<h1>笔记管理</h1>`
				}
				return `<div role="dialog">` + tc.body + `<button onclick="document.body.innerHTML='笔记管理';document.title='成功落地页'">发布</button></div>`
			})
			require.NoError(t, page.WaitLoad())
			last, err := monitorPublish(page.GetContext(), 3*time.Second, func(ctx context.Context) publishObservation {
				state := observePublish(page.Context(ctx))
				state.URL = urlOfPublic
				if state.Title == "成功落地页" {
					state.URL = publishNoteManagerURL
				}
				return state
			})
			if tc.safe {
				require.NoError(t, err)
				require.Equal(t, publishNoteManagerURL, last.URL)
			} else {
				require.ErrorContains(t, err, "需要账号本人处理")
				require.True(t, page.MustElement(`[role="dialog"]`).MustVisible())
			}
		})
	}
}

func TestPublishPreflightLocal(t *testing.T) {
	for _, tc := range []struct {
		name, title, counter string
		invalid              bool
	}{
		{"原稿emoji", "没带筹码？我现场写了个德扑记分器🃏", "18/20", false},
		{"emoji超限无计数节点", "一二三四五六七八九十一二三四五六七八九🃏", "", true},
		{"站点计数超限", "短标题", "21/20", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := publishResultFixture(t, func(string) string {
				return `<div class="title-container"><div class="d-input"><input value="` + tc.title + `"></div>` + map[bool]string{true: `<div class="max_suffix">` + tc.counter + `</div>`}[tc.counter != ""] + `</div>`
			})
			require.NoError(t, page.WaitLoad())
			err := checkTitleMaxLength(page)
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestPublishDiagnosticLocal(t *testing.T) {
	page := publishResultFixture(t, func(string) string {
		return `<title>测试稿件</title><div class="title-container"><div class="d-input"><input value="不应改稿"></div></div><div role="alert">标题校验未通过</div><p style="color:rgb(220, 0, 0)">红字错误</p><div role="dialog">需要实名验证<button>确认</button></div><xhs-publish-btn style="display:block;width:100px;height:40px" is-publish="true" submit-disabled="true" aria-disabled="true">发布</xhs-publish-btn><p>token=visible-secret</p>`
	})
	require.NoError(t, page.WaitLoad())
	state := observePublish(page)
	require.Empty(t, state.ReadErrors)
	require.Contains(t, state.Prompts, "红字错误")
	require.Contains(t, state.Prompts, "标题校验未通过")
	require.True(t, state.ButtonFound)
	require.Equal(t, "widget", state.Buttons[0].Version)
	require.Equal(t, "true", state.Buttons[0].Attributes["submit-disabled"])
	data := publishDiagnostic{StartedAt: time.Now(), CapturedAt: time.Now(), publishObservation: state}
	dir, err := savePublishDiagnostic(filepath.Join(t.TempDir(), "diag"), data, func() ([]byte, error) { return publishScreenshot(page) })
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "-secret")
	require.FileExists(t, filepath.Join(dir, "page.png"))
	require.True(t, page.MustElement("p").MustVisible(), "截图遮盖应还原")
	require.Equal(t, "不应改稿", page.MustElement("input").MustProperty("value").Str())
}

func TestPublishDismissTopicsLocal(t *testing.T) {
	page := publishResultFixture(t, func(string) string {
		return `<div class="d-input"><input value="标题"></div><div contenteditable="true"><a data-topic='{"name":"测试话题"}'>#测试话题</a></div><div id="creator-editor-topic-container">联想遮挡</div><div class="publish-page-publish-btn"><button class="bg-red" onclick="document.body.dataset.clicked='true'">发布</button></div><script>document.addEventListener('keydown',e=>{if(e.key==='Escape')document.querySelector('#creator-editor-topic-container').remove()})</script>`
	})
	require.NoError(t, page.WaitLoad())
	require.NoError(t, clickPublishButton(page))
	require.Equal(t, "true", page.MustEval(`() => document.body.dataset.clicked`).Str())
	require.Equal(t, 1, len(page.MustElements("a[data-topic]")))
	require.Equal(t, 0, len(page.MustElements("#creator-editor-topic-container")))
}

func TestPublishUnconfirmedDiagnosticLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := publishResultFixture(t, func(string) string { return `<div style="height:1400px"><div role="alert">请检查标题</div></div>` })
	err := waitPublishSuccess(page, 30*time.Millisecond)
	require.ErrorContains(t, err, "发布未确认成功")
	require.ErrorContains(t, err, "请检查标题")
	require.ErrorContains(t, err, "URL=")
	files, globErr := filepath.Glob("diag/*/metadata.json")
	require.NoError(t, globErr)
	require.Len(t, files, 1)
	raw, readErr := os.ReadFile(files[0])
	require.NoError(t, readErr)
	require.Contains(t, string(raw), "publish_started_at")
	require.Contains(t, string(raw), "请检查标题")
	pngFile, openErr := os.Open(filepath.Join(filepath.Dir(files[0]), "page.png"))
	require.NoError(t, openErr)
	defer pngFile.Close()
	config, decodeErr := png.DecodeConfig(pngFile)
	require.NoError(t, decodeErr)
	require.GreaterOrEqual(t, config.Height, 1400, "应保存整页截图")
}

func TestPublishButtonStateLocal(t *testing.T) {
	for _, attr := range []string{`submit-disabled="true"`, `disabled`, `aria-disabled="true"`} {
		t.Run(attr, func(t *testing.T) {
			page := publishResultFixture(t, func(string) string {
				return `<xhs-publish-btn style="display:block;width:100px;height:40px" is-publish="true" ` + attr + `>发布</xhs-publish-btn>`
			})
			button, reason, err := findPublishButton(page)
			require.NoError(t, err)
			require.NotNil(t, button)
			require.NotEmpty(t, reason)
		})
	}
}

func TestPublishScanVerifyDetectionLocal(t *testing.T) {
	page := publishResultFixture(t, func(string) string {
		return `<div class="d-input"><input value="原稿"></div><section style="background:#222;color:white;width:400px;padding:24px"><h3>扫码验证</h3><p>为保护账号安全，请使用已登录该账号的「<span>小红书APP</span>」扫码验证身份</p><img width="72" height="72"><p>测试账号</p><canvas width="200" height="200"></canvas><p>二维码1分钟失效</p><button>问题反馈</button><button>×</button></section>`
	})
	state := observePublish(page)
	require.NotEmpty(t, state.Dialogs, "可见的扫码弹窗不能漏检")
	if len(state.Dialogs) > 0 {
		require.Contains(t, state.Dialogs[0].Text, "扫码验证")
		_, safe := safePublishConfirmation(state.Dialogs[0])
		require.False(t, safe)
	}
}

// 仅绘制本地测试图案，不使用现场二维码或真实凭证。
const scanVerifyFixture = `<style>section{background:#222;color:white;width:400px;padding:24px}canvas{display:block;background:white}#refresh{display:none}</style>
<div class="d-input"><input value="原稿"></div><div contenteditable="true">正文</div><button onclick="document.body.dataset.bad='publish'">发布</button>
<section id="scan"><h3>扫码验证</h3><p>为保护账号安全，请使用已登录该账号的「<span style="color:red">小红书APP</span>」扫码验证身份</p><img width="72" height="72"><p>测试账号</p><canvas width="200" height="200"></canvas><p id="expiry">二维码1分钟失效</p><span id="refresh" onclick="refreshQR()">刷新二维码</span><button onclick="document.body.dataset.bad='feedback'">问题反馈</button><button onclick="document.body.dataset.bad='close'">×</button><button onclick="document.body.dataset.bad='cancel'">取消</button><button onclick="document.body.dataset.bad='unlink'">取消关联</button></section>
<script>
let version=0;
function paint(){const ctx=document.querySelector('canvas').getContext('2d');ctx.fillStyle='white';ctx.fillRect(0,0,200,200);ctx.fillStyle='black';for(let y=0;y<10;y++)for(let x=0;x<10;x++)if((x+y+version)%3)ctx.fillRect(x*20,y*20,15,15)}
function refreshQR(){version++;document.body.dataset.refresh=String(version);paint();document.querySelector('#expiry').textContent='二维码1分钟失效';document.querySelector('#refresh').style.display='none'}
paint();
</script>`

func TestPublishScanVerifyFilesRefreshLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := publishResultFixture(t, func(string) string { return scanVerifyFixture })
	state := observePublish(page)
	require.Empty(t, state.ReadErrors)
	require.NotNil(t, state.Scan)
	wait := publishScanWait{}
	require.NoError(t, wait.update(page.GetContext(), state.Scan))
	read := func() (publishQRMetadata, []byte) {
		raw, err := os.ReadFile("verify_qr.json")
		require.NoError(t, err)
		require.NotContains(t, string(raw), "token")
		require.NotContains(t, string(raw), "测试账号")
		var meta publishQRMetadata
		require.NoError(t, json.Unmarshal(raw, &meta))
		require.Equal(t, time.Minute, meta.ExpiresAt.Sub(meta.WrittenAt))
		data, err := os.ReadFile("verify_qr.png")
		require.NoError(t, err)
		config, err := png.DecodeConfig(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, 200, config.Width)
		require.Equal(t, 200, config.Height)
		return meta, data
	}
	first, png1 := read()
	page.MustEval(`() => {document.querySelector('#expiry').textContent='二维码已过期';document.querySelector('#refresh').style.display='block'}`)
	wait.expires = time.Now().Add(-time.Second)
	state = observePublish(page)
	require.NoError(t, wait.update(page.GetContext(), state.Scan))
	require.Equal(t, "1", page.MustEval(`() => document.body.dataset.refresh`).Str())
	require.NoError(t, wait.update(page.GetContext(), observePublish(page).Scan))
	second, png2 := read()
	require.True(t, second.WrittenAt.After(first.WrittenAt))
	require.NotEqual(t, png1, png2)
	require.Empty(t, page.MustEval(`() => document.body.dataset.bad || ''`).Str())
	require.Equal(t, "原稿", page.MustElement("input").MustProperty("value").Str())
}

func TestPublishScanVerifyTextVariantsLocal(t *testing.T) {
	for _, text := range []string{"扫码验证", "扫码验证身份", "二维码1分钟失效", "请使用小红书APP扫码"} {
		t.Run(text, func(t *testing.T) {
			page := publishResultFixture(t, func(string) string {
				return `<section><h3>` + text + `</h3><canvas width="200" height="200"></canvas></section>`
			})
			require.NotNil(t, observePublish(page).Scan)
		})
	}
	t.Run("隐藏弹窗与原稿不触发验证", func(t *testing.T) {
		page := publishResultFixture(t, func(string) string {
			return `<section style="display:none"><h3>扫码验证</h3><canvas width="200" height="200"></canvas></section><div contenteditable="true"><p>请使用小红书APP扫码验证身份</p><img width="200" height="200"></div>`
		})
		require.Nil(t, observePublish(page).Scan)
	})
}

func TestPublishScanVerifyFailureDiagnosticLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := publishResultFixture(t, func(string) string { return scanVerifyFixture })
	page.MustEval(`() => document.querySelector('#expiry').textContent='二维码已过期'`)
	err := waitPublishSuccess(page, time.Second)
	require.ErrorContains(t, err, "需要账号本人扫码验证")
	require.FileExists(t, "verify_qr.png")
	files, e := filepath.Glob("diag/*/metadata.json")
	require.NoError(t, e)
	require.Len(t, files, 1)
	raw, e := os.ReadFile(files[0])
	require.NoError(t, e)
	require.Contains(t, string(raw), "扫码验证")
	require.Contains(t, string(raw), "scan_verification")
	require.Empty(t, page.MustEval(`() => document.body.dataset.bad || ''`).Str())
}

func TestPublishScanVerifySiblingRefreshLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := publishResultFixture(t, func(string) string {
		return `<section><div><p>二维码1分钟失效</p><canvas width="200" height="200"></canvas></div><button onclick="document.body.dataset.refreshed='true'">刷新二维码</button><button onclick="document.body.dataset.bad='true'">取消</button></section>`
	})
	state := observePublish(page)
	require.NotNil(t, state.Scan)
	require.True(t, state.Scan.RefreshAvailable)
	require.NoError(t, state.Scan.refresh(page.GetContext()))
	require.Equal(t, "true", page.MustEval(`() => document.body.dataset.refreshed`).Str())
	require.Empty(t, page.MustEval(`() => document.body.dataset.bad || ''`).Str())
}

func TestPublishScanVerifyRefreshRedrawLocal(t *testing.T) {
	for _, placeholder := range []bool{false, true} {
		t.Run(fmt.Sprint(placeholder), func(t *testing.T) {
			t.Chdir(t.TempDir())
			page := publishResultFixture(t, func(string) string { return scanVerifyFixture })
			page.MustEval(`placeholder => {
   document.querySelector('#expiry').textContent='二维码已过期';
   const refresh=document.querySelector('#refresh');refresh.style.display='block';
   refresh.onclick=()=>{document.body.dataset.refresh='1';document.querySelector('canvas').style.display='none';refresh.style.display='none'};
   if(placeholder)document.querySelector('canvas').style.display='none';
  }`, placeholder)
			wait := publishScanWait{}
			require.NoError(t, wait.update(page.GetContext(), observePublish(page).Scan))
			require.Equal(t, "1", page.MustEval(`() => document.body.dataset.refresh`).Str())
			missing := observePublish(page)
			require.NotNil(t, missing.Scan)
			require.Nil(t, missing.Scan.capture)
			require.NoError(t, wait.update(page.GetContext(), missing.Scan))
			page.MustEval(`() => {version++;paint();document.querySelector('canvas').style.display='block';document.querySelector('#expiry').textContent='二维码1分钟失效'}`)
			require.NoError(t, wait.update(page.GetContext(), observePublish(page).Scan))
			require.FileExists(t, "verify_qr.png")
			require.FileExists(t, "verify_qr.json")
			require.Empty(t, page.MustEval(`() => document.body.dataset.bad || ''`).Str())
		})
	}
}

func TestPublishScanVerifyRejectsUnsafeRefreshAncestorLocal(t *testing.T) {
	page := publishResultFixture(t, func(string) string {
		return `<section><p>扫码验证</p><canvas width="200" height="200"></canvas><button onclick="document.body.dataset.bad='true'">问题反馈<span>刷新</span></button></section>`
	})
	state := observePublish(page)
	require.NotNil(t, state.Scan)
	require.False(t, state.Scan.RefreshAvailable)
	require.Error(t, state.Scan.refresh(page.GetContext()))
	require.Empty(t, page.MustEval(`() => document.body.dataset.bad || ''`).Str())
}
