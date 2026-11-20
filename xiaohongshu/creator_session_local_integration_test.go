//go:build integration

package xiaohongshu

import (
	"context"
	"encoding/base64"

	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
			state, err := CheckCreatorLoginStatus(page.GetContext(), page)
			require.NoError(t, err)
			require.Equal(t, "agreement_required", state.Status)
			require.False(t, state.IsLoggedIn)
			require.True(t, page.MustElement("[role=dialog]").MustVisible())
			require.False(t, page.MustElement("input").MustProperty("checked").Bool())
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
	state, err := CheckCreatorLoginStatus(page.GetContext(), page)
	require.NoError(t, err)
	require.Equal(t, "login_required", state.Status)
	require.False(t, state.IsLoggedIn)
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
