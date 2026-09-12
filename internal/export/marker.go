package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// marker is `.cloudfs-export.json` in the destination directory. It is what
// makes a mirror safe: a mirror deletes everything in the destination that is
// not in the plan, so it is only allowed where a previous export of the same
// sources left its marker behind. Without that rule, naming any directory as
// the destination of a mirror would empty it.
type marker struct {
	Sources      []string  `json:"sources"`
	JobID        string    `json:"job_id"`
	MetaIdentity string    `json:"meta_identity"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func markerPath(dest string) string { return filepath.Join(dest, markerName) }

func readMarker(dest string) (marker, error) {
	b, err := os.ReadFile(markerPath(dest))
	if err != nil {
		return marker{}, err
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil {
		return marker{}, err
	}
	return m, nil
}

func writeMarker(dest string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := markerPath(dest) + partSuffix
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, markerPath(dest))
}

// sameSources compares two source sets regardless of the order they were
// given in.
func sameSources(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
