package xiaohongshu

import (
	"context"
	"encoding/json"
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
	// 总预算包含列表回读和本人扫码；每次查找、每轮扫码仍有独立上限。
	pp := page.Context(ctx).Timeout(15 * time.Minute)
	defer pp.CancelTimeout()
	if err := EnsureCreatorSession(pp.GetContext(), pp); err != nil {
		return nil, err
	}
	var panel *rod.Element
	read := func() (string, error) {
		if err := pp.Navigate(urlOfNoteManager); err != nil {
			return "", err
		}
		if err := pp.WaitLoad(); err != nil {
			return "", err
		}
		if err := waitNoteVerification(pp); err != nil {
			return "", err
		}
		card, err := findManagedNote(pp, feedID)
		if err != nil {
			return "", err
		}
		// 遮罩明确显示目标私密状态时无需打开菜单；不能由遮罩缺失推断公开。
		labels, err := card.Elements(".play_permission .permission_msg")
		if err != nil {
			return "", err
		}
		for _, label := range labels {
			visible, err := label.Visible()
			if err != nil {
				return "", err
			}
			text, err := label.Text()
			if err != nil {
				return "", err
			}
			if value := visibilityLabel(text); visible && value == visibility && value != "公开可见" {
				return value, nil
			}
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
		wait()
		if err := waitNoteVerification(pp); err != nil {
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
						waitSave := pp.WaitRequestIdle(800*time.Millisecond, nil, nil, nil)
						if err := button.Click(proto.InputMouseButtonLeft, 1); err != nil {
							return err
						}
						waitSave()
						break
					}
				}
			}
		}
		// 验证结束后重新导航，以持久状态判断是否成功。
		return waitNoteVerification(pp)
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
	if err := card.ScrollIntoView(); err != nil {
		return nil, err
	}
	if err := card.Hover(); err != nil {
		return nil, err
	}
	trigger, err := notePermissionTrigger(page, card)
	if err != nil {
		return nil, err
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
	if err := waitNoteVerification(page); err != nil {
		return nil, err
	}
	pp := page.Timeout(12 * time.Second)
	defer pp.CancelTimeout()
	for {
		panels, err := pp.Elements(permissionPanels)
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
			return candidates[0].Context(page.GetContext()), nil
		}
		if err := waitCreatorPoll(pp.GetContext()); err != nil {
			return nil, fmt.Errorf("未找到笔记权限面板: %w", err)
		}
	}
}

