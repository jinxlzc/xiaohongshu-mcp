//go:build integration

package xiaohongshu

import (
	"context"
	_ "embed"
	"net/http"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/creator_dashboard.html
var creatorDashboardHTML string

func creatorDashboardFixture(t *testing.T, path, html string) *rod.Page {
	t.Helper()
	return creatorHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && path != "/" {
			http.Redirect(w, r, path, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
}

func TestCreatorDashboardLocal(t *testing.T) {
	for _, path := range []string{"/new/home", "/new/note-manager", "/publish/publish"} {
		t.Run(path, func(t *testing.T) {
			page := creatorDashboardFixture(t, path, creatorDashboardHTML)
			// 远小于取码的 30 秒超时，保证工作台直接返回已登录。
			ctx, cancel := context.WithTimeout(page.GetContext(), 5*time.Second)
			defer cancel()
			state, err := CheckCreatorLoginStatus(ctx, page)
			require.NoError(t, err)
			require.Equal(t, CreatorLoginStatus{IsLoggedIn: true, Status: "logged_in"}, state)
			require.Empty(t, page.MustElements(creatorReady))
			require.Empty(t, page.MustElements(`.login-container, .login-box, `+creatorQR))

			img, loggedIn, err := FetchCreatorQrcodeImage(ctx, page)
			require.NoError(t, err)
			require.True(t, loggedIn)
			require.Empty(t, img)
			require.NoError(t, ctx.Err())
		})
	}
}

func TestCreatorDashboardSignalsLocal(t *testing.T) {
	for _, tc := range []struct {
		name, path, html, status string
	}{
		{"只有标题和路径", "/new/home", `<title>小红书创作服务平台</title>`, "unknown"},
		{"缺少账号", "/new/home", `<div class="d-menu-item">笔记管理</div>`, "unknown"},
		{"缺少菜单", "/new/home", `<div class="account-name">测试账号</div>`, "unknown"},
		{"隐藏账号", "/new/home", `<div class="account-name" style="display:none">测试账号</div><div class="d-menu-item">首页</div>`, "unknown"},
		{"空账号", "/new/home", `<div class="account-name"> </div><div class="d-menu-item">首页</div>`, "unknown"},
		{"隐藏菜单", "/new/home", `<div class="account-name">测试账号</div><div class="d-menu-item" style="display:none">笔记管理</div>`, "unknown"},
		{"菜单文字不匹配", "/new/home", `<div class="account-name">测试账号</div><div class="d-menu-item">返回首页登录</div>`, "unknown"},
		{"未认证路径", "/", creatorDashboardHTML, "unknown"},
		{"路径前缀不匹配", "/new/home-public", creatorDashboardHTML, "unknown"},
		{"登录路径优先", "/login", creatorDashboardHTML, "login_required"},
		{"登录表单优先", "/new/home", creatorDashboardHTML + `<div class="login-container">扫码登录</div>`, "login_required"},
		{"协议优先", "/new/home", creatorDashboardHTML + `<div role="dialog">创作者服务协议<button onclick="this.parentNode.remove()">同意</button></div>`, "agreement_required"},
		{"仅首页菜单", "/new/home", `<div class="account-name">测试账号</div><div class="d-menu-item"> 首页 </div>`, "logged_in"},
		{"仅笔记管理菜单", "/new/home", `<div class="account-name">测试账号</div><div class="d-menu-item"> 笔记管理 </div>`, "logged_in"},
		{"保留旧版布局", "/", `<a href="/new/note-manager">笔记管理</a>`, "logged_in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := creatorDashboardFixture(t, tc.path, tc.html)
			require.NoError(t, page.Navigate(urlOfCreatorHome))
			require.NoError(t, page.WaitLoad())
			state, err := creatorPageStatus(page)
			require.NoError(t, err)
			require.Equal(t, tc.status, state.Status)
			require.Equal(t, tc.status == "logged_in", state.IsLoggedIn)
			if tc.status == "agreement_required" {
				state, err = CheckCreatorLoginStatus(page.GetContext(), page)
				require.NoError(t, err)
				require.Equal(t, tc.status, state.Status)
				require.True(t, page.MustElement(`[role="dialog"]`).MustVisible())
			}
		})
	}
}
