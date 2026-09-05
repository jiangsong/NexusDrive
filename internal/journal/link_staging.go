package journal

import (
	"errors"
	"os"
	"path/filepath"

	"cloudfs/internal/provider"
)

var ErrLinkSourceChanged = errors.New("journal: cache name changed while linking staging")

// LinkStaging snapshots an immutable open cache file without copying bytes.
// The caller must keep that file immutable and open until this call returns.
// The new link is private to the journal and only used for hashing/commit,
// never for WriteAt or Truncate. Callers may fall back to a streamed staging
// copy when links are unsupported or the cache name has changed.
func (j *Journal) LinkStaging(src *os.File, want []provider.HashType) (*Staging, error) {
	info, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("journal: linked source is not a regular file")
	}
	// Reject symlink names explicitly: platforms differ in whether Link
	// follows the source symlink. The post-link identity check below remains
	// necessary because the name can change after this inspection.
	named, err := os.Lstat(src.Name())
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() || !os.SameFile(info, named) {
		return nil, ErrLinkSourceChanged
	}
	p := filepath.Join(j.StagingDir(), NewID()+".part")
	if err := os.Link(src.Name(), p); err != nil {
		return nil, err
	}
	linked, err := os.Lstat(p)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(info, linked) || info.Size() != linked.Size() {
		os.Remove(p)
		return nil, ErrLinkSourceChanged
	}
	s, err := j.AdoptStaging(p, want)
	if err != nil {
		os.Remove(p)
		return nil, err
	}
	s.immutable = true
	return s, nil
}
