package xiaohongshu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const publishDialogs = creatorDialogs + `, [aria-modal="true"], .ant-modal-content, .verify-modal, .verify-dialog, .captcha-modal, .captcha-dialog`
const publishHints = `[role="alert"], [role="status"], .d-toast, .d-message, .toast, .el-message, .ant-message, .error-message, .error-msg, .length-error, .max_suffix, .validation-message, [aria-invalid="true"], [class*="error"], [class*="invalid"]`

type publishButtonState struct {
	Version    string            `json:"version"`
	Visible    bool              `json:"visible"`
	Text       string            `json:"text"`
	Attributes map[string]string `json:"attributes"`
}

type publishDialog struct {
	Text       string   `json:"text"`
	Buttons    []string `json:"buttons"`
	HasChoices bool     `json:"has_choices"`
	confirm    func(context.Context, int) error
}

type publishObservation struct {
	retry       func(context.Context) error
	NoteID      string               `json:"note_id,omitempty"`
	URL         string               `json:"url"`
	Title       string               `json:"title"`
	ButtonFound bool                 `json:"button_found"`
	Buttons     []publishButtonState `json:"publish_buttons"`
	Prompts     []string             `json:"visible_prompts"`
	SeenPrompts []string             `json:"observed_prompts"`
	Dialogs     []publishDialog      `json:"dialogs"`
	Scan        *publishScan         `json:"scan_verification,omitempty"`
	FormPresent bool                 `json:"form_present"`
	ReadErrors  []string             `json:"read_errors,omitempty"`
}

// 只接受已知创作平台落地页；登录、验证及任意外链都不是成功。
// 暂无同 URL 成功页的可靠现场，不凭 toast 放行。
func isPublishSuccessURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "creator.xiaohongshu.com" || u.User != nil {
		return false
	}
	switch strings.TrimSuffix(u.Path, "/") {
	case "/new/note-manager", "/new/home", "/publish/success", "/publish/publish-success":
		return true
	}
	return false
}

var publishAIWord = regexp.MustCompile(`(?i)\bai\b`)

