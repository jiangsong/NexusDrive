package vfs

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrInvalidCursor is returned for a directory cursor this process did not
// issue, or one that has been tampered with.
var ErrInvalidCursor = errors.New("vfs: invalid directory cursor")

// ParseDirectoryCursor turns an opaque cursor from an earlier page into the
// options for the next one. Cursors issued now seek by name ("n:" plus the
// URL-safe base64 of the last name seen); a canonical decimal offset is still
// accepted for a client holding an older response.
//
// This lives in vfs so that every adapter paging a directory — the MCP tools,
// the control API a UI talks to — issues and accepts the same cursor bytes. A
// cursor from one surface that did not round-trip on another would be a
// puzzling bug to chase.
func ParseDirectoryCursor(cursor string, limit int) (DirectoryPageOptions, error) {
	opt := DirectoryPageOptions{Limit: limit, Count: true}
	if cursor == "" {
		return opt, nil
	}
	if len(cursor) > 8192 {
		return opt, ErrInvalidCursor
	}
	if encoded, ok := strings.CutPrefix(cursor, "n:"); ok {
		b, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(b) == 0 || len(b) > 4096 || !utf8.Valid(b) || strings.ContainsAny(string(b), "/\x00\\") || string(b) == "." || string(b) == ".." || base64.RawURLEncoding.EncodeToString(b) != encoded {
			return opt, ErrInvalidCursor
		}
		opt.After = string(b)
		return opt, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 || strconv.Itoa(n) != cursor {
		return opt, ErrInvalidCursor
	}
	opt.Offset = n
	return opt, nil
}

// NextDirectoryCursor is the cursor that continues after page, or "" when the
// page was the last one.
func NextDirectoryCursor(page DirectoryPage) string {
	if !page.HasMore || len(page.Entries) == 0 {
		return ""
	}
	return DirectoryCursorAfter(page.Entries[len(page.Entries)-1].Name)
}

// DirectoryCursorAfter returns the opaque cursor that resumes after name.
// Callers that apply a response budget after reading a page use it to avoid
// skipping entries they read but could not return.
func DirectoryCursorAfter(name string) string {
	return "n:" + base64.RawURLEncoding.EncodeToString([]byte(name))
}
