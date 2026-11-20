//go:build integration

package xiaohongshu

import (
	"context"
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
					state.URL = urlOfNoteManager
				}
				return state
			})
			if tc.safe {
				require.NoError(t, err)
				require.Equal(t, urlOfNoteManager, last.URL)
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