func managedNoteID(card *rod.Element) (string, error) {
	for _, attr := range []string{"data-note-id", "data-impression"} {
		value, err := card.Attribute(attr)
		if err != nil {
			return "", err
		}
		if value != nil {
			if id := managedNoteAttributeID(attr, *value); id != "" {
				return id, nil
			}
		}
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
		if u.Host != "" && u.Hostname() != "www.xiaohongshu.com" && u.Hostname() != "creator.xiaohongshu.com" {
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

// 只读取明确的笔记字段，不把埋点内其它 24 位值当成笔记 ID。
func managedNoteAttributeID(attr, value string) string {
	if attr == "data-impression" {
		var impression struct {
			NoteTarget struct {
				Value struct {
					NoteID string `json:"noteId"`
				} `json:"value"`
			} `json:"noteTarget"`
		}
		if json.Unmarshal([]byte(value), &impression) != nil {
			return ""
		}
		value = impression.NoteTarget.Value.NoteID
	} else if attr != "data-note-id" {
		return ""
	}
	if !noteIDPattern.MatchString(value) {
		return ""
	}
	return strings.ToLower(value)
}

func findManagedNote(page *rod.Page, feedID string) (_ *rod.Element, err error) {
	pp := page.Timeout(90 * time.Second)
	defer pp.CancelTimeout()
	defer func() {
		if err != nil && pp.GetContext().Err() != nil {
			err = fmt.Errorf("定位笔记超时或已取消；请核对列表加载状态及笔记 ID 选择器: %w", pp.GetContext().Err())
		}
	}()
	if err := pp.WaitLoad(); err != nil {
		return nil, err
	}
	previous, paged := "", ""
	stableSince := time.Now()
	for {
		allCards, err := pp.Elements(noteCards)
		if err != nil {
			return nil, err
		}
		cards := rod.Elements{}
		ids := []string{}
		unknown := 0
		for _, card := range allCards {
			// Visible 不要求在视口内，但排除非激活列表中的隐藏副本。
			visible, err := card.Visible()
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
			cards = append(cards, card)
			id, err := managedNoteID(card)
			if err != nil {
				return nil, err
			}
			if id == "" {
				unknown++
			} else {
				ids = append(ids, strings.ToLower(id))
			}
			if id != "" && strings.EqualFold(id, feedID) {
				return card.Context(page.GetContext()), nil
			}
		}
		scroller, err := managedNoteScroller(pp, cards)
		if err != nil {
			return nil, err
		}
		top, err := scroller.Property("scrollTop")
		if err != nil {
			return nil, err
		}
		height, err := scroller.Property("scrollHeight")
		if err != nil {
			return nil, err
		}
		client, err := scroller.Property("clientHeight")
		if err != nil {
			return nil, err
		}
		loading, err := managedNotesLoading(pp)
		if err != nil {
			return nil, err
		}
		signature := fmt.Sprintf("%d/%s/%g/%g", len(cards), strings.Join(ids, ","), top.Num(), height.Num())
		if signature != previous || loading {
			previous, stableSince = signature, time.Now()
		}
		bottom := top.Num()+client.Num() >= height.Num()-2
		if bottom && !loading && time.Since(stableSince) >= 1500*time.Millisecond {
			next, err := visibleElement(pp, `.pagination .next, .el-pagination .btn-next, .d-pagination-next, button[aria-label="下一页"]`)
			if err != nil {
				return nil, err
			}
			canPage := false
			if next != nil {
				disabled, err := publishElementDisabled(next)
				if err != nil {
					return nil, err
				}
				class, err := next.Attribute("class")
				if err != nil {
					return nil, err
				}
				canPage = !disabled && (class == nil || !strings.Contains(*class, "disabled"))
				if canPage && signature != paged {
					if err := next.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return nil, err
					}
					paged, stableSince = signature, time.Now()
					continue
				}
			}
			// 空 ID 可能是尚未完成的卡片渲染，不能用逗号签名宣告结束。
			// 点击后仍是旧页时继续等待异步换页。
			if unknown == 0 && !canPage {
				return nil, fmt.Errorf("管理列表未找到笔记 %s；列表已停止增长且加载提示消失，请确认笔记归属", feedID)
			}
		}
		if !bottom {
			if err := scroller.Hover(); err != nil {
				return nil, err
			}
			if err := pp.Mouse.Scroll(0, max(client.Num()*0.8, 200), 1); err != nil {
				return nil, err
			}
		}
		if err := waitCreatorPoll(pp.GetContext()); err != nil {
			return nil, err
		}
	}
}

// 优先使用卡片所在的可滚动祖先，兼容内容容器更名与整页滚动。
func managedNoteScroller(page *rod.Page, cards rod.Elements) (*rod.Element, error) {
	candidates, err := page.Elements("div.content")
	if err != nil {
		return nil, err
	}
	if len(cards) > 0 {
		parents, err := cards[0].Parents("*")
		if err != nil {
			return nil, err
		}
		candidates = append(parents, candidates...)
	}
	for _, el := range candidates {
		v, err := el.Eval(`() => /auto|scroll/.test(getComputedStyle(this).overflowY) && this.clientHeight > 0 && this.scrollHeight > this.clientHeight + 1`)
		if err != nil {
			return nil, err
		}
		if v.Value.Bool() {
			return el, nil
		}
	}
	return page.Element("html")
}

func managedNotesLoading(page *rod.Page) (bool, error) {
	elements, err := page.ElementsX(`//*[@aria-busy="true" or @role="progressbar" or normalize-space(text())="正在加载中..." or normalize-space(text())="加载中..." or normalize-space(text())="正在加载中…" or normalize-space(text())="加载中"]`)
	if err != nil {
		return false, err
	}
	for _, el := range elements {
		visible, err := el.Visible()
		if err != nil {
			return false, err
		}
		if visible {
			return true, nil
		}
	}
	return false, nil
}

// 复用发布流程的二维码文件、过期刷新和五分钟上限。
func waitNoteVerification(page *rod.Page) error {
	var wait publishScanWait
	quietSince := time.Now()
	for {
		scan, err := observePublishScan(page)
		if err != nil {
			if !wait.started.IsZero() {
				return fmt.Errorf("需要账号本人扫码验证：读取验证层失败: %w", err)
			}
			return err
		}
		if scan != nil {
			if err := wait.update(page.GetContext(), scan); err != nil {
				return err
			}
			quietSince = time.Now()
		} else if time.Since(quietSince) >= time.Second {
			return nil
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			if !wait.started.IsZero() {
				return fmt.Errorf("需要账号本人扫码验证: %w", err)
			}
			return err
		}
	}
}

func permissionEntry(elements rod.Elements) (*rod.Element, error) {
	var result *rod.Element
	for _, el := range elements {
		text, err := el.Text()
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		if text != "权限设置" && text != "设置权限" && text != "修改可见范围" {
			continue
		}
		visible, err := el.Visible()
		if err != nil {
			return nil, err
		}
		if visible {
			result = el
		}
	}
	return result, nil
}

func notePermissionTrigger(page *rod.Page, card *rod.Element) (*rod.Element, error) {
	menus := `[role="menu"], .d-dropdown-menu, .dropdown-menu, .d-popover, .d-popover-content`
	existing := map[proto.DOMBackendNodeID]bool{}
	visibleMenus, err := page.Elements(menus)
	if err != nil {
		return nil, err
	}
	for _, menu := range visibleMenus {
		visible, err := menu.Visible()
		if err != nil {
			return nil, err
		}
		if visible {
			node, err := menu.Describe(0, false)
			if err != nil {
				return nil, err
			}
			existing[node.BackendNodeID] = true
		}
	}
	opened := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := card.Elements(`button, [role="button"], div, span, a`)
		if err != nil {
			return nil, err
		}
		entry, err := permissionEntry(entries)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			return entry, nil
		}
		controls, err := card.Elements(".permission, .permission-setting, .permission-text")
		if err != nil {
			return nil, err
		}
		for _, control := range controls {
			text, err := control.Text()
			if err != nil {
				return nil, err
			}
			visible, err := control.Visible()
			if err != nil {
				return nil, err
			}
			if visible && visibilityLabel(text) != "" {
				return control, nil
			}
		}
		if opened {
			candidates, err := page.Elements(menus)
			if err != nil {
				return nil, err
			}
			for _, menu := range candidates {
				node, err := menu.Describe(0, false)
				if err != nil {
					return nil, err
				}
				if existing[node.BackendNodeID] {
					continue
				}
				entries, err := menu.Elements(`button, [role="menuitem"], div, span, a, li`)
				if err != nil {
					return nil, err
				}
				found, err := permissionEntry(entries)
				if err != nil {
					return nil, err
				}
				if found != nil {
					if entry != nil {
						a, err := entry.Describe(0, false)
						if err != nil {
							return nil, err
						}
						b, err := found.Describe(0, false)
						if err != nil {
							return nil, err
						}
						if a.BackendNodeID != b.BackendNodeID {
							return nil, fmt.Errorf("出现多个权限菜单，拒绝修改")
						}
					}
					entry = found
				}
			}
			if entry != nil {
				return entry, nil
			}
		} else {
			more, err := card.Elements(`button, [role="button"], [aria-haspopup="menu"], .more, .more-button, .more-icon, .note-card__more`)
			if err != nil {
				return nil, err
			}
			for _, el := range more {
				visible, err := el.Visible()
				if err != nil {
					return nil, err
				}
				if !visible {
					continue
				}
				text, err := el.Text()
				if err != nil {
					return nil, err
				}
				labels := []string{text}
				for _, attr := range []string{"aria-label", "title", "class", "aria-haspopup"} {
					value, err := el.Attribute(attr)
					if err != nil {
						return nil, err
					}
					if value != nil {
						labels = append(labels, *value)
					}
				}
				allowed := false
				for _, label := range labels {
					switch strings.TrimSpace(label) {
					case "更多", "更多操作", "操作", "...", "…", "⋯", "menu":
						allowed = true
					}
					for _, class := range strings.Fields(label) {
						switch class {
						case "more", "more-button", "more-icon", "note-card__more":
							allowed = true
						}
					}
				}
				if allowed {
					if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return nil, err
					}
					opened = true
					break
				}
			}
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("目标笔记 hover 后仍没有可识别的权限设置入口，请核对管理页选择器")
}
