package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

var publishURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
var publishSecretPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_-]*(?:token|secret|password|api[_-]?key)|authorization|cookie|set-cookie|localstorage|sessionstorage)\s*["']?\s*[:=]|\bbearer\s+\S+`)

// 查询参数和 fragment 一律丢弃，避免漏掉未识别的凭证字段。
func redactPublishURL(raw string) string {
	if raw == "about:blank" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[URL unavailable]"
	}
	if u.User != nil {
		return "[URL redacted]"
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u.String()
}

func redactPublishText(text string) string {
	text = publishURLPattern.ReplaceAllStringFunc(text, redactPublishURL)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if publishSecretPattern.MatchString(line) {
			lines[i] = "[REDACTED]"
		}
	}
	return strings.Join(lines, "\n")
}

type publishDiagnostic struct {
	StartedAt       time.Time `json:"publish_started_at"`
	CapturedAt      time.Time `json:"captured_at"`
	Error           string    `json:"error"`
	ScreenshotError string    `json:"screenshot_error,omitempty"`
	publishObservation
}

// 最后一道脱敏覆盖所有字符串字段，避免新字段漏掉调用。
func sanitizePublishJSON(value any) any {
	switch v := value.(type) {
	case string:
		return redactPublishText(v)
	case []any:
		for i := range v {
			v[i] = sanitizePublishJSON(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = sanitizePublishJSON(v[k])
		}
	}
	return value
}

func savePublishDiagnostic(root string, data publishDiagnostic, screenshot func() ([]byte, error)) (string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, data.CapturedAt.UTC().Format("20060102T150405.000000000Z")+"-")
	if err != nil {
		return "", err
	}
	// 先写元数据，截图失败或浏览器不可用也不丢现场。
	writeMetadata := func() error {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		raw, err = json.MarshalIndent(sanitizePublishJSON(value), "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "metadata.json"), raw, 0600)
	}
	if err := writeMetadata(); err != nil {
		return dir, err
	}
	png, err := screenshot()
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "page.png"), png, 0600)
	}
	if err != nil {
		data.ScreenshotError = redactPublishText(err.Error())
	}
	return dir, writeMetadata()
}

// 仅截图前临时遮盖凭证文本和输入框，不读取 cookie 或 Web Storage。
// 文本节点遍历和跨节点遮盖需 DOM API；其余采集、查找和点击均使用 go-rod。
func publishScreenshot(page *rod.Page) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(page.GetContext()), 5*time.Second)
	defer cancel()
	page = page.Context(ctx)
	restore, err := page.Evaluate(rod.Eval(`() => {
  const saved = new Map();
  const hide = el => { if (!saved.has(el)) { saved.set(el, el.getAttribute('style')); el.style.setProperty('opacity', '0', 'important'); } };
  const secret = /(?:[a-z0-9_-]*(?:token|secret|password|api[_-]?key)|authorization|cookie|localstorage|sessionstorage)\s*["']?\s*[:=]|\bbearer\s+\S+|https?:\/\/[^\s]*[?#@]/i;
  const walk = root => {
   root.querySelectorAll('input, textarea, iframe, object, embed').forEach(hide);
   const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
   while (walker.nextNode()) { const n = walker.currentNode; if (secret.test(n.textContent) && n.parentElement) hide(n.parentElement.parentElement || n.parentElement); }
   root.querySelectorAll('*').forEach(el => { if (el.shadowRoot) walk(el.shadowRoot); });
  };
  walk(document);
  return () => { for (const [el, style] of saved) { if (style === null) el.removeAttribute('style'); else el.setAttribute('style', style); } };
 }`).ByObject())
	if err != nil {
		return nil, fmt.Errorf("截图脱敏准备失败: %w", err)
	}
	defer func() {
		clean, done := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer done()
		p := page.Context(clean)
		_, _ = p.Evaluate(rod.Eval(`function() { this(); }`).This(restore))
		_ = p.Release(restore)
	}()
	return page.Screenshot(true, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
}

type publishDiagnosticError struct {
	cause   error
	message string
}

func (e *publishDiagnosticError) Error() string { return e.message }
func (e *publishDiagnosticError) Unwrap() error { return e.cause }

func publishFailureDiagnostic(page *rod.Page, started time.Time, last publishObservation, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(page.GetContext()), 3*time.Second)
	defer cancel()
	current := observePublish(page.Context(ctx))
	for _, dialog := range last.Dialogs {
		last.SeenPrompts = appendPublishPrompts(last.SeenPrompts, dialog.Text)
	}
	if current.URL != "" {
		current.SeenPrompts = appendPublishPrompts(last.SeenPrompts, last.Prompts...)
		last = current
	} else {
		last.ReadErrors = append(last.ReadErrors, current.ReadErrors...)
	}
	last.SeenPrompts = appendPublishPrompts(last.SeenPrompts, last.Prompts...)
	for _, dialog := range last.Dialogs {
		last.SeenPrompts = appendPublishPrompts(last.SeenPrompts, dialog.Text)
	}
	data := publishDiagnostic{StartedAt: started, CapturedAt: time.Now(), Error: redactPublishText(cause.Error()), publishObservation: last}
	root, err := filepath.Abs("diag")
	dir := ""
	if err == nil {
		dir, err = savePublishDiagnostic(root, data, func() ([]byte, error) { return publishScreenshot(page) })
	}
	message := publishFailureMessage(cause, last, dir)
	if err != nil {
		message += "; 诊断写入失败: " + redactPublishText(err.Error())
	}
	slog.Error("发布失败现场", "diag_dir", dir, "summary", message)
	return &publishDiagnosticError{cause: cause, message: message}
}

func publishFailureMessage(cause error, last publishObservation, dir string) string {
	prompts := appendPublishPrompts(nil, last.SeenPrompts...)
	prompts = appendPublishPrompts(prompts, last.Prompts...)
	for _, d := range last.Dialogs {
		prompts = appendPublishPrompts(prompts, d.Text)
	}
	summary := strings.Join(prompts, " | ")
	if summary == "" {
		summary = "未采集到可见提示"
	}
	if r := []rune(summary); len(r) > 1200 {
		summary = string(r[:1200]) + "…"
	}
	return fmt.Sprintf("%s; URL=%s; 可见提示=%s; 诊断目录=%s", redactPublishText(cause.Error()), redactPublishURL(last.URL), summary, dir)
}
