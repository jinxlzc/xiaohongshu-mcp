//go:build integration

package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
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
	return creatorHTTPFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html(r.URL.Path)))
	}))
}

func creatorHTTPFixture(t *testing.T, handler http.Handler) *rod.Page {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	b := newSearchFixtureBrowser(t, func(client rod.CDPClient) rod.CDPClient {
		return &localCreatorClient{CDPClient: client, origin: server.URL}
	})
	page := b.MustPage().Timeout(12 * time.Second)
	t.Cleanup(func() { _ = page.Close() })
	return page
}