func needsPublishOwner(text string) bool {
	if isPublishScanText(text) || publishAIWord.MatchString(text) {
		return true
	}
	lower := strings.ToLower(text)
	for _, word := range []string{"实名", "认证", "验证码", "安全验证", "滑块", "登录", "协议", "条款", "隐私", "付费", "支付", "营销", "授权", "原创", "声明", "人工智能", "生成式", "承诺", "责任"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// 整个弹窗必须是单纯发布确认，不能仅按按钮文案判断。
func safePublishConfirmation(d publishDialog) (int, bool) {
	if d.HasChoices || needsPublishOwner(d.Text) {
		return 0, false
	}
	body := strings.Join(strings.Fields(d.Text), "")
	index := -1
	for i, button := range d.Buttons {
		switch strings.TrimSpace(button) {
		case "发布", "确认发布", "确定发布":
			if index != -1 {
				return 0, false
			}
			index = i
		case "取消", "返回", "关闭":
		default:
			return 0, false
		}

	}
	for i := len(d.Buttons) - 1; i >= 0; i-- {
		body = strings.TrimSuffix(body, strings.Join(strings.Fields(d.Buttons[i]), ""))
	}
	body = strings.Trim(body, "？?。！!：:")
	switch body {
	case "", "确认", "是否", "确定", "是否发布", "确认发布", "确定发布", "确认发布笔记", "是否发布笔记", "确定发布笔记", "确认发布这篇笔记", "是否发布这篇笔记", "确定发布这篇笔记":
		return index, index >= 0
	}
	return 0, false
}

func appendPublishPrompts(dst []string, values ...string) []string {
	for _, value := range values {
		value = redactPublishText(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if existing == value {
				found = true
				break
			}
		}
		if !found && len(dst) < 64 {
			dst = append(dst, value)
		}
	}
	return dst
}

// 观察与点击分开，离线测试可断言所有决策和点击次数。
func monitorPublish(ctx context.Context, timeout time.Duration, observe func(context.Context) publishObservation) (publishObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*max(timeout, publishConfirmTimeout)+2*publishScanTimeout)
	defer cancel()
	var last publishObservation
	var seen []string
	confirmed := false
	deadline := time.Now().Add(timeout)
	var scanWait publishScanWait
	scanning := false
	verified, retried := false, false
	var stableSince time.Time
	for {
		current := observe(ctx)
		if current.URL != "" {
			last = current
		} else {
			last.ReadErrors = append(last.ReadErrors, current.ReadErrors...)
		}
		seen = appendPublishPrompts(seen, current.Prompts...)
		for _, dialog := range current.Dialogs {
			seen = appendPublishPrompts(seen, dialog.Text)
		}
		last.SeenPrompts = seen
		if current.Scan != nil {
			stableSince = time.Time{}
			if !scanning {
				scanWait = publishScanWait{}
			}
			scanning = true
			seen = appendPublishPrompts(seen, current.Scan.Text)
			last.SeenPrompts = seen
			if err := scanWait.update(ctx, current.Scan); err != nil {
				return last, err
			}
		} else if scanning && (len(current.ReadErrors) > 0 || current.URL == "") {
			// 读取失败不能证明验证弹窗已消失，也不能丢掉本人扫码提示。
			if time.Since(scanWait.started) >= publishScanTimeout {
				return last, fmt.Errorf("需要账号本人扫码验证：等待超过5分钟")
			}
		} else {
			if scanning {
				deadline = time.Now().Add(max(timeout, publishConfirmTimeout))
				verified = true
				scanning = false
			}
			for _, prompt := range current.Prompts {
				if publishVerificationPassed(prompt) {
					if !verified {
						deadline = time.Now().Add(max(timeout, publishConfirmTimeout))
					}
					verified = true
					continue
				}
				if needsPublishOwner(prompt) {
					return last, fmt.Errorf("需要账号本人处理：%s", redactPublishText(prompt))
				}
			}
			for _, prompt := range current.Prompts {
				for _, word := range []string{"失败", "错误", "超限", "超过", "不能为空", "未通过", "不支持", "不允许", "违规"} {
					if strings.Contains(prompt, word) {
						return last, fmt.Errorf("发布被页面拦截：%s", redactPublishText(prompt))
					}
				}
			}
			if len(current.Dialogs) > 1 {
				return last, fmt.Errorf("需要账号本人处理：同时出现多个发布弹窗")
			}
			for _, dialog := range current.Dialogs {
				index, safe := safePublishConfirmation(dialog)
				if !safe {
					return last, fmt.Errorf("需要账号本人处理：%s", redactPublishText(dialog.Text))
				}
				if !confirmed && len(current.ReadErrors) == 0 {
					if dialog.confirm == nil {
						return last, fmt.Errorf("发布确认按钮不可用")
					}
					if err := dialog.confirm(ctx, index); err != nil {
						return last, fmt.Errorf("点击二次发布确认失败: %w", err)
					}
					confirmed = true
				}
			}
			if len(current.ReadErrors) == 0 && len(current.Dialogs) == 0 && !current.FormPresent && isPublishSuccessURL(current.URL) {
				return last, nil
			}
			if verified && !retried && current.retry != nil && len(current.ReadErrors) == 0 && len(current.Dialogs) == 0 && current.FormPresent && isPublishEditorURL(current.URL) {
				if stableSince.IsZero() {
					stableSince = time.Now()
				}
				if time.Since(stableSince) >= 2*time.Second {
					// 点击结果即使不明也消耗唯一补点机会。
					retried = true
					if err := current.retry(ctx); err != nil {
						seen = appendPublishPrompts(seen, fmt.Sprintf("发布补点反馈未确认: %v", err))
					}
					confirmed = false
					deadline = time.Now().Add(max(timeout, publishConfirmTimeout))
				}
			} else {
				stableSince = time.Time{}
			}
			if !time.Now().Before(deadline) {
				return last, fmt.Errorf("发布未确认成功：未观察到发布表单退出并进入成功落地页")
			}
		}
		select {
		case <-ctx.Done():
			if scanning {
				return last, fmt.Errorf("需要账号本人扫码验证: %w", ctx.Err())
			}
			return last, fmt.Errorf("发布未确认成功：未观察到发布表单退出并进入成功落地页: %w", ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// 保留无稿件信息的诊断入口，生产路径使用 waitPublishResult。
func waitPublishSuccess(page *rod.Page, timeout time.Duration) error {
	_, err := waitPublishResult(page, timeout, publishAttempt{Started: time.Now()})
	return err
}

func waitPublishResult(page *rod.Page, timeout time.Duration, attempt publishAttempt) (*PublishResult, error) {
	last, err := monitorPublish(page.GetContext(), timeout, func(ctx context.Context) publishObservation {
		p := page.Context(ctx)
		state := observePublish(p)
		if attempt.Title != "" && retryPublishReady(p, state, attempt) {
			state.retry = func(ctx context.Context) error { return retryPublish(p.Context(ctx), attempt) }
		}
		return state
	})
	// 查重用独立页，保留编辑页诊断；不启动创作者登录流程。
	if attempt.Title != "" && page.GetContext().Err() == nil && (err != nil || last.NoteID == "") {
		id, lookupErr := lookupPublishedNote(page, attempt)
		if lookupErr == nil && id != "" {
			return &PublishResult{NoteID: id}, nil
		}
		if err != nil && lookupErr != nil {
			err = errors.Join(err, fmt.Errorf("笔记管理回读未确认: %w", lookupErr))
		}
	}
	if err != nil {
		return nil, publishFailureDiagnostic(page, attempt.Started, last, err)
	}
	slog.Info("发布已确认成功", "url", last.URL)
	return &PublishResult{NoteID: last.NoteID}, nil
}

func publishElementDisabled(elem *rod.Element) (bool, error) {
	for _, attr := range []string{"disabled", "aria-disabled", "submit-disabled"} {
		value, err := elem.Attribute(attr)
		if err != nil {
			return false, err
		}
		if value != nil && (attr == "disabled" || *value == "true") {
			return true, nil
		}
	}
	return false, nil
}

func observePublish(page *rod.Page) publishObservation {
	page = page.Timeout(2 * time.Second)
	defer page.CancelTimeout()
	var out publishObservation
	record := func(err error) {
		if err != nil {
			out.ReadErrors = appendPublishPrompts(out.ReadErrors, err.Error())
		}
	}
	if info, err := page.Info(); err != nil {
		record(err)
	} else {
		out.URL = redactPublishURL(info.URL)
		if isPublishSuccessURL(info.URL) {
			out.NoteID = publishURLNoteID(info.URL)
		}
		out.Title = redactPublishText(info.Title)
	}
	buttons, err := page.Elements(`xhs-publish-btn, .publish-page-publish-btn button.bg-red`)
	record(err)
	out.ButtonFound = len(buttons) > 0
	for _, elem := range buttons {
		state := publishButtonState{Version: "legacy", Attributes: map[string]string{}}
		tag, err := elem.Property("tagName")
		record(err)
		if strings.EqualFold(tag.Str(), "xhs-publish-btn") {
			state.Version = "widget"
		}
		state.Visible, err = elem.Visible()
		record(err)
		text, err := elem.Text()
		record(err)
		state.Text = redactPublishText(text)
		for _, key := range []string{"is-publish", "submit-disabled", "disabled", "aria-disabled"} {
			value, err := elem.Attribute(key)
			record(err)
			if value != nil {
				state.Attributes[key] = redactPublishText(*value)
			}
		}
		out.Buttons = append(out.Buttons, state)
	}
	hints, err := page.Elements(publishHints)
	record(err)
	for _, elem := range hints {
		visible, err := elem.Visible()
		record(err)
		if !visible {
			continue
		}
		text, err := elem.Text()
		record(err)
		out.Prompts = appendPublishPrompts(out.Prompts, text)
	}
	// 计算样式只能通过 DOM API 读取；仅取可见红字，不导出 HTML 或页面存储。
	red, err := page.Eval(`() => Array.from(document.querySelectorAll('body *')).filter(el => {
  if (el.children.length || el.closest('script, style, [contenteditable="true"]')) return false;
  const s = getComputedStyle(el), c = s.color.match(/[\d.]+/g), r = el.getBoundingClientRect();
  return r.width > 0 && r.height > 0 && s.visibility === 'visible' && c && +c[0] > 150 && +c[0] > +c[1] * 1.4 && +c[0] > +c[2] * 1.4;
 }).map(el => el.innerText).filter(Boolean).slice(0, 32)`)
	record(err)
	if err == nil {
		for _, text := range red.Value.Arr() {
			out.Prompts = appendPublishPrompts(out.Prompts, text.Str())
		}
	}
	out.Scan, err = observePublishScan(page)
	record(err)
	if out.Scan != nil {
		out.Dialogs = []publishDialog{{Text: out.Scan.Text}}
		out.FormPresent = true
		return out
	}
	dialogs, err := page.Elements(publishDialogs)
	record(err)
	for _, elem := range dialogs {
		visible, err := elem.Visible()
		record(err)
		if !visible {
			continue
		}
		// 嵌套 modal 容器只处理内层，防止重复点击。
		nested, err := elem.Elements(publishDialogs)
		record(err)
		hasNested := false
		for _, child := range nested {
			v, e := child.Visible()
			record(e)
			hasNested = hasNested || v
		}
		if hasNested {
			continue
		}
		text, err := elem.Text()
		record(err)
		d := publishDialog{Text: redactPublishText(text)}
		choices, err := elem.Elements(`input, select, [role="checkbox"], [role="radio"], [role="switch"], [contenteditable="true"]`)
		record(err)
		d.HasChoices = len(choices) > 0
		candidates, err := elem.Elements(`button, [role="button"], .d-button`)
		record(err)
		var active []*rod.Element
		for _, button := range candidates {
			v, err := button.Visible()
			record(err)
			if !v {
				continue
			}
			text, err := button.Text()
			record(err)
			d.Buttons = append(d.Buttons, redactPublishText(strings.TrimSpace(text)))
			active = append(active, button)
		}
		d.confirm = func(ctx context.Context, index int) error {
			button := active[index].Context(ctx)
			disabled, err := publishElementDisabled(button)
			if err != nil {
				return err
			}
			if disabled {
				return fmt.Errorf("二次确认按钮不可点击")
			}
			return button.Click(proto.InputMouseButtonLeft, 1)
		}
		out.Dialogs = append(out.Dialogs, d)
	}
	forms, err := page.Elements(`div.d-input input, .title-container input, div[contenteditable="true"], .upload-content`)
	record(err)
	for _, elem := range forms {
		visible, err := elem.Visible()
		record(err)
		out.FormPresent = out.FormPresent || visible
	}
	return out
}
