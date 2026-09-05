package mcpsrv

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"cloudfs/internal/vfs"
)

// Newly issued cursors seek by name. Strict canonical decimal offsets remain
// accepted for clients holding an old response, without allocating that prefix.
func directoryToolPage(cursor string, limit int) (vfs.DirectoryPageOptions, error) {
	opt := vfs.DirectoryPageOptions{Limit: limit, Count: true}
	if cursor == "" {
		return opt, nil
	}
	invalid := errors.New("invalid cursor")
	if len(cursor) > 8192 {
		return opt, invalid
	}
	if encoded, ok := strings.CutPrefix(cursor, "n:"); ok {
		b, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(b) == 0 || len(b) > 4096 || !utf8.Valid(b) || strings.ContainsAny(string(b), "/\x00\\") || string(b) == "." || string(b) == ".." || base64.RawURLEncoding.EncodeToString(b) != encoded {
			return opt, invalid
		}
		opt.After = string(b)
		return opt, nil
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 || strconv.Itoa(n) != cursor {
		return opt, invalid
	}
	opt.Offset = n
	return opt, nil
}
