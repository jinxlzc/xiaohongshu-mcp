package xiaohongshu

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	urlOfNoteManager  = "https://creator.xiaohongshu.com/new/note-manager"
	noteCards         = `.note-item, .note-card, .note-list-item, [data-note-id]`
	permissionPanels  = `[role="dialog"], .d-modal, .d-dialog, .el-dialog, .d-options-wrapper, .permission-popover`
	permissionOptions = `[role="radio"], [role="option"], label, .custom-option, .permission-option`
)

var noteIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)

type NoteVisibilityResult struct {
	FeedID string `json:"feed_id"`
	Before string `json:"before"`
	After  string `json:"after"`
	Status string `json:"status"`
}

func ValidateNoteVisibility(feedID, visibility string) error {
	if !noteIDPattern.MatchString(feedID) {
		return fmt.Errorf("feed_id 必须是 24 位十六进制笔记 ID")
	}
	if visibility != "公开可见" && visibility != "仅自己可见" && visibility != "仅互关好友可见" {
		return fmt.Errorf("visibility 必须是：公开可见、仅自己可见、仅互关好友可见")
	}
	return nil
}

// changeNoteVisibility 只有明确回读目标状态才报告成功；幂等调用不触发保存。
func changeNoteVisibility(ctx context.Context, feedID, target string, read func() (string, error), write func() error) (*NoteVisibilityResult, error) {
	if err := ValidateNoteVisibility(feedID, target); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := read()
	if err != nil {
		return nil, err
	}
	if err := ValidateNoteVisibility(feedID, before); err != nil {
		return nil, fmt.Errorf("无法确认原可见范围: %w", err)
	}
	result := &NoteVisibilityResult{FeedID: feedID, Before: before, After: before, Status: "skipped"}
	if before == target {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := write(); err != nil {
		return nil, err
	}
	after, err := read()
	if err != nil {
		return nil, fmt.Errorf("已提交可见范围修改，但回读失败，请检查笔记状态: %w", err)
	}
	if after != target {
		return nil, fmt.Errorf("可见范围回读不符：修改前=%s，目标=%s，实际=%s；请检查笔记状态", before, target, after)
	}
	result.After = after
	result.Status = "updated"
	return result, nil
}

// SetNoteVisibility 只操作自己的管理列表，xsec_token 不参与此路径。
func SetNoteVisibility(ctx context.Context, page *rod.Page, feedID, visibility string) (*NoteVisibilityResult, error) {
	if err := ValidateNoteVisibility(feedID, visibility); err != nil {
		return nil, err
	}
	pp := page.Context(ctx).Timeout(90 * time.Second)
	defer pp.CancelTimeout()
	if err := EnsureCreatorSession(pp.GetContext(), pp); err != nil {
		return nil, err
	}
	var panel *rod.Element
	read := func() (string, error) {
		if err := pp.Navigate(urlOfNoteManager); err != nil {
			return "", err
		}
		card, err := findManagedNote(pp, feedID)
		if err != nil {
			return "", err
		}
		panel, err = openNotePermission(pp, card)
		if err != nil {
			return "", err
		}
		return selectedNoteVisibility(panel)
	}
	write := func() error {
		options, err := panel.Elements(permissionOptions)
		if err != nil {
			return err
		}
		var target *rod.Element
		for _, option := range options {
			text, err := option.Text()
			if err != nil {
				return err
			}
			if visibilityLabel(text) == visibility {
				visible, err := option.Visible()
				if err != nil {
					return err
				}
				if visible {
					target = option
					break
				}
			}
		}
		if target == nil {
			return fmt.Errorf("权限面板没有目标选项 %s；账号或页面可能不支持此范围", visibility)
		}
		wait := pp.WaitRequestIdle(800*time.Millisecond, nil, nil, nil)
		if err := target.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return err
		}
		// 某些面板选择即保存，另一些需要明确确认。只在当前权限面板中查找。
		buttons, err := panel.Elements(`button, [role="button"], .d-button`)
		if err == nil {
			for _, button := range buttons {
				text, e := button.Text()
				if e != nil {
					continue
				}
				text = strings.TrimSpace(text)
				if text == "确定" || text == "确认" || text == "保存" {
					visible, e := button.Visible()
					if e == nil && visible {
						if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
							return err
						}
						break
					}
				}
			}
		}
		// 给页面提交请求的机会；最终结果仍由重新导航后的持久状态决定。
		wait()
		return pp.GetContext().Err()
	}
	return changeNoteVisibility(pp.GetContext(), feedID, visibility, read, write)
}

func visibilityLabel(text string) string {
	text = strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	switch text {
	case "公开", "公开可见":
		return "公开可见"
	case "私密", "仅自己可见":
		return "仅自己可见"
	case "互关好友可见", "仅互关好友可见":
		return "仅互关好友可见"
	}
	return ""
}

func selectedNoteVisibility(panel *rod.Element) (string, error) {
	options, err := panel.Elements(permissionOptions)
	if err != nil {
		return "", err
	}
	selected := ""
	for _, option := range options {
		visible, err := option.Visible()
		if err != nil {
			return "", err
		}
		if !visible {
			continue
		}
		checked := false
		for _, attr := range []string{"aria-checked", "aria-selected"} {
			v, err := option.Attribute(attr)
			if err != nil {
				return "", err
			}
			checked = checked || (v != nil && *v == "true")
		}
		class, err := option.Attribute("class")
		if err != nil {
			return "", err
		}
		if class != nil {
			for _, c := range strings.Fields(*class) {
				checked = checked || c == "selected" || c == "is-selected" || c == "checked" || c == "is-checked"
			}
		}
		inputs, err := option.Elements(`input[type="radio"], input[type="checkbox"]`)
		if err != nil {
			return "", err
		}
		for _, in := range inputs {
			value, err := in.Property("checked")
			if err != nil {
				return "", err
			}
			checked = checked || value.Bool()
		}
		if !checked {
			continue
		}
		text, err := option.Text()
		if err != nil {
			return "", err
		}
		value := visibilityLabel(text)
		if value == "" {
			continue
		}
		if selected != "" && selected != value {
			return "", fmt.Errorf("权限面板有多个选中状态，拒绝修改")
		}
		selected = value
	}
	if selected == "" {
		return "", fmt.Errorf("无法识别权限面板当前选中状态，请核对选择器")
	}
	return selected, nil
}

