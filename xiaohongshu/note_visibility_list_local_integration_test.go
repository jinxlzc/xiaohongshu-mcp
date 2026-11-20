//go:build integration

package xiaohongshu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const fixtureNoteID = "0123456789abcdef01234567"

func TestManagedNoteIDLocal(t *testing.T) {
	for _, tc := range []struct{ name, markup, want string }{
		{"埋点卡片无链接", `<div class="note-card" data-impression='{"noteTarget":{"value":{"noteId":"` + fixtureNoteID + `"}}}'></div>`, fixtureNoteID},
		{"无效埋点回退链接", `<div class="note-card" data-impression='{bad'><a href="/explore/` + fixtureNoteID + `">测试</a></div>`, fixtureNoteID},
		{"兼容直接属性", `<div class="note-card" data-note-id="` + fixtureNoteID + `"></div>`, fixtureNoteID},
		{"兼容详情路径", `<div class="note-card"><a href="/discovery/item/` + fixtureNoteID + `">测试</a></div>`, fixtureNoteID},
		{"拒绝外站链接", `<div class="note-card"><a href="//example.invalid/explore/` + fixtureNoteID + `">测试</a></div>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := creatorFixture(t, func(string) string { return tc.markup })
			require.NoError(t, page.Navigate(urlOfNoteManager))
			id, err := managedNoteID(page.MustElement(".note-card"))
			require.NoError(t, err)
			require.Equal(t, tc.want, id)
		})
	}
}

// 模拟固定高度容器内分批加载，窗口本身不能滚动。
const managedScrollFixture = `<!doctype html><style>
body {margin:0;overflow:hidden} .content {height:240px;width:600px;overflow:auto}
#cards {overflow:auto} .note-card {height:100px} #loading {height:30px}
</style><div class="content"><div id="cards"></div><div id="loading">正在加载中...</div></div>
<script>
const content=document.querySelector('.content'), cards=document.querySelector('#cards');
let batch=0, pending=false;
function appendBatch(){
  for(let i=0;i<5;i++){
    const card=document.createElement('div');card.className='note-card';
    card.textContent='脱敏测试笔记 '+(batch*5+i);
    const id=batch===3 && i===4 ? '0123456789abcdef01234567' : (batch*5+i+1).toString(16).padStart(24,'0');
    card.dataset.impression=JSON.stringify({noteTarget:{type:'NoteTarget',value:{noteId:id}}});
    cards.append(card);
  }
  batch++;pending=false;document.body.dataset.batches=batch;
  if(batch===4)document.querySelector('#loading').remove();
}
appendBatch();
content.addEventListener('scroll',()=>{
  if(batch<4 && !pending && content.scrollTop+content.clientHeight>=content.scrollHeight-5){
    pending=true;setTimeout(appendBatch,1700);
  }
});
</script>`

func TestManagedNoteInfiniteScrollLocal(t *testing.T) {
	for _, class := range []string{"content", "list-scroll"} {
		t.Run(class, func(t *testing.T) {
			page := creatorFixture(t, func(string) string {
				return strings.ReplaceAll(strings.ReplaceAll(managedScrollFixture, ".content", "."+class), `class="content"`, `class="`+class+`"`)
			})
			require.NoError(t, page.Navigate(urlOfNoteManager))
			card, err := findManagedNote(page, fixtureNoteID)
			require.NoError(t, err)
			id, err := managedNoteID(card)
			require.NoError(t, err)
			require.Equal(t, fixtureNoteID, id)
			require.Equal(t, "4", *page.MustElement("body").MustAttribute("data-batches"))
			require.Greater(t, page.MustElement("."+class).MustProperty("scrollTop").Num(), float64(0))
			require.Zero(t, page.MustEval(`() => window.scrollY`).Num())
			require.Len(t, page.MustElements(".note-card"), 20)
		})
	}
}

func TestManagedNoteListCompletionLocal(t *testing.T) {
	for _, tc := range []struct {
		name, markup, want string
		timeout            time.Duration
	}{
		{"延迟补全ID", `<div class="note-card">测试笔记</div><script>setTimeout(()=>document.querySelector('.note-card').dataset.impression=JSON.stringify({noteTarget:{value:{noteId:'` + fixtureNoteID + `'}}}),2000)</script>`, "", 5 * time.Second},
		{"加载完成但无目标", `<div class="note-card" data-note-id="ffffffffffffffffffffffff">其他测试笔记</div>`, "未找到笔记", 5 * time.Second},
		{"加载不结束", `<div class="content"><div class="note-card">测试笔记</div><div>正在加载中...</div></div>`, "定位笔记超时", time.Second},
		{"空ID不宣告完成", `<div class="note-card">测试笔记</div>`, "定位笔记超时", 2200 * time.Millisecond},
		{"空列表", `<div class="content"></div>`, "未找到笔记", 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := creatorFixture(t, func(string) string { return tc.markup })
			require.NoError(t, page.Navigate(urlOfNoteManager))
			pp := page.Timeout(tc.timeout)
			defer pp.CancelTimeout()
			card, err := findManagedNote(pp, fixtureNoteID)
			if tc.want == "" {
				require.NoError(t, err)
				require.NotNil(t, card)
			} else {
				require.ErrorContains(t, err, tc.want)
				require.Nil(t, card)
				if tc.want == "定位笔记超时" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			}
		})
	}
}

func TestNotePermissionHoverMenuLocal(t *testing.T) {
	page := creatorFixture(t, func(string) string {
		return `<style>.more {display:none}.note-card:hover .more{display:block}</style>
<div role="menu"><button onclick="document.body.dataset.wrong='yes'">权限设置</button></div>
<div class="note-card" style="height:100px" data-impression='{"noteTarget":{"value":{"noteId":"` + fixtureNoteID + `"}}}'>
<div class="play_permission"><span class="play_permission_inner"><span class="permission_msg" onclick="document.body.dataset.wrong='yes'">仅自己可见</span></span></div>
<button class="more" aria-label="更多操作" onclick="document.querySelector('#menu').style.display='block'">...</button></div>
<div id="menu" role="menu" style="display:none"><div class="d-popover-content"><button onclick="document.querySelector('#panel').style.display='block';document.querySelector('#menu').style.display='none'">权限设置</button></div></div>
<div id="panel" role="dialog" style="display:none"><label aria-checked="true">仅自己可见</label><label>公开可见</label><button onclick="document.body.dataset.saved='yes'">保存</button></div>`
	})
	require.NoError(t, page.Navigate(urlOfNoteManager))
	card, err := findManagedNote(page, fixtureNoteID)
	require.NoError(t, err)
	writes := 0
	result, err := changeNoteVisibility(page.GetContext(), fixtureNoteID, "仅自己可见", func() (string, error) {
		panel, err := openNotePermission(page, card)
		if err != nil {
			return "", err
		}
		return selectedNoteVisibility(panel)
	}, func() error { writes++; return errors.New("不应写入") })
	require.NoError(t, err)
	require.Equal(t, "skipped", result.Status)
	require.Zero(t, writes)
	require.Nil(t, page.MustElement("body").MustAttribute("data-wrong"))
	require.Nil(t, page.MustElement("body").MustAttribute("data-saved"))
}

func TestNoteVisibilityVerificationLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			page := creatorFixture(t, func(string) string {
				html := `<section id="scan"><h3>扫码验证</h3><canvas width="200" height="200"></canvas><button onclick="document.body.dataset.wrong='yes'">取消</button></section>`
				if completed {
					html += `<script>setTimeout(()=>document.querySelector('#scan').remove(),1200)</script>`
				}
				return html
			})
			require.NoError(t, page.Navigate(urlOfNoteManager))
			require.NoError(t, page.WaitLoad())
			pp := page.Timeout(3 * time.Second)
			defer pp.CancelTimeout()
			err := waitNoteVerification(pp)
			if completed {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "需要账号本人扫码验证")
			}
			data, err := os.ReadFile("verify_qr.png")
			require.NoError(t, err)
			require.NotEmpty(t, data)
			require.FileExists(t, "verify_qr.json")
			require.Nil(t, page.MustElement("body").MustAttribute("data-wrong"))
		})
	}
}

func TestNoteVisibilityOverlaySkippedLocal(t *testing.T) {
	page := creatorFixture(t, func(path string) string {
		if path != "/new/note-manager" {
			return `<a href="/new/note-manager">笔记管理</a>`
		}
		return `<div class="note-card" data-impression='{"noteTarget":{"value":{"noteId":"` + fixtureNoteID + `"}}}'>
<div class="play_permission"><span class="play_permission_inner"><span class="permission_msg">仅自己可见</span></span></div>
<button onclick="document.body.dataset.clicked='yes'">权限设置</button></div>`
	})
	result, err := SetNoteVisibility(page.GetContext(), page, fixtureNoteID, "仅自己可见")
	require.NoError(t, err)
	require.Equal(t, "skipped", result.Status)
	require.Equal(t, "仅自己可见", result.Before)
	require.Equal(t, result.Before, result.After)
	require.Nil(t, page.MustElement("body").MustAttribute("data-clicked"))
}

func TestNoteVisibilityIgnoresCardScanTextLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	page := creatorFixture(t, func(string) string {
		return `<div class="note-card">扫码验证教程<h3>扫码验证教程</h3><canvas width="200" height="200"></canvas></div>`
	})
	require.NoError(t, page.Navigate(urlOfNoteManager))
	require.NoError(t, page.WaitLoad())
	require.NoError(t, waitNoteVerification(page))
	require.NoFileExists(t, "verify_qr.png")
	require.NoFileExists(t, "verify_qr.json")
}
