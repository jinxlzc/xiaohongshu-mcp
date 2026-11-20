package xiaohongshu

import (
	"github.com/go-rod/rod"
	"net/url"
	"regexp"
	"strings"
)

const publishNoteManagerURL = "https://creator.xiaohongshu.com/new/note-manager"
const publishNoteCards = `.note-item, .note-card, .note-list-item, [data-note-id]`

var publishNoteIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)

func publishVisibleElement(page *rod.Page, selector string) (*rod.Element, error) {
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
func publishManagedNoteID(card *rod.Element) (string, error) {
	id, err := card.Attribute("data-note-id")
	if err != nil {
		return "", err
	}
	if id != nil && publishNoteIDPattern.MatchString(*id) {
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
		if len(parts) == 2 && (parts[0] == "explore" || parts[0] == "discovery") && publishNoteIDPattern.MatchString(parts[1]) {
			return parts[1], nil
		}
		if len(parts) == 3 && parts[0] == "discovery" && parts[1] == "item" && publishNoteIDPattern.MatchString(parts[2]) {
			return parts[2], nil
		}
		for _, key := range []string{"note_id", "noteId"} {
			if id := u.Query().Get(key); publishNoteIDPattern.MatchString(id) {
				return id, nil
			}
		}
	}
	return "", nil
}
