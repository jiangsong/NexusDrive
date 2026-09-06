package mcpsrv

import (
	"errors"

	"cloudfs/internal/vfs"
)

// directoryToolPage is vfs.ParseDirectoryCursor with the tool's error wording.
func directoryToolPage(cursor string, limit int) (vfs.DirectoryPageOptions, error) {
	opt, err := vfs.ParseDirectoryCursor(cursor, limit)
	if errors.Is(err, vfs.ErrInvalidCursor) {
		return opt, errors.New("invalid cursor")
	}
	return opt, err
}
