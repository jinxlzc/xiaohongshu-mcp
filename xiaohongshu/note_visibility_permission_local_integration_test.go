//go:build integration

package xiaohongshu

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 只模拟脱敏操作条；按钮无文字、无 aria-label，hover 后延迟出现。
func noteIconFixture(actions string) string {
	return `<!doctype html><style>
.note-card {width:600px;height:120px;margin:20px}
.note-card__actions {display:none;width:180px;float:right}
.note-card__actions--visible {display:flex}
.note-card__action-btn {display:block;width:36px;height:32px;cursor:pointer}
svg {width:20px;height:20px}
[role=dialog], [role=alertdialog] {position:fixed;inset:180px 40px auto;background:white;padding:20px}
</style><div class="note-card" data-note-id="ffffffffffffffffffffffff"><button onclick="log('wrong-card')">权限设置</button></div>
<div id="target" class="note-card" data-note-id="` + fixtureNoteID + `">脱敏测试笔记<div class="note-card__actions">` + actions + `</div></div>
<div id="permission" class="d-popover" role="dialog" hidden><div class="d-options-wrapper"><label class="selected">公开</label><label>仅自己可见</label><label>仅互关好友可见</label></div></div>
<div id="more" role="menu" hidden><button onclick="this.parentNode.hidden=true;openPanel('permission')">权限设置</button><button onclick="log('deleted')">删除</button></div>
<div id="share" role="dialog" hidden>分享笔记<label hidden>公开</label><label hidden>仅自己可见</label><button onclick="log('close');this.parentNode.hidden=true">关闭</button></div>
<div id="delete" role="alertdialog" hidden>确定删除这篇笔记吗？<button onclick="log('cancel');this.parentNode.hidden=true">取消</button><button onclick="log('deleted')">确认删除</button></div>
<script>
function log(value){document.body.dataset.events=(document.body.dataset.events||'')+value+','}
function openPanel(id){log(id);document.getElementById(id).hidden=false}
document.getElementById('target').addEventListener('mouseenter',()=>setTimeout(()=>document.querySelector('#target .note-card__actions').classList.add('note-card__actions--visible'),250));
</script>`
}

func noteIcon(action, class string) string {
	return fmt.Sprintf(`<span class="note-card__action-btn %s" onclick="%s"><svg><circle cx="10" cy="10" r="8"/></svg></span>`, class, action)
}

func TestNotePermissionIconsLocal(t *testing.T) {
	for _, tc := range []struct {
		name, actions, events, wantErr string
	}{
		{"公开图标入口", noteIcon("openPanel('permission')", "") + strings.Repeat(noteIcon("log('unexpected')", ""), 3) + noteIcon("openPanel('delete')", "note-card__action-btn--del"), "permission,", ""},
		{"跳过首个删除图标", noteIcon("openPanel('delete')", "note-card__action-btn--del") + noteIcon("openPanel('permission')", ""), "permission,", ""},
		{"关闭错误面板再试", noteIcon("openPanel('share')", "") + noteIcon("if(document.getElementById('share').hidden)openPanel('permission');else log('not-closed')", ""), "share,close,permission,", ""},
		{"可见双选项不足以识别图标面板", noteIcon("document.querySelectorAll('#share label').forEach(el=>el.hidden=false);openPanel('share')", "") + noteIcon("openPanel('permission')", ""), "share,close,permission,", ""},
		{"兼容更多图标菜单", noteIcon("openPanel('more')", "more") + noteIcon("log('unexpected')", ""), "more,permission,", ""},
		{"文字入口兼容双选项", `<span class="note-card__action-btn" onclick="document.querySelector('#permission label:last-child').remove();openPanel('permission')">权限设置</span>`, "permission,", ""},
		{"已有菜单内打开权限面板", noteIcon("document.getElementById('more').classList.add('d-popover');document.querySelector('#more button').onclick=()=>{document.getElementById('more').append(document.getElementById('permission'));openPanel('permission')};openPanel('more')", "more"), "more,permission,", ""},
		{"意外删除确认必须取消", noteIcon("openPanel('delete')", "") + noteIcon("openPanel('permission')", ""), "delete,cancel,", "删除确认"},
		{"只有删除入口", noteIcon("openPanel('delete')", "note-card__action-btn--del"), "", "权限设置入口"},
		{"无法关闭时停止", noteIcon("document.querySelector('#share button').remove();openPanel('share')", "") + noteIcon("openPanel('permission')", ""), "share,", "关闭"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := creatorFixture(t, func(string) string { return noteIconFixture(tc.actions) }).Context(context.Background()).Timeout(40 * time.Second)
			defer page.CancelTimeout()
			require.NoError(t, page.Navigate(urlOfNoteManager))
			require.NoError(t, page.WaitLoad())
			card, err := findManagedNote(page, fixtureNoteID)
			require.NoError(t, err)
			panel, err := openNotePermission(page, card)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, panel)
			} else {
				require.NoError(t, err)
				value, err := selectedNoteVisibility(panel)
				require.NoError(t, err)
				require.Equal(t, "公开可见", value)
			}
			require.Equal(t, tc.events, page.MustEval(`() => document.body.dataset.events || ''`).String())
			require.False(t, page.MustElement("#delete").MustVisible())
		})
	}
}

func TestNoteVisibilityIconOverlaySkippedLocal(t *testing.T) {
	page := creatorFixture(t, func(path string) string {
		if path != "/new/note-manager" {
			return `<a href="/new/note-manager">笔记管理</a>`
		}
		html := noteIconFixture(noteIcon("openPanel('permission')", ""))
		return strings.Replace(html, "脱敏测试笔记", `脱敏测试笔记<div class="play_permission"><span class="permission_msg">仅自己可见</span></div>`, 1)
	})
	result, err := SetNoteVisibility(page.GetContext(), page, fixtureNoteID, "仅自己可见")
	require.NoError(t, err)
	require.Equal(t, "skipped", result.Status)
	require.Equal(t, "仅自己可见", result.Before)
	require.Equal(t, result.Before, result.After)
	require.Empty(t, page.MustEval(`() => document.body.dataset.events || ''`).String())
}

func TestNotePermissionDelayedVerificationLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := creatorFixture(t, func(string) string {
		html := noteIconFixture(noteIcon("setTimeout(()=>openPanel('scan'),1500);setTimeout(()=>{document.getElementById('scan').remove();openPanel('permission')},4000)", ""))
		return html + `<div id="scan" role="dialog" hidden><h3>扫码验证</h3><canvas width="200" height="200"></canvas><button onclick="log('scan-canceled');this.parentNode.hidden=true">取消</button></div>`
	}).Context(context.Background()).Timeout(60 * time.Second)
	defer page.CancelTimeout()
	require.NoError(t, page.Navigate(urlOfNoteManager))
	require.NoError(t, page.WaitLoad())
	panel, err := openNotePermission(page, page.MustElement("#target"))
	require.NoError(t, err)
	value, err := selectedNoteVisibility(panel)
	require.NoError(t, err)
	require.Equal(t, "公开可见", value)
	require.Equal(t, "scan,permission,", page.MustEval(`() => document.body.dataset.events || ''`).String())
	require.FileExists(t, "verify_qr.png")
}
