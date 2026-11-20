package xiaohongshu

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// PublishResult 只返回笔记标识，不携带链接中的凭证。
type PublishResult struct {
	NoteID string `json:"note_id,omitempty"`
}

type publishAttempt struct {
	Title      string
	ImageCount int
	Video      bool
	Started    time.Time
}

var verificationPassed = regexp.MustCompile(`^验证成功[,，]?(正在登录[.。…]*)?$`)

func publishVerificationPassed(text string) bool {
	return verificationPassed.MatchString(strings.Join(strings.Fields(text), ""))
}

func isPublishEditorURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "creator.xiaohongshu.com" && u.User == nil && strings.TrimSuffix(u.Path, "/") == "/publish/publish"
}

func publishURLNoteID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !isPublishSuccessURL(raw) {
		return ""
	}
	for _, key := range []string{"note_id", "noteId", "id"} {
		if id := u.Query().Get(key); noteIDPattern.MatchString(id) {
			return id
		}
	}
	return ""
}

func retryPublishReady(page *rod.Page, state publishObservation, attempt publishAttempt) bool {
	if !isPublishEditorURL(state.URL) || !state.FormPresent || state.Scan != nil || len(state.Dialogs) != 0 || len(state.ReadErrors) != 0 {
		return false
	}
	for _, text := range state.Prompts {
		if needsPublishOwner(text) && !publishVerificationPassed(text) {
			return false
		}
	}
	title, err := visibleElement(page, `div.d-input input, .title-container input`)
	if err != nil || title == nil {
		return false
	}
	value, err := title.Property("value")
	if err != nil || strings.TrimSpace(value.Str()) == "" || value.Str() != attempt.Title {
		return false
	}
	if attempt.Video {
		video, err := visibleElement(page, `video`)
		if err != nil || video == nil {
			return false
		}
		source, err := video.Property("currentSrc")
		if err != nil || source.Str() == "" {
			return false
		}
	} else {
		images, err := page.Elements(`.img-preview-area .pr`)
		if err != nil || attempt.ImageCount == 0 || len(images) != attempt.ImageCount {
			return false
		}
		for _, img := range images {
			visible, err := img.Visible()
			if err != nil || !visible {
				return false
			}
		}
	}
	button, reason, err := findPublishButton(page)
	if err != nil || reason != "" || button == nil {
		return false
	}
	_, err = button.elem.Interactable()
	return err == nil
}

func retryPublish(page *rod.Page, attempt publishAttempt) error {
	page = page.Timeout(5 * time.Second)
	defer page.CancelTimeout()
	// 点击前重新读回，防止稳定等待期间页面换稿、丢图或再弹验证层。
	if !retryPublishReady(page, observePublish(page), attempt) {
		return fmt.Errorf("标题、素材或页面状态发生变化，停止补点")
	}
	button, reason, err := findPublishButton(page)
	if err != nil {
		return err
	}
	if button == nil || reason != "" {
		return fmt.Errorf("发布按钮不可点击")
	}
	// go-rod 检查遮挡后仅发送一次鼠标点击，不关闭任何弹窗。
	if button.isWidget {
		return clickPublishWidget(page, button.elem)
	}
	return button.elem.Click(proto.InputMouseButtonLeft, 1)
}

// 同一 BrowserContext 的新页共享登录态，原编辑页留作失败诊断。
func lookupPublishedNote(page *rod.Page, attempt publishAttempt) (string, error) {
	ctx, cancel := context.WithTimeout(page.GetContext(), 45*time.Second)
	defer cancel()
	manager, err := page.Browser().Context(ctx).Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", err
	}
	defer func() { _ = manager.Context(context.WithoutCancel(ctx)).Timeout(2 * time.Second).Close() }()
	if err := manager.Navigate(urlOfNoteManager); err != nil {
		return "", err
	}
	return waitManagedPublishedNote(manager, attempt)
}

