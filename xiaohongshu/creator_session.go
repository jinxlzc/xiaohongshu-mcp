package xiaohongshu

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

const (
	urlOfCreatorHome      = "https://creator.xiaohongshu.com/"
	creatorSessionTimeout = 20 * time.Second
	creatorDialogs        = `[role="dialog"], .d-modal, .d-dialog, .el-dialog, .agreement-dialog, .agreement-modal`
	creatorReady          = `a[href*="/publish/publish"], a[href*="/new/note-manager"], .creator-header .user-info`
	creatorQR             = `.login-container .qrcode img, .login-container img.qrcode-img, .qrcode-container img, .qr-code img, .qrcode canvas, .qrcode-container canvas`
)

var ErrCreatorLoginRequired = errors.New("创作者中心需要单独扫码，请调用 /api/v1/login/creator/qrcode 或 get_creator_login_qrcode")

// CreatorLoginStatus 区分未登录、协议阻挡和无法判断，避免把页面故障误报成退出登录。
type CreatorLoginStatus struct {
	IsLoggedIn bool   `json:"is_logged_in"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

func ensureCreatorSession(page *rod.Page) error {
	return EnsureCreatorSession(page.GetContext(), page)
}

// EnsureCreatorSession 在主站会话换取创作者会话时处理必要协议；不选择任何复选框。
func EnsureCreatorSession(ctx context.Context, page *rod.Page) error {
	pp := page.Context(ctx).Timeout(creatorSessionTimeout)
	defer pp.CancelTimeout()
	if err := pp.Navigate(urlOfCreatorHome); err != nil {
		return fmt.Errorf("打开创作者中心: %w", err)
	}
	for {
		if _, err := acceptCreatorAgreement(pp); err != nil {
			return err
		}
		state, err := creatorPageStatus(pp)
		if err != nil {
			return err
		}
		if state.IsLoggedIn {
			return nil
		}
		if err := waitCreatorPoll(pp.GetContext()); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if state.Status == "login_required" {
				return ErrCreatorLoginRequired
			}
			return fmt.Errorf("创作者中心会话未就绪（%s），请检查页面路径、协议弹窗或调用 /api/v1/login/creator/qrcode", state.Status)
		}
	}
}

func waitCreatorPoll(ctx context.Context) error {
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func visibleElement(page *rod.Page, selector string) (*rod.Element, error) {
	elems, err := page.Elements(selector)
	if err != nil {
		return nil, err
	}
	for _, el := range elems {
		visible, err := el.Visible()
		if err != nil {
			return nil, err
		}
		if visible {
			return el, nil
		}
	}
	return nil, nil
}

func safeCreatorAgreement(body, button string) bool {
	if button != "同意" && button != "同意并继续" && button != "我知道了" && button != "知道了" {
		return false
	}
	if !strings.Contains(body, "协议") && !strings.Contains(body, "条款") && !strings.Contains(body, "隐私政策") {
		return false
	}
	for _, word := range []string{"营销", "付费", "支付", "购买", "订阅", "广告推送", "推广信息", "会员"} {
		if strings.Contains(body, word) {
			return false
		}
	}
	return true
}

func acceptCreatorAgreement(page *rod.Page) (bool, error) {
	info, err := page.Info()
	if err != nil {
		return false, err
	}
	u, err := url.Parse(info.URL)
	if err != nil {
		return false, err
	}
	if u.Hostname() != "creator.xiaohongshu.com" {
		return false, nil
	}
	dialogs, err := page.Elements(creatorDialogs)
	if err != nil {
		return false, err
	}
	for _, dialog := range dialogs {
		visible, err := dialog.Visible()
		if err != nil {
			return false, err
		}
		if !visible {
			continue
		}
		body, err := dialog.Text()
		if err != nil {
			return false, err
		}
		buttons, err := dialog.Elements(`button, [role="button"], .d-button`)
		if err != nil {
			return false, err
		}
		for _, button := range buttons {
			text, err := button.Text()
			if err != nil {
				return false, err
			}
			if !safeCreatorAgreement(body, strings.TrimSpace(text)) {
				continue
			}
			visible, err := button.Visible()
			if err != nil {
				return false, err
			}
			if !visible {
				continue
			}
			if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return false, fmt.Errorf("确认创作者协议: %w", err)
			}
			return true, nil
		}
	}
	return false, nil
}

func creatorPageStatus(page *rod.Page) (CreatorLoginStatus, error) {
	unknown := CreatorLoginStatus{Status: "unknown"}
	info, err := page.Info()
	if err != nil {
		return unknown, err
	}
	u, err := url.Parse(info.URL)
	if err != nil {
		return unknown, err
	}
	if u.Hostname() != "creator.xiaohongshu.com" {
		return unknown, nil
	}
	dialogs, err := page.Elements(creatorDialogs)
	if err != nil {
		return unknown, err
	}
	for _, dialog := range dialogs {
		visible, err := dialog.Visible()
		if err != nil {
			return unknown, err
		}
		if !visible {
			continue
		}
		text, err := dialog.Text()
		if err != nil {
			return unknown, err
		}
		if strings.Contains(text, "协议") || strings.Contains(text, "条款") || strings.Contains(text, "隐私政策") {
			return CreatorLoginStatus{Status: "agreement_required", Message: "请在创作者中心确认协议；状态检查不会自动同意"}, nil
		}
	}
	login, err := visibleElement(page, `.login-container, .login-box, `+creatorQR)
	if err != nil {
		return unknown, err
	}
	if strings.Contains(u.Path, "/login") || login != nil {
		return CreatorLoginStatus{Status: "login_required"}, nil
	}
	ready, err := visibleElement(page, creatorReady)
	if err != nil {
		return unknown, err
	}
	if ready != nil {
		return CreatorLoginStatus{IsLoggedIn: true, Status: "logged_in"}, nil
	}
	dashboard, err := creatorDashboardReady(page, u.Path)
	if err != nil {
		return unknown, err
	}
	if dashboard {
		return CreatorLoginStatus{IsLoggedIn: true, Status: "logged_in"}, nil
	}
	return unknown, nil
}

// 新版工作台用 div 菜单；结合认证后路径、可见账号和菜单，避免仅凭标题或 URL 误判。
func creatorDashboardReady(page *rod.Page, path string) (bool, error) {
	switch strings.TrimSuffix(path, "/") {
	case "/new/home", "/new/note-manager", "/publish/publish":
	default:
		return false, nil
	}
	account, err := visibleElement(page, ".account-name")
	if err != nil || account == nil {
		return false, err
	}
	name, err := account.Text()
	if err != nil || strings.TrimSpace(name) == "" {
		return false, err
	}
	menus, err := page.Elements("div.d-menu-item")
	if err != nil {
		return false, err
	}
	for _, menu := range menus {
		visible, err := menu.Visible()
		if err != nil {
			return false, err
		}
		if !visible {
			continue
		}
		text, err := menu.Text()
		if err != nil {
			return false, err
		}
		switch strings.TrimSpace(text) {
		case "首页", "笔记管理":
			return true, nil
		}
	}
	return false, nil
}

// CheckCreatorLoginStatus 只检查页面，不点击协议，不以 cookie 名称判断登录有效。
func CheckCreatorLoginStatus(ctx context.Context, page *rod.Page) (CreatorLoginStatus, error) {
	pp := page.Context(ctx).Timeout(creatorSessionTimeout)
	defer pp.CancelTimeout()
	state := CreatorLoginStatus{Status: "unknown"}
	if err := pp.Navigate(urlOfCreatorHome); err != nil {
		return state, err
	}
	for {
		var err error
		state, err = creatorPageStatus(pp)
		if err != nil {
			return state, err
		}
		if state.Status != "unknown" {
			return state, nil
		}
		if err := waitCreatorPoll(pp.GetContext()); err != nil {
			return state, err
		}
	}
}

// FetchCreatorQrcodeImage 截取页面二维码，兼容 img、远程图片和 canvas。
func FetchCreatorQrcodeImage(ctx context.Context, page *rod.Page) (string, bool, error) {
	pp := page.Context(ctx).Timeout(30 * time.Second)
	defer pp.CancelTimeout()
	if err := pp.Navigate(urlOfCreatorHome); err != nil {
		return "", false, err
	}
	for {
		if _, err := acceptCreatorAgreement(pp); err != nil {
			return "", false, err
		}
		state, err := creatorPageStatus(pp)
		if err != nil {
			return "", false, err
		}
		if state.IsLoggedIn {
			return "", true, nil
		}
		qr, err := visibleElement(pp, creatorQR)
		if err != nil {
			return "", false, err
		}
		if qr != nil {
			data, err := qr.Screenshot(proto.PageCaptureScreenshotFormatPng, 100)
			if err != nil {
				return "", false, fmt.Errorf("读取创作者二维码: %w", err)
			}
			if qrcodeHasPixels(data) {
				return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), false, nil
			}
		}
		// 仅切换到扫码入口，不选择手机号等其他登录方式。
		tabs, err := pp.Elements(`button, [role="tab"], .login-tab, .tab-item`)
		if err != nil {
			return "", false, err
		}
		for _, tab := range tabs {
			text, err := tab.Text()
			if err != nil {
				return "", false, err
			}
			if strings.TrimSpace(text) == "扫码登录" {
				visible, _ := tab.Visible()
				if visible {
					if err := tab.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return "", false, err
					}
					break
				}
			}
		}
		if err := waitCreatorPoll(pp.GetContext()); err != nil {
			return "", false, fmt.Errorf("未找到创作者登录二维码，请核对页面路径与选择器: %w", err)
		}
	}
}

func WaitForCreatorLogin(ctx context.Context, page *rod.Page) bool {
	pp := page.Context(ctx)
	for {
		_, err := acceptCreatorAgreement(pp)
		if err == nil {
			var status CreatorLoginStatus
			status, err = creatorPageStatus(pp)
			if err == nil && status.IsLoggedIn {
				return true
			}
		}
		if err != nil && !creatorNavigationTransient(err) {
			return false
		}
		if waitCreatorPoll(ctx) != nil {
			return false
		}
	}
}

// 登录跳转会让上一页元素失效，允许下一轮重新获取。
func creatorNavigationTransient(err error) bool {
	if errors.Is(err, &rod.ObjectNotFoundError{}) || errors.Is(err, &rod.ElementNotFoundError{}) {
		return true
	}
	var protocolErr *cdp.Error
	if errors.As(err, &protocolErr) {
		return protocolErr.Code == -32000 && (strings.Contains(protocolErr.Message, "context") || strings.Contains(protocolErr.Message, "object"))
	}
	return false
}

// qrcodeHasPixels 避免把尚未绘制的空白二维码返回给用户。
func qrcodeHasPixels(data []byte) bool {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	bounds := img.Bounds()
	dark, light := 0, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a < 32768 {
				continue
			}
			brightness := (r + g + b) / 3
			if brightness < 16384 {
				dark++
			}
			if brightness > 49152 {
				light++
			}
		}
	}
	threshold := bounds.Dx() * bounds.Dy() / 100
	return dark > threshold && light > threshold
}
