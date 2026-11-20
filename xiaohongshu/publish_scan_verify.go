package xiaohongshu

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// PublishTimeout 覆盖填写、上传、本人扫码及成功确认，仅用于发布接口。
const PublishTimeout = 12 * time.Minute
const publishScanTimeout = 5 * time.Minute

type publishScan struct {
	Text             string `json:"text"`
	RefreshAvailable bool   `json:"refresh_available"`
	capture          func(context.Context) ([]byte, error)
	refresh          func(context.Context) error
}

type publishQRMetadata struct {
	WrittenAt time.Time `json:"written_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

var scanDuration = regexp.MustCompile(`(\d+)\s*(分钟|分|秒)`)
var scanClock = regexp.MustCompile(`(?:剩余|有效期|倒计时)\s*[:：]?\s*(\d{1,2}):(\d{2})`)

func publishScanLifetime(text string) time.Duration {
	if strings.Contains(text, "已过期") || strings.Contains(text, "已失效") {
		return 0
	}
	if m := scanClock.FindStringSubmatch(text); len(m) > 0 {
		minutes, _ := strconv.Atoi(m[1])
		seconds, _ := strconv.Atoi(m[2])
		return time.Duration(minutes*60+seconds) * time.Second
	}
	if m := scanDuration.FindStringSubmatch(text); len(m) > 0 {
		n, _ := strconv.Atoi(m[1])
		unit := time.Second
		if m[2] != "秒" {
			unit = time.Minute
		}
		return time.Duration(n) * unit
	}
	return time.Minute
}

func isPublishScanText(text string) bool {
	text = strings.Join(strings.Fields(text), "")
	return strings.Contains(text, "扫码验证") || strings.Contains(text, "二维码1分钟失效") ||
		(strings.Contains(strings.ToLower(text), "小红书app") && strings.Contains(text, "扫码"))
}

// 先写同目录临时文件再替换，watch 不会读到半张图片，也不会沿旧路径的符号链接写入。
func writePublishQRFile(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".verify-qr-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

func savePublishQR(png []byte, now time.Time, lifetime time.Duration) error {
	metadata, err := json.MarshalIndent(publishQRMetadata{now, now.Add(lifetime)}, "", "  ")
	if err != nil {
		return err
	}
	if err = writePublishQRFile("verify_qr.png", png); err != nil {
		return err
	}
	return writePublishQRFile("verify_qr.json", metadata)
}

type publishScanWait struct {
	started        time.Time
	expires        time.Time
	hash           [32]byte
	saved          bool
	refreshing     time.Time
	refreshVisible bool
}

func (w *publishScanWait) update(ctx context.Context, scan *publishScan) error {
	now := time.Now()
	if w.started.IsZero() {
		w.started = now
	}
	if now.Sub(w.started) >= publishScanTimeout {
		return fmt.Errorf("需要账号本人扫码验证：等待超过5分钟")
	}
	if !w.refreshing.IsZero() && now.Sub(w.refreshing) >= 5*time.Second {
		return fmt.Errorf("需要账号本人扫码验证：刷新后二维码未更新")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var png []byte
	var err error
	if scan.capture != nil {
		png, err = scan.capture(ctx)
	}
	if err != nil || len(png) == 0 {
		// 异步刷新会短暂移除二维码；只容忍有限重绘时间。
		if !w.refreshing.IsZero() {
			return nil
		}
		if scan.RefreshAvailable {
			return w.refresh(ctx, scan)
		}
		if now.Sub(w.started) < 5*time.Second {
			return nil
		}
		return fmt.Errorf("需要账号本人扫码验证：未能截取二维码")
	}
	if !w.refreshing.IsZero() && publishScanLifetime(scan.Text) == 0 {
		return nil
	}
	hash := sha256.Sum256(png)
	if !w.saved || hash != w.hash {
		now = time.Now()
		lifetime := publishScanLifetime(scan.Text)
		if err := savePublishQR(png, now, lifetime); err != nil {
			return fmt.Errorf("需要账号本人扫码验证：二维码落盘失败: %w", err)
		}
		if w.saved {
			slog.Info("已刷新二维码文件")
		} else {
			slog.Info("已写入二维码文件")
		}
		w.saved = true
		w.hash = hash
		w.expires = now.Add(lifetime)
		w.refreshing = time.Time{}
	}
	if !w.refreshing.IsZero() {
		return nil
	}
	refreshAppeared := scan.RefreshAvailable && !w.refreshVisible
	w.refreshVisible = scan.RefreshAvailable
	if !now.Before(w.expires) || publishScanLifetime(scan.Text) == 0 || refreshAppeared {
		return w.refresh(ctx, scan)
	}
	return nil
}

func (w *publishScanWait) refresh(ctx context.Context, scan *publishScan) error {
	if scan.refresh == nil {
		return fmt.Errorf("需要账号本人扫码验证：二维码已过期，未找到刷新控件")
	}
	if err := scan.refresh(ctx); err != nil {
		return fmt.Errorf("需要账号本人扫码验证：无法刷新二维码: %w", err)
	}
	w.refreshing = time.Now()
	w.refreshVisible = scan.RefreshAvailable
	return nil
}

// 只从弹窗内选择足够大的方形图像；排除现场截图中的小头像。
func publishScanQR(container *rod.Element) (*rod.Element, error) {
	elements, err := container.Elements(`canvas, img, svg, [class*="qrcode"], [class*="qr-code"]`)
	if err != nil {
		return nil, err
	}
	for _, el := range elements {
		visible, err := el.Visible()
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		shape, err := el.Shape()
		if err != nil {
			return nil, err
		}
		box := shape.Box()
		if box == nil {
			continue
		}
		if box.Width >= 100 && box.Width <= 400 && box.Height >= 100 && box.Height <= 400 && box.Width/box.Height > 0.8 && box.Width/box.Height < 1.25 {
			return el, nil
		}
	}
	return nil, nil
}

func publishRefreshLabel(el *rod.Element) (bool, error) {
	text, err := el.Text()
	if err != nil {
		return false, err
	}
	labels := []string{text}
	for _, attr := range []string{"aria-label", "title"} {
		v, err := el.Attribute(attr)
		if err != nil {
			return false, err
		}
		if v != nil {
			labels = append(labels, *v)
		}
	}
	for _, label := range labels {
		for _, deny := range []string{"取消", "关闭", "×", "问题反馈", "关联"} {
			if strings.Contains(label, deny) {
				return false, nil
			}
		}
	}
	for _, label := range labels {
		switch strings.TrimSpace(label) {
		case "刷新", "刷新二维码", "点击刷新", "点击刷新二维码", "二维码已过期，点击刷新", "二维码已失效，点击刷新":
			return true, nil
		}
	}
	return false, nil
}

func publishScanRefresh(container *rod.Element) (*rod.Element, error) {
	candidates, err := container.Elements(`button, a, [role="button"], [aria-label], [title], [class*="refresh"], div, span`)
	if err != nil {
		return nil, err
	}
	// 从内层开始，避免父容器同时含有其它操作。
	for i := len(candidates) - 1; i >= 0; i-- {
		el := candidates[i]
		v, err := el.Visible()
		if err != nil {
			return nil, err
		}
		if !v {
			continue
		}
		ok, err := publishRefreshLabel(el)
		if err != nil {
			return nil, err
		}
		if ok {
			// 内层文字不能绕过实际按钮的语义或 disabled 状态。
			parents, err := el.Parents(`button, a, [role="button"]`)
			if err != nil {
				return nil, err
			}
			safe := true
			for _, parent := range parents {
				allowed, err := publishRefreshLabel(parent)
				if err != nil {
					return nil, err
				}
				if !allowed {
					safe = false
					break
				}
				el = parent
			}
			if safe {
				return el, nil
			}
		}
	}
	return nil, nil
}

// 使用可见文案定位独立验证层，兼容没有 role/class 标记的第三方弹窗。
func observePublishScan(page *rod.Page) (*publishScan, error) {
	anchors, err := page.ElementsX(`//*[not(self::script or self::style) and (contains(text(),'扫码验证') or contains(text(),'二维码') or contains(text(),'小红书APP') or contains(text(),'小红书App'))]`)
	if err != nil {
		return nil, err
	}
	var fallback *publishScan
	for _, anchor := range anchors {
		editors, err := anchor.ElementsX(`ancestor-or-self::*[@contenteditable="true" or self::textarea or self::input]`)
		if err != nil {
			return nil, err
		}
		if len(editors) > 0 {
			continue
		}
		visible, err := anchor.Visible()
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		var container *rod.Element
		for el, depth := anchor, 0; el != nil && depth < 8; depth++ {
			tag, err := el.Property("tagName")
			if err != nil {
				return nil, err
			}
			if strings.EqualFold(tag.Str(), "body") || strings.EqualFold(tag.Str(), "html") {
				break
			}
			// 边界最多扩到独立验证层，不能越过背后的发布表单寻找刷新按钮。
			forms, err := el.Elements(`input:not([type="hidden"]), textarea, [contenteditable="true"], xhs-publish-btn, .publish-page-publish-btn`)
			if err != nil {
				return nil, err
			}
			if len(forms) > 0 {
				break
			}
			text, err := el.Text()
			if err != nil {
				return nil, err
			}
			if isPublishScanText(text) {
				container = el
			}
			el, err = el.Parent()
			if err != nil {
				return nil, err
			}
		}
		if container == nil {
			continue
		}
		scan, err := publishScanInContainer(container)
		if err != nil {
			return nil, err
		}
		if scan.capture != nil {
			return scan, nil
		}
		fallback = scan
	}

	return fallback, nil
}

func publishScanInContainer(container *rod.Element) (*publishScan, error) {
	text, err := container.Text()
	if err != nil {
		return nil, err
	}
	scan := &publishScan{Text: redactPublishText(text)}
	qr, err := publishScanQR(container)
	if err != nil {
		return nil, err
	}
	if qr != nil {
		scan.capture = func(ctx context.Context) ([]byte, error) {
			return qr.Context(ctx).Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
		}
	}
	refresh, err := publishScanRefresh(container)
	if err != nil {
		return nil, err
	}
	scan.RefreshAvailable = refresh != nil
	// 每次点击前重新查找、校验，只允许刷新；不借用发布确认逻辑。
	scan.refresh = func(ctx context.Context) error {
		button, err := publishScanRefresh(container.Context(ctx))
		if err != nil {
			return err
		}
		if button == nil {
			return fmt.Errorf("没有明确的二维码刷新控件")
		}
		disabled, err := publishElementDisabled(button)
		if err != nil {
			return err
		}
		if disabled {
			return fmt.Errorf("二维码刷新控件不可用")
		}
		return button.Click(proto.InputMouseButtonLeft, 1)
	}
	return scan, nil
}
