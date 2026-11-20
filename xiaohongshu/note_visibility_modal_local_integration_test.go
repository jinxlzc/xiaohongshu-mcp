//go:build integration

package xiaohongshu

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/input"
	"github.com/stretchr/testify/require"
)

func permissionModalFixture(t *testing.T, value string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/note_visibility_permission_modal.html")
	require.NoError(t, err)
	return strings.ReplaceAll(string(data), "__VALUE__", value)
}

func TestNoteVisibilityModalLocal(t *testing.T) {
	for _, tc := range []struct {
		name, initial, target string
		inline, fail          bool
	}{
		{name: "公开幂等取消", initial: "公开可见", target: "公开可见"},
		{name: "私密幂等取消", initial: "仅自己可见", target: "仅自己可见"},
		{name: "互关幂等取消", initial: "仅互关好友可见", target: "仅互关好友可见"},
		{name: "公开转私密_外置下拉层", initial: "公开可见", target: "仅自己可见"},
		{name: "私密转公开_内置下拉层", initial: "仅自己可见", target: "公开可见", inline: true},
		{name: "公开转互关", initial: "公开可见", target: "仅互关好友可见"},
		{name: "提交未持久化必须失败", initial: "公开可见", target: "仅自己可见", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var current atomic.Value
			current.Store(tc.initial)
			var writes, navigations atomic.Int32
			html := permissionModalFixture(t, "__VALUE__")
			page := creatorFixture(t, func(path string) string {
				if strings.HasPrefix(path, "/save/") {
					writes.Add(1)
					index, err := strconv.Atoi(strings.TrimPrefix(path, "/save/"))
					if !tc.fail && err == nil && index >= 0 && index < 3 {
						current.Store([]string{"公开可见", "仅自己可见", "仅互关好友可见"}[index])
					}
					return "ok"
				}
				if path != "/new/note-manager" {
					return `<a href="/new/note-manager">笔记管理</a>`
				}
				navigations.Add(1)
				result := strings.ReplaceAll(html, "__VALUE__", current.Load().(string))
				if tc.inline {
					result += `<script>document.querySelector('#modal').append(document.querySelector('#choices'))</script>`
				}
				return result
			}).Context(context.Background()).Timeout(40 * time.Second)
			defer page.CancelTimeout()
			result, err := SetNoteVisibility(page.GetContext(), page, fixtureNoteID, tc.target)
			if tc.fail {
				require.ErrorContains(t, err, "回读不符")
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.initial, result.Before)
				require.Equal(t, tc.target, result.After)
				require.False(t, page.MustElement("#modal").MustVisible())
			}
			events := page.MustEval(`() => localStorage.events || ''`).String()
			if tc.initial == tc.target {
				require.Equal(t, "skipped", result.Status)
				require.Zero(t, writes.Load())
				require.EqualValues(t, 1, navigations.Load())
				require.Equal(t, "icon,cancel,", events)
			} else {
				require.EqualValues(t, 1, writes.Load())
				require.EqualValues(t, 2, navigations.Load())
				if tc.fail {
					require.Equal(t, "icon,expand,select,confirm,icon,", events)
				} else {
					require.Equal(t, "updated", result.Status)
					require.Equal(t, "icon,expand,select,confirm,icon,cancel,", events)
				}
			}
		})
	}
}

func TestNotePermissionModalCancelAfterEscapeLocal(t *testing.T) {
	html := permissionModalFixture(t, "公开可见")
	// 同时验证真实按钮 class 的兜底，不依赖 button 标签或 d-button。
	html = strings.ReplaceAll(html, " d-button", "")
	page := creatorFixture(t, func(string) string { return html })
	require.NoError(t, page.Navigate(urlOfNoteManager))
	require.NoError(t, page.WaitLoad())
	panel, err := openNotePermission(page, page.MustElement(".note-card"))
	require.NoError(t, err)
	value, err := selectedNoteVisibility(panel)
	require.NoError(t, err)
	require.Equal(t, "公开可见", value)
	require.NoError(t, page.Keyboard.Type(input.Escape))
	require.True(t, panel.MustVisible())
	require.NoError(t, closeNoteActionPanel(panel))
	require.False(t, panel.MustVisible())
	require.Equal(t, "icon,escape,cancel,", page.MustEval(`() => localStorage.events`).String())
}

func TestNotePermissionModalIdentityLocal(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		want           bool
	}{
		{"完整弹窗", "", "", true},
		{"分享弹窗不能冒充", "权限设置", "分享笔记", false},
		{"无取消不能冒充", ">取消<", ">返回<", false},
		{"无确定不能冒充", ">确定<", ">下一步<", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html := permissionModalFixture(t, "公开可见")
			if tc.from != "" {
				html = strings.ReplaceAll(html, tc.from, tc.to)
			}
			html = strings.ReplaceAll(html, `id="modal" hidden`, `id="modal"`)
			page := creatorFixture(t, func(string) string { return html })
			require.NoError(t, page.Navigate(urlOfNoteManager))
			require.NoError(t, page.WaitLoad())
			modal, err := isNotePermissionModal(page.MustElement("#modal"))
			require.NoError(t, err)
			require.Equal(t, tc.want, modal)
			require.Empty(t, page.MustEval(`() => localStorage.events || ''`).String())
		})
	}
}
