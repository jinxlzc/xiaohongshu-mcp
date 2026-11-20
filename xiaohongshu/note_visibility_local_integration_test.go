//go:build integration

package xiaohongshu

import (
	"context"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 仅在 CDP 测试替身返回会话标记，无值、无真实登录态，不读浏览器凭证。
type visibilityFixtureClient struct{ rod.CDPClient }

func (c *visibilityFixtureClient) Call(ctx context.Context, sessionID, method string, params interface{}) ([]byte, error) {
	if method == "Network.getAllCookies" || method == "Storage.getCookies" {
		return []byte(`{"cookies":[{"name":"galaxy_creator_session_id"}]}`), nil
	}
	return c.CDPClient.Call(ctx, sessionID, method, params)
}
func visibilityFixture(t *testing.T, html func(string) string) *rod.Page {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html(r.URL.Path)))
	}))
	t.Cleanup(server.Close)
	b := newSearchFixtureBrowser(t, func(client rod.CDPClient) rod.CDPClient {
		return &visibilityFixtureClient{&localCreatorClient{CDPClient: client, origin: server.URL}}
	})
	page := b.MustPage().Timeout(12 * time.Second)
	t.Cleanup(func() { _ = page.Close() })
	return page
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
			page := visibilityFixture(t, func(path string) string {
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
	page := visibilityFixture(t, func(string) string {
		return `<div class="note-item"><button onclick="document.querySelectorAll('[role=dialog]').forEach(el=>el.style.display='block')">权限设置</button></div><div role="dialog" style="display:none"><label>公开可见</label><label>仅自己可见</label></div><div role="dialog" style="display:none"><label>公开可见</label><label>仅自己可见</label></div>`
	})
	require.NoError(t, page.Navigate(urlOfNoteManager))
	require.NoError(t, page.WaitLoad())
	_, err := openNotePermission(page, page.MustElement(".note-item"))
	require.ErrorContains(t, err, "多个权限面板")
}

func TestManagedNotePaginationLocal(t *testing.T) {
	const id = "0123456789abcdef01234567"
	page := visibilityFixture(t, func(string) string {
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
