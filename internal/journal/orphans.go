package journal

import (
	"context"
	"os"
	"path/filepath"
)

// OrphanObjects reports the payloads on disk that no row in the queue names.
// It is read-only, so an inspection process may call it while another owns the
// queue.
//
// The expected answer is zero. Anything else is either a crash between a row
// being removed and its payload unlinked — which the next Recover clears — or
// content the queue has stopped reclaiming, which is invisible everywhere
// else: these bytes are not in the cache budget and not in the queued total,
// so nothing else would ever mention them.
func (j *Journal) OrphanObjects(ctx context.Context) (count int, bytes int64, err error) {
	all, err := j.list(ctx, ``)
	if err != nil {
		return 0, 0, err
	}
	live := map[string]bool{}
	for _, u := range all {
		if u.BlobPath != "" {
			live[filepath.Base(u.BlobPath)] = true
		}
	}
	copyLive, err := j.copyRetention(ctx, all)
	if err != nil {
		return 0, 0, err
	}
	for _, dir := range []string{j.ObjectsDir(), j.CopiesDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, 0, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if live[e.Name()] || copyLive[p] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			count++
			bytes += info.Size()
		}
	}
	return count, bytes, nil
}
