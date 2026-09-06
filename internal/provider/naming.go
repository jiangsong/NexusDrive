package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Naming is what a backend refuses in a file name. Every drive has rules
// of its own — Windows-derived reserved names on SMB, a forbidden set on
// OneDrive, case folding on most — and a name legal on one member of a
// pool may be refused on another. The pool asks here before placing a
// replica, so a refusal never becomes a failed upload. The zero value
// refuses nothing; a backend that does not describe itself is assumed
// permissive, and what it does refuse at run time is learnt then.
type Naming struct {
	// CaseInsensitive means two names differing only by case cannot
	// coexist in one directory.
	CaseInsensitive bool
	// MaxNameBytes is the longest name in bytes (0 = unknown).
	MaxNameBytes int
	// MaxPathBytes is the longest full path in bytes (0 = unknown).
	MaxPathBytes int
	// ForbiddenRunes lists characters that may not appear in a name.
	ForbiddenRunes string
	// ReservedNames lists names (compared without extension, case-folded)
	// the backend refuses: CON, PRN, AUX, NUL, COM1…
	ReservedNames []string
	// NoTrailingDotSpace refuses names ending in "." or " ".
	NoTrailingDotSpace bool
	// NoLeadingDot refuses hidden names.
	NoLeadingDot bool
}

// ErrBadName wraps ErrUnsupported: the backend cannot hold a name.
var ErrBadName = fmt.Errorf("%w: name refused by the backend's rules", ErrUnsupported)

// CheckName reports why a backend with these rules would refuse name, or
// nil when it would not.
func CheckName(n Naming, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if n.MaxNameBytes > 0 && len(name) > n.MaxNameBytes {
		return fmt.Errorf("%w: %q is longer than %d bytes", ErrBadName, name, n.MaxNameBytes)
	}
	if n.ForbiddenRunes != "" && strings.ContainsAny(name, n.ForbiddenRunes) {
		return fmt.Errorf("%w: %q contains one of %q", ErrBadName, name, n.ForbiddenRunes)
	}
	if n.NoTrailingDotSpace && (strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ")) {
		return fmt.Errorf("%w: %q ends in a dot or a space", ErrBadName, name)
	}
	if n.NoLeadingDot && strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: %q is hidden", ErrBadName, name)
	}
	if len(n.ReservedNames) > 0 {
		stem := name
		if i := strings.IndexByte(stem, '.'); i > 0 {
			stem = stem[:i]
		}
		for _, r := range n.ReservedNames {
			if strings.EqualFold(stem, r) {
				return fmt.Errorf("%w: %q is a reserved name", ErrBadName, name)
			}
		}
	}
	return nil
}

// WindowsReservedNames are the device names NTFS-derived backends refuse.
var WindowsReservedNames = []string{"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

// WindowsForbiddenRunes are the characters NTFS-derived backends refuse.
const WindowsForbiddenRunes = "<>:\"|?*\\"

// Quota is a backend's space, in bytes. Zero Total means unknown.
type Quota struct {
	Total int64
	Used  int64
}

// Free is what remains, or a negative number when Total is unknown.
func (q Quota) Free() int64 {
	if q.Total <= 0 {
		return -1
	}
	if q.Used > q.Total {
		return 0
	}
	return q.Total - q.Used
}

// Quotaer is implemented by backends that can report their space. A pool
// places by free space and reports the sum to df; a plain mount reports
// its drive's. Reach it through Unwrap: the instrumented wrapper does not
// forward it.
type Quotaer interface {
	Quota(ctx context.Context) (Quota, error)
}

// QuotaOf asks p for its space, through any instrumentation.
func QuotaOf(ctx context.Context, p Provider) (Quota, bool, error) {
	q, ok := Unwrap(p).(Quotaer)
	if !ok {
		return Quota{}, false, nil
	}
	quota, err := q.Quota(ctx)
	if err != nil {
		return Quota{}, true, err
	}
	return quota, true, nil
}

var _ = errors.New