func waitManagedPublishedNote(page *rod.Page, attempt publishAttempt) (string, error) {
	var readErr error
	for {
		info, err := page.Info()
		if err != nil {
			return "", err
		}
		if strings.Contains(info.URL, "/login") {
			return "", fmt.Errorf("创作者会话未就绪，不自动重新登录")
		}
		if redactPublishURL(info.URL) == urlOfNoteManager {
			id, err := readManagedPublishedNote(page, attempt, time.Now())
			if err == nil && id != "" {
				return id, nil
			}
			readErr = err
		}
		select {
		case <-page.GetContext().Done():
			if readErr != nil {
				return "", fmt.Errorf("无法读取笔记管理列表: %w", readErr)
			}
			return "", fmt.Errorf("管理列表未确认同标题新笔记: %w", page.GetContext().Err())
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func readManagedPublishedNote(page *rod.Page, attempt publishAttempt, now time.Time) (string, error) {
	page = page.Timeout(3 * time.Second)
	defer page.CancelTimeout()
	dialogs, err := page.Elements(publishDialogs)
	if err != nil {
		return "", err
	}
	for _, dialog := range dialogs {
		visible, err := dialog.Visible()
		if err != nil {
			return "", err
		}
		if visible {
			return "", fmt.Errorf("管理列表被弹窗遮挡，需本人处理")
		}
	}
	cards, err := page.Elements(noteCards)
	if err != nil {
		return "", err
	}
	if len(cards) == 0 {
		return "", fmt.Errorf("未读到笔记列表")
	}
	found := ""
	for _, card := range cards {
		visible, err := card.Visible()
		if err != nil {
			return "", err
		}
		if !visible {
			continue
		}
		titles, err := card.Elements(`.note-title, .title, [data-note-title]`)
		if err != nil || len(titles) == 0 {
			continue
		}
		text, err := titles[0].Text()
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) != attempt.Title {
			continue
		}
		statuses, err := card.Elements(`.note-status, .status`)
		if err != nil {
			return "", err
		}
		unpublished := false
		for _, status := range statuses {
			text, err := status.Text()
			if err != nil {
				return "", err
			}
			for _, word := range []string{"草稿", "未发布", "发布失败"} {
				unpublished = unpublished || strings.Contains(text, word)
			}
		}
		if unpublished {
			continue
		}
		id, err := managedNoteID(card)
		if err != nil {
			return "", err
		}
		if id == "" {
			continue
		}
		published, err := managedPublishTime(card)
		if err != nil || published.Before(attempt.Started.Truncate(time.Second)) || published.After(now.Add(time.Second)) {
			continue
		}
		// 时间精度不足、缺失或多个同名新笔记均不能证明本次成功。
		if found != "" && found != id {
			return "", fmt.Errorf("存在多个同标题新笔记，无法确认本次笔记 ID")
		}
		found = id
	}
	return found, nil
}

var managedDate = regexp.MustCompile(`\d{4}[-/]\d{2}[-/]\d{2}[ T]\d{2}:\d{2}(:\d{2})?`)

func managedPublishTime(card *rod.Element) (time.Time, error) {
	value, err := card.Attribute("data-publish-time")
	if err != nil {
		return time.Time{}, err
	}
	if value != nil {
		if parsed, err := parseManagedPublishTime(*value); err == nil {
			return parsed, nil
		}
	}
	elements, err := card.Elements(`time, .publish-time, .note-time, .time, .publish-date, [data-publish-time]`)
	if err != nil {
		return time.Time{}, err
	}
	for _, el := range elements {
		visible, err := el.Visible()
		if err != nil {
			return time.Time{}, err
		}
		if !visible {
			continue
		}
		for _, attr := range []string{"datetime", "data-publish-time"} {
			value, err := el.Attribute(attr)
			if err != nil {
				return time.Time{}, err
			}
			if value != nil {
				if parsed, err := parseManagedPublishTime(*value); err == nil {
					return parsed, nil
				}
			}
		}
		text, err := el.Text()
		if err != nil {
			return time.Time{}, err
		}
		if parsed, err := parseManagedPublishTime(text); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("缺少可确认的发布时间")
}

func parseManagedPublishTime(text string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		return parsed, nil
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		if len(text) == 13 {
			return time.UnixMilli(n), nil
		}
		if len(text) == 10 {
			return time.Unix(n, 0), nil
		}
	}
	// 创作者页面显示北京时间，不使用服务进程本地时区。
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	value := strings.ReplaceAll(managedDate.FindString(text), "/", "-")
	value = strings.ReplaceAll(value, "T", " ")
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if parsed, err := time.ParseInLocation(layout, value, zone); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("发布时间不可确认")
}