func openNotePermission(page *rod.Page, card *rod.Element) (*rod.Element, error) {
	buttons, err := card.Elements(`button, [role="button"], .permission, .permission-setting, .permission-text, .dropdown-item, .d-dropdown-item`)
	if err != nil {
		return nil, err
	}
	var trigger *rod.Element
	for _, button := range buttons {
		text, err := button.Text()
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		if text == "权限设置" || text == "设置权限" || text == "修改可见范围" || visibilityLabel(text) != "" {
			visible, err := button.Visible()
			if err != nil {
				return nil, err
			}
			if visible {
				trigger = button
				break
			}
		}
	}
	if trigger == nil {
		return nil, fmt.Errorf("目标笔记没有可识别的权限设置入口，请核对管理页选择器")
	}
	// 排除点击前已展开的筛选器或其他笔记面板。
	existing := map[proto.DOMBackendNodeID]bool{}
	panels, err := page.Elements(permissionPanels)
	if err != nil {
		return nil, err
	}
	for _, panel := range panels {
		visible, err := panel.Visible()
		if err != nil {
			return nil, err
		}
		if visible {
			node, err := panel.Describe(0, false)
			if err != nil {
				return nil, err
			}
			existing[node.BackendNodeID] = true
		}
	}
	if err := trigger.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return nil, err
	}
	for {
		panels, err := page.Elements(permissionPanels)
		if err != nil {
			return nil, err
		}
		var candidates []*rod.Element
		for _, panel := range panels {
			visible, err := panel.Visible()
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
			options, err := panel.Elements(permissionOptions)
			if err != nil {
				return nil, err
			}
			count := 0
			for _, option := range options {
				text, err := option.Text()
				if err != nil {
					return nil, err
				}
				if visibilityLabel(text) != "" {
					count++
				}
			}
			if count >= 2 {
				node, err := panel.Describe(0, false)
				if err != nil {
					return nil, err
				}
				if !existing[node.BackendNodeID] {
					candidates = append(candidates, panel)
				}
			}
		}
		if len(candidates) > 1 {
			return nil, fmt.Errorf("出现多个权限面板，无法确认目标笔记，拒绝修改")
		}
		if len(candidates) == 1 {
			return candidates[0], nil
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			return nil, fmt.Errorf("未找到笔记权限面板: %w", err)
		}
	}
}

func managedNoteID(card *rod.Element) (string, error) {
	id, err := card.Attribute("data-note-id")
	if err != nil {
		return "", err
	}
	if id != nil && noteIDPattern.MatchString(*id) {
		return *id, nil
	}
	links, err := card.Elements(`a[href]`)
	if err != nil {
		return "", err
	}
	for _, link := range links {
		href, err := link.Attribute("href")
		if err != nil {
			return "", err
		}
		if href == nil {
			continue
		}
		u, err := url.Parse(*href)
		if err != nil {
			continue
		}
		if u.IsAbs() && u.Hostname() != "www.xiaohongshu.com" && u.Hostname() != "creator.xiaohongshu.com" {
			continue
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 && (parts[0] == "explore" || parts[0] == "discovery") && noteIDPattern.MatchString(parts[1]) {
			return parts[1], nil
		}
		if len(parts) == 3 && parts[0] == "discovery" && parts[1] == "item" && noteIDPattern.MatchString(parts[2]) {
			return parts[2], nil
		}
		for _, key := range []string{"note_id", "noteId"} {
			if id := u.Query().Get(key); noteIDPattern.MatchString(id) {
				return id, nil
			}
		}
	}
	return "", nil
}

func findManagedNote(page *rod.Page, feedID string) (*rod.Element, error) {
	previous := ""
	for {
		cards, err := page.Elements(noteCards)
		if err != nil {
			return nil, err
		}
		signature := ""
		for _, card := range cards {
			visible, err := card.Visible()
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
			id, err := managedNoteID(card)
			if err != nil {
				return nil, err
			}
			signature += id + ","
			if strings.EqualFold(id, feedID) {
				return card, nil
			}
		}
		if signature != "" && signature != previous {
			next, err := visibleElement(page, `.pagination .next, .el-pagination .btn-next, .d-pagination-next, button[aria-label="下一页"]`)
			if err != nil {
				return nil, err
			}
			if next == nil {
				return nil, fmt.Errorf("管理列表未找到笔记 %s；请确认归属、页面路径和选择器", feedID)
			}
			disabled, err := next.Attribute("disabled")
			if err != nil {
				return nil, err
			}
			aria, err := next.Attribute("aria-disabled")
			if err != nil {
				return nil, err
			}
			class, err := next.Attribute("class")
			if err != nil {
				return nil, err
			}
			if disabled != nil || (aria != nil && *aria == "true") || (class != nil && strings.Contains(*class, "disabled")) {
				return nil, fmt.Errorf("管理列表未找到笔记 %s", feedID)
			}
			previous = signature
			if err := next.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return nil, err
			}
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			return nil, fmt.Errorf("定位笔记超时，请核对管理页路径、分页及笔记 ID 选择器: %w", err)
		}
	}
}
