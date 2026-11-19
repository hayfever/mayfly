package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	// defaultPageLimit is the page size when the request has no limit.
	defaultPageLimit = 50
	// maxPageLimit caps a requested page size.
	maxPageLimit = 500
)

// page is the body of every list route. next_cursor is absent on the last
// page.
type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func writePage[T any](w http.ResponseWriter, items []T, next string) {
	writeJSON(w, http.StatusOK, page[T]{Items: items, NextCursor: next})
}

// pageQuery is the parsed limit and cursor of a list request.
type pageQuery struct {
	limit     int
	cursor    cursorKey
	hasCursor bool
}

// parsePage reads limit and cursor from the query string. A bad value is the
// caller's error, reported as invalid_request.
func parsePage(r *http.Request) (pageQuery, error) {
	out := pageQuery{limit: defaultPageLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxPageLimit {
			return pageQuery{}, fmt.Errorf("limit must be an integer from 1 to %d", maxPageLimit)
		}
		out.limit = limit
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		key, err := decodeCursor(raw)
		if err != nil {
			return pageQuery{}, errors.New("cursor is not valid")
		}
		out.cursor = key
		out.hasCursor = true
	}
	return out, nil
}

// cursorKey is the position of the last item on a page: the sort timestamp
// plus the id that breaks ties. Clients treat the encoded form as opaque.
type cursorKey struct {
	Time time.Time `json:"t"`
	ID   string    `json:"id"`
}

func encodeCursor(key cursorKey) string {
	raw, err := json.Marshal(key)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(value string) (cursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursorKey{}, err
	}
	var key cursorKey
	if err := json.Unmarshal(raw, &key); err != nil {
		return cursorKey{}, err
	}
	if key.ID == "" {
		return cursorKey{}, errors.New("cursor has no key")
	}
	return key, nil
}

// newerKey orders keys newest first, with the id descending as tie-break so
// every page boundary is stable.
func newerKey(a, b cursorKey) bool {
	if a.Time.Equal(b.Time) {
		return a.ID > b.ID
	}
	return a.Time.After(b.Time)
}

// pageOf returns the window of items that starts at start and the cursor for
// the next page. The cursor is empty on the last page.
func pageOf[T any](items []T, start, limit int, key func(T) cursorKey) ([]T, string) {
	end := min(start+limit, len(items))
	if end <= start {
		return items[start:start], ""
	}
	var next string
	if end < len(items) {
		next = encodeCursor(key(items[end-1]))
	}
	return items[start:end], next
}
