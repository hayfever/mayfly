package e2e

import "encoding/json"

// sessionListIDs decodes the ids of a session list response. The API returns
// {"sessions": [...]} today and moves to {"items": [...], "next_cursor": ...};
// the upgrade test must read either shape while that changes.
func sessionListIDs(body []byte) ([]string, error) {
	var list struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	items := list.Sessions
	if list.Items != nil {
		items = list.Items
	}
	ids := make([]string, 0, len(items))
	for _, s := range items {
		ids = append(ids, s.ID)
	}
	return ids, nil
}
