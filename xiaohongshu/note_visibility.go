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
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

const (
	urlOfNoteManager  = "https://creator.xiaohongshu.com/new/note-manager"
	noteCards         = `.note-item, .note-card, .note-list-item, [data-note-id]`
	permissionPanels  = `[role="dialog"], .d-modal, .d-dialog, .el-dialog, .d-options-wrapper, .permission-popover`
	noteActionPanels  = permissionPanels + `, [role="alertdialog"], .d-popover, .el-popover`
	permissionButtons = `button, [role="button"], .d-button, .btn-cancel, .btn-confirm`
	permissionSelects = `.d-select-content, [role="combobox"]`
	permissionTitle   = `.//*[normalize-space(text())="权限设置"]`
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
		value, err := selectedNoteVisibility(panel)
		if err != nil {
			return "", err
		}
		// 下拉弹窗回读已达目标时取消退出，幂等调用不提交。
		modal, err := isNotePermissionModal(panel)
		if err != nil {
			return "", err
		}
		if modal && value == visibility {
			if err := closeNoteActionPanel(panel); err != nil {
				return "", err
			}
		}
		return value, nil
	}
	write := func() error {
		options, err := noteVisibilityOptions(pp, panel)
		if err != nil {
			return err
		}
		var target *rod.Element
		for _, option := range options {
			text, err := option.Text()
			if err != nil {
				return err
			}
			deleted, err := option.Matches(`.note-card__action-btn--del, .note-card__action-btn--del *`)
			if err != nil {
				return err
			}
			if !deleted && visibilityLabel(text) == visibility {
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
		buttons, err := panel.Elements(permissionButtons)
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
	modal, err := isNotePermissionModal(panel)
	if err != nil {
		return "", err
	}
	if modal {
		selectBox, err := notePermissionSelect(panel)
		if err != nil {
			return "", err
		}
		if selectBox != nil {
			text, err := selectBox.Text()
			if err != nil {
				return "", err
			}
			if value := visibilityLabel(text); value != "" {
				return value, nil
			}
			return "", fmt.Errorf("无法识别权限下拉框当前值")
		}
	}
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

// 标题和取消/确定共同限定弹窗，不依赖未展开的选项数量。
func isNotePermissionModal(panel *rod.Element) (bool, error) {
	titles, err := panel.ElementsX(permissionTitle)
	if err != nil {
		return false, err
	}
	titleCount := 0
	for _, title := range titles {
		visible, err := title.Visible()
		if err != nil {
			return false, err
		}
		if visible {
			titleCount++
		}
	}
	if titleCount != 1 {
		return false, nil
	}
	buttons, err := panel.Elements(permissionButtons)
	if err != nil {
		return false, err
	}
	cancel, confirm := false, false
	for _, button := range buttons {
		visible, err := button.Visible()
		if err != nil {
			return false, err
		}
		if !visible {
			continue
		}
		text, err := button.Text()
		if err != nil {
			return false, err
		}
		cancel = cancel || strings.TrimSpace(text) == "取消"
		confirm = confirm || strings.TrimSpace(text) == "确定"
	}
	return cancel && confirm, nil
}

// 真实弹窗未必带 dialog class；从标题向上寻找最小完整容器。
func noteActionPanelElements(page *rod.Page) (rod.Elements, error) {
	panels, err := page.Elements(noteActionPanels)
	if err != nil {
		return nil, err
	}
	titles, err := page.ElementsX(permissionTitle)
	if err != nil {
		return nil, err
	}
	for _, title := range titles {
		visible, err := title.Visible()
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		parents, err := title.Parents(":not(body):not(html)")
		if err != nil {
			return nil, err
		}
		for _, parent := range parents {
			modal, err := isNotePermissionModal(parent)
			if err != nil {
				return nil, err
			}
			if modal {
				panels = append(panels, parent)
				break
			}
		}
	}
	seen := map[proto.DOMBackendNodeID]bool{}
	var unique rod.Elements
	for _, panel := range panels {
		node, err := panel.Describe(0, false)
		if err != nil {
			return nil, err
		}
		if !seen[node.BackendNodeID] {
			seen[node.BackendNodeID] = true
			unique = append(unique, panel)
		}
	}
	return unique, nil
}

func notePermissionSelect(panel *rod.Element) (*rod.Element, error) {
	selects, err := panel.Elements(permissionSelects)
	if err != nil {
		return nil, err
	}
	var result *rod.Element
	for _, selectBox := range selects {
		visible, err := selectBox.Visible()
		if err != nil {
			return nil, err
		}
		if visible {
			if result != nil {
				return nil, fmt.Errorf("权限面板有多个下拉框，拒绝修改")
			}
			result = selectBox
		}
	}
	return result, nil
}

// 兼容弹窗内选项和挂载到 body 的下拉层，排除点击前已展开的筛选器。
func noteVisibilityOptions(page *rod.Page, panel *rod.Element) (rod.Elements, error) {
	selectBox, err := notePermissionSelect(panel)
	if err != nil {
		return nil, err
	}
	if selectBox == nil {
		return panel.Elements(permissionOptions)
	}
	const lists = `.d-options-wrapper, [role="listbox"]`
	existing := map[proto.DOMBackendNodeID]bool{}
	before, err := page.Elements(lists)
	if err != nil {
		return nil, err
	}
	for _, list := range before {
		visible, err := list.Visible()
		if err != nil {
			return nil, err
		}
		if visible {
			node, err := list.Describe(0, false)
			if err != nil {
				return nil, err
			}
			existing[node.BackendNodeID] = true
		}
	}
	if err := selectBox.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		opened, err := page.Elements(lists)
		if err != nil {
			return nil, err
		}
		var candidates rod.Elements
		for _, list := range opened {
			visible, err := list.Visible()
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
			node, err := list.Describe(0, false)
			if err != nil {
				return nil, err
			}
			if !existing[node.BackendNodeID] {
				candidates = append(candidates, list)
			}
		}
		if len(candidates) > 1 {
			return nil, fmt.Errorf("出现多个可见范围下拉层，拒绝修改")
		}
		if len(candidates) == 1 {
			options, err := candidates[0].Elements(permissionOptions)
			if err != nil {
				return nil, err
			}
			if len(options) > 0 {
				return options, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("展开权限下拉框后未找到可见范围选项")
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			return nil, err
		}
	}
}

func openNotePermission(page *rod.Page, card *rod.Element) (*rod.Element, error) {
	if err := card.ScrollIntoView(); err != nil {
		return nil, err
	}
	if err := card.Hover(); err != nil {
		return nil, err
	}
	triggers, icons, err := notePermissionTriggers(page, card)
	if err != nil {
		return nil, err
	}
	// 排除点击前已展开的筛选器或其他笔记面板。
	existing := map[proto.DOMBackendNodeID]bool{}
	panels, err := noteActionPanelElements(page)
	if err != nil {
		return nil, err
	}
	for _, panel := range panels {
		visible, err := panel.Visible()
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		node, err := panel.Describe(0, false)
		if err != nil {
			return nil, err
		}
		existing[node.BackendNodeID] = true
	}
	for _, trigger := range triggers {
		// 每次点击前重新检查，绝不点击删除按钮及其子节点。
		deleted, err := trigger.Matches(`.note-card__action-btn--del, .note-card__action-btn--del *`)
		if err != nil {
			return nil, err
		}
		if deleted {
			continue
		}
		if err := card.Hover(); err != nil {
			return nil, err
		}
		if err := trigger.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return nil, err
		}
		if err := waitNoteVerification(page); err != nil {
			return nil, err
		}
		panel, opened, err := waitNotePermissionPanel(page, existing, icons)
		if err != nil {
			return nil, err
		}
		if panel != nil {
			return panel, nil
		}
		if len(opened) == 0 {
			return nil, fmt.Errorf("点击后未找到可识别的面板，无法安全继续尝试权限入口")
		}
		// 只有确认错误面板已关闭，才允许尝试下一个非删除按钮。
		for _, other := range opened {
			if err := closeNoteActionPanel(other); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("非删除操作中未找到笔记权限面板，请核对管理页选择器")
}

func newNoteActionPanels(page *rod.Page, existing map[proto.DOMBackendNodeID]bool) (rod.Elements, error) {
	panels, err := noteActionPanelElements(page)
	if err != nil {
		return nil, err
	}
	var visiblePanels rod.Elements
	for _, panel := range panels {
		visible, err := panel.Visible()
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		node, err := panel.Describe(0, false)
		if err != nil {
			return nil, err
		}
		if !existing[node.BackendNodeID] {
			visiblePanels = append(visiblePanels, panel)
		}
	}
	var result rod.Elements
	for i, panel := range visiblePanels {
		// 同一弹层的外壳和选项容器只算一个面板。
		nested := false
		for j, outer := range visiblePanels {
			if i == j {
				continue
			}
			contains, err := outer.ContainsElement(panel)
			if err != nil {
				return nil, err
			}
			nested = nested || contains
		}
		if nested {
			continue
		}
		result = append(result, panel)
	}
	return result, nil
}

func waitNotePermissionPanel(page *rod.Page, existing map[proto.DOMBackendNodeID]bool, icon bool) (*rod.Element, rod.Elements, error) {
	deadline := time.Now().Add(12 * time.Second)
	for {
		// 验证可能晚于第一次静默等待出现，不能把扫码层当成错误面板关闭。
		scan, err := observePublishScan(page)
		if err != nil {
			return nil, nil, err
		}
		if scan != nil {
			if err := waitNoteVerification(page); err != nil {
				return nil, nil, err
			}
			deadline = time.Now().Add(12 * time.Second)
			continue
		}
		panels, err := newNoteActionPanels(page, existing)
		if err != nil {
			return nil, nil, err
		}
		var candidates rod.Elements
		for _, panel := range panels {
			text, err := panel.Text()
			if err != nil {
				return nil, nil, err
			}
			if strings.Contains(text, "删除") {
				if err := closeNoteActionPanel(panel); err != nil {
					return nil, nil, fmt.Errorf("出现删除确认，取消失败，停止操作: %w", err)
				}
				return nil, nil, fmt.Errorf("出现删除确认，已取消并停止操作")
			}
			modal, err := isNotePermissionModal(panel)
			if err != nil {
				return nil, nil, err
			}
			if modal {
				candidates = append(candidates, panel)
				continue
			}
			options, err := panel.Elements(permissionOptions)
			if err != nil {
				return nil, nil, err
			}
			values := map[string]bool{}
			for _, option := range options {
				visible, err := option.Visible()
				if err != nil {
					return nil, nil, err
				}
				if !visible {
					continue
				}
				text, err := option.Text()
				if err != nil {
					return nil, nil, err
				}
				if value := visibilityLabel(text); value != "" {
					values[value] = true
				}
			}
			if len(values) >= 2 && (!icon || len(values) == 3) {
				candidates = append(candidates, panel)
			}
		}
		if len(candidates) > 1 {
			return nil, nil, fmt.Errorf("出现多个权限面板，无法确认目标笔记，拒绝修改")
		}
		if len(candidates) == 1 {
			return candidates[0], nil, nil
		}
		if time.Now().After(deadline) {
			return nil, panels, nil
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			return nil, nil, err
		}
	}
}

// 仅使用明确的取消、关闭按钮或 Escape，不点击保存、确认等操作。
func closeNoteActionPanel(panel *rod.Element) error {
	buttons, err := panel.Elements(permissionButtons + `, [aria-label="关闭"], [title="关闭"], .d-modal-close, .d-dialog-close, .el-dialog__headerbtn`)
	if err != nil {
		return err
	}
	var dismiss *rod.Element
	for _, button := range buttons {
		visible, err := button.Visible()
		if err != nil {
			return err
		}
		if !visible {
			continue
		}
		text, err := button.Text()
		if err != nil {
			return err
		}
		close, err := button.Matches(`[aria-label="关闭"], [title="关闭"], .d-modal-close, .d-dialog-close, .el-dialog__headerbtn`)
		if err != nil {
			return err
		}
		if text = strings.TrimSpace(text); text != "取消" && text != "关闭" && !(text == "" && close) {
			continue
		}
		if dismiss == nil || text == "取消" {
			dismiss = button
		}
		if text == "取消" {
			break
		}
	}
	if dismiss != nil {
		if err := dismiss.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return err
		}
	} else {
		if err := panel.Page().Keyboard.Type(input.Escape); err != nil {
			return err
		}
	}
	bounded := panel.Timeout(2 * time.Second)
	defer bounded.CancelTimeout()
	if err := bounded.WaitInvisible(); err != nil {
		return fmt.Errorf("无法确认操作面板已关闭，停止后续操作: %w", err)
	}
	return nil
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

func notePermissionTriggers(page *rod.Page, card *rod.Element) (triggers rod.Elements, icons bool, err error) {
	menus := `[role="menu"], .d-dropdown-menu, .dropdown-menu, .d-popover, .d-popover-content`
	existing := map[proto.DOMBackendNodeID]bool{}
	visibleMenus, err := page.Elements(menus)
	if err != nil {
		return nil, false, err
	}
	for _, menu := range visibleMenus {
		visible, err := menu.Visible()
		if err != nil {
			return nil, false, err
		}
		if visible {
			node, err := menu.Describe(0, false)
			if err != nil {
				return nil, false, err
			}
			existing[node.BackendNodeID] = true
		}
	}
	opened := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := card.Elements(`button, [role="button"], div, span, a`)
		if err != nil {
			return nil, false, err
		}
		entry, err := permissionEntry(entries)
		if err != nil {
			return nil, false, err
		}
		if entry != nil {
			return rod.Elements{entry}, false, nil
		}
		controls, err := card.Elements(".permission, .permission-setting, .permission-text")
		if err != nil {
			return nil, false, err
		}
		for _, control := range controls {
			text, err := control.Text()
			if err != nil {
				return nil, false, err
			}
			visible, err := control.Visible()
			if err != nil {
				return nil, false, err
			}
			if visible && visibilityLabel(text) != "" {
				return rod.Elements{control}, false, nil
			}
		}
		if opened {
			candidates, err := page.Elements(menus)
			if err != nil {
				return nil, false, err
			}
			for _, menu := range candidates {
				node, err := menu.Describe(0, false)
				if err != nil {
					return nil, false, err
				}
				if existing[node.BackendNodeID] {
					continue
				}
				entries, err := menu.Elements(`button, [role="menuitem"], div, span, a, li`)
				if err != nil {
					return nil, false, err
				}
				found, err := permissionEntry(entries)
				if err != nil {
					return nil, false, err
				}
				if found != nil {
					if entry != nil {
						a, err := entry.Describe(0, false)
						if err != nil {
							return nil, false, err
						}
						b, err := found.Describe(0, false)
						if err != nil {
							return nil, false, err
						}
						if a.BackendNodeID != b.BackendNodeID {
							return nil, false, fmt.Errorf("出现多个权限菜单，拒绝修改")
						}
					}
					entry = found
				}
			}
			if entry != nil {
				return rod.Elements{entry}, false, nil
			}
		} else {
			more, err := card.Elements(`button, [role="button"], [aria-haspopup="menu"], .more, .more-button, .more-icon, .note-card__more`)
			if err != nil {
				return nil, false, err
			}
			for _, el := range more {
				visible, err := el.Visible()
				if err != nil {
					return nil, false, err
				}
				if !visible {
					continue
				}
				text, err := el.Text()
				if err != nil {
					return nil, false, err
				}
				labels := []string{text}
				for _, attr := range []string{"aria-label", "title", "class", "aria-haspopup"} {
					value, err := el.Attribute(attr)
					if err != nil {
						return nil, false, err
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
					deleted, err := el.Matches(`.note-card__action-btn--del, .note-card__action-btn--del *`)
					if err != nil {
						return nil, false, err
					}
					if deleted {
						continue
					}
					if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return nil, false, err
					}
					opened = true
					break
				}
			}
		}
		if !opened {
			// 操作条按页面顺序排列，优先尝试第一个可见的非删除图标。
			icons, err := card.Elements(`.note-card__actions .note-card__action-btn:not(.note-card__action-btn--del)`)
			if err != nil {
				return nil, false, err
			}
			var visibleIcons rod.Elements
			for _, icon := range icons {
				visible, err := icon.Visible()
				if err != nil {
					return nil, false, err
				}
				if visible {
					visibleIcons = append(visibleIcons, icon)
				}
			}
			if len(visibleIcons) > 0 {
				return visibleIcons, true, nil
			}
		}
		if err := waitCreatorPoll(page.GetContext()); err != nil {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("目标笔记 hover 后仍没有可识别的权限设置入口，请核对管理页选择器")
}
