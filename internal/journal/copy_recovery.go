package journal

import (
	"context"
	"os"
	"path/filepath"
)

// copyRetention is resolved completely before any cleanup: a malformed intent
// must not cause us to delete bytes we failed to recognise. Submitted intents
// transfer ownership to their upload row; failed preparations retain payloads.
func (j *Journal) copyRetention(ctx context.Context, uploads []Upload) (map[string]bool, error) {
	jobs, err := j.CopyJobs(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, job := range jobs {
		if job.State == CopySubmitted {
			continue
		}
		p, err := j.copyPath(job.ID)
		if err != nil {
			return nil, err
		}
		live[p] = true
	}
	for _, u := range uploads {
		live[u.BlobPath] = true
	}
	return live, nil
}

func (j *Journal) cleanCopyOrphans(live map[string]bool) error {
	dir, err := filepath.Abs(filepath.Join(j.dir, "copies"))
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		p := filepath.Join(dir, entry.Name())
		if entry.IsDir() || live[p] {
			continue
		}
		// This is an exact entry in the private copy directory, never a
		// caller-supplied path. Remove does not follow symlink targets.
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}
