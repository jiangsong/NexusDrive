package journal

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"cloudfs/internal/provider"

	"github.com/google/uuid"
)

// sliceMD5Bytes is the prefix length Baidu hashes for its rapid-upload check.
const sliceMD5Bytes = 256 << 10

// preSHA1Bytes is the prefix length Aliyun hashes as pre_hash.
const preSHA1Bytes = 1 << 10

// Staging is an in-progress write. Bytes land in a temporary file; hashes are
// computed on the fly while the write is sequential, and recomputed from disk
// if the writer seeks backwards.
type Staging struct {
	immutable     bool
	mu            sync.Mutex
	reserveSpace  func(string, int64) (func(), error)
	releaseObject func()
	ID            string
	Path          string

	f    stagingFile
	size int64
	// pos is the next sequential offset; when a write lands elsewhere the
	// streaming hashes are abandoned and recomputed at commit.
	pos        int64
	streaming  bool
	full       map[provider.HashType]hash.Hash
	prefix     map[provider.HashType]hash.Hash
	prefixCaps map[provider.HashType]int64
	want       []provider.HashType
}

type stagingFile interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Truncate(int64) error
	Close() error
}

// NewStaging creates a staging file that will compute the given hashes.
func (j *Journal) NewStaging(want []provider.HashType) (*Staging, error) {
	id := uuid.NewString()
	p := filepath.Join(j.StagingDir(), id+".part")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: staging: %w", err)
	}
	return newStagingState(f, id, p, want, j.reserveSpace), nil
}

func newStagingState(f stagingFile, id, p string, want []provider.HashType, reserve func(string, int64) (func(), error)) *Staging {
	want = withCRC(want)
	s := &Staging{
		ID: id, Path: p, f: f, streaming: true, reserveSpace: reserve,
		full: map[provider.HashType]hash.Hash{}, prefix: map[provider.HashType]hash.Hash{},
		prefixCaps: map[provider.HashType]int64{}, want: want,
	}
	for _, ht := range want {
		switch ht {
		case provider.HashCRC32C:
			s.full[ht] = crc32.New(castagnoli)
		case provider.HashMD5:
			s.full[ht] = md5.New()
		case provider.HashSHA1:
			s.full[ht] = sha1.New()
		case provider.HashSHA256:
			s.full[ht] = sha256.New()
		case provider.HashSliceMD5:
			s.prefix[ht] = md5.New()
			s.prefixCaps[ht] = sliceMD5Bytes
		case provider.HashPreSHA1:
			s.prefix[ht] = sha1.New()
			s.prefixCaps[ht] = preSHA1Bytes
		}
	}
	return s
}

// AdoptStaging wraps an existing file as a staging object, used when a write
// handle starts from a downloaded copy of the remote file.
func (j *Journal) AdoptStaging(path string, want []provider.HashType) (*Staging, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: adopt staging: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("journal: adopt staging: %w", err)
	}
	return &Staging{
		ID: filepath.Base(path), Path: path, f: f, size: info.Size(), reserveSpace: j.reserveSpace,
		streaming: false, // content predates us: hash from disk at commit
		full:      map[provider.HashType]hash.Hash{}, prefix: map[provider.HashType]hash.Hash{},
		prefixCaps: map[provider.HashType]int64{}, want: withCRC(want),
	}, nil
}

// WriteAt writes at an offset, keeping streaming hashes valid only while the
// writes stay sequential.
func (s *Staging) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.immutable {
		return 0, fmt.Errorf("journal: immutable linked staging: %w", syscall.EPERM)
	}
	if off < 0 || int64(len(p)) > math.MaxInt64-off {
		return 0, fmt.Errorf("journal: invalid write range: %w", syscall.EINVAL)
	}
	// Reserve even overwrites: sparse holes and copy-on-write filesystems may
	// allocate new blocks without changing logical file size.
	release, err := s.reserve(int64(len(p)))
	if err != nil {
		return 0, err
	}
	defer release()
	n, err := s.f.WriteAt(p, off)
	if n == 0 {
		if err != nil {
			return 0, fmt.Errorf("journal: staging write: %w", err)
		}
		return 0, nil
	}
	// A short failing write can still change the file. Keep size and hashes
	// consistent with the accepted prefix before returning the syscall error.
	if off+int64(n) > s.size {
		s.size = off + int64(n)
	}
	if s.streaming && off == s.pos {
		for _, h := range s.full {
			h.Write(p[:n])
		}
		for ht, h := range s.prefix {
			cap := s.prefixCaps[ht]
			if s.pos < cap {
				take := int64(n)
				if s.pos+take > cap {
					take = cap - s.pos
				}
				h.Write(p[:take])
			}
		}
		s.pos += int64(n)
	} else {
		s.streaming = false
	}
	if err != nil {
		return n, fmt.Errorf("journal: staging write: %w", err)
	}
	return n, nil
}

func (s *Staging) reserve(n int64) (func(), error) {
	if n == 0 || s.reserveSpace == nil {
		return func() {}, nil
	}
	release, err := s.reserveSpace(filepath.Dir(s.Path), n)
	if err != nil {
		return nil, fmt.Errorf("journal: reserve write space: %w", err)
	}
	return release, nil
}

// ReadAt reads back what has been written, so a reader on a write handle sees
// its own data.
func (s *Staging) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.f.ReadAt(p, off)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("journal: staging read: %w", err)
	}
	return n, nil
}

// Truncate resizes the staging file.
func (s *Staging) Truncate(size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.immutable {
		return fmt.Errorf("journal: immutable linked staging: %w", syscall.EPERM)
	}
	if size < 0 {
		return fmt.Errorf("journal: negative truncate size: %w", syscall.EINVAL)
	}
	release, err := s.reserve(max(0, size-s.size))
	if err != nil {
		return err
	}
	defer release()
	if err := s.f.Truncate(size); err != nil {
		return fmt.Errorf("journal: staging truncate: %w", err)
	}
	s.size = size
	s.streaming = false
	return nil
}

// Size returns the current length.
func (s *Staging) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Hashes finalises the requested digests, rehashing from disk when the writer
// was not purely sequential.
func (s *Staging) Hashes() (provider.Hashes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := provider.Hashes{}
	if s.streaming && s.pos == s.size {
		for ht, h := range s.full {
			out[ht] = hex.EncodeToString(h.Sum(nil))
		}
		for ht, h := range s.prefix {
			out[ht] = hex.EncodeToString(h.Sum(nil))
		}
		return out, nil
	}
	return hashFile(s.f, s.size, s.want)
}

func hashFile(f io.ReaderAt, size int64, want []provider.HashType) (provider.Hashes, error) {
	out := provider.Hashes{}
	full := map[provider.HashType]hash.Hash{}
	prefix := map[provider.HashType]hash.Hash{}
	caps := map[provider.HashType]int64{}
	for _, ht := range want {
		switch ht {
		case provider.HashCRC32C:
			full[ht] = crc32.New(castagnoli)
		case provider.HashMD5:
			full[ht] = md5.New()
		case provider.HashSHA1:
			full[ht] = sha1.New()
		case provider.HashSHA256:
			full[ht] = sha256.New()
		case provider.HashSliceMD5:
			prefix[ht] = md5.New()
			caps[ht] = sliceMD5Bytes
		case provider.HashPreSHA1:
			prefix[ht] = sha1.New()
			caps[ht] = preSHA1Bytes
		}
	}
	buf := make([]byte, 1<<20)
	var pos int64
	for pos < size {
		n, err := f.ReadAt(buf, pos)
		if n > 0 {
			chunk := buf[:n]
			if pos+int64(n) > size {
				chunk = buf[:size-pos]
			}
			for _, h := range full {
				h.Write(chunk)
			}
			for ht, h := range prefix {
				c := caps[ht]
				if pos < c {
					take := int64(len(chunk))
					if pos+take > c {
						take = c - pos
					}
					h.Write(chunk[:take])
				}
			}
			pos += int64(len(chunk))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("journal: hash: %w", err)
		}
	}
	for ht, h := range full {
		out[ht] = hex.EncodeToString(h.Sum(nil))
	}
	for ht, h := range prefix {
		out[ht] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// withCRC adds the recovery checksum to the digests a staging file computes.
func withCRC(want []provider.HashType) []provider.HashType {
	for _, ht := range want {
		if ht == provider.HashCRC32C {
			return want
		}
	}
	return append(append([]provider.HashType{}, want...), provider.HashCRC32C)
}

// crc32cOf hashes a whole file the way staging does.
func crc32cOf(f *os.File) (string, error) {
	h := crc32.New(castagnoli)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Commit fsyncs the staging file (in power mode) and moves it into the
// objects directory under its content hash; the directory fsync that makes
// the rename durable is the committer's, once per group. It returns the
// object path.
// Pass s.ID as Upload.StagingID when committing its row. Until that commit
// succeeds (or staging is explicitly discarded), collection keeps the object.
func (j *Journal) CommitStaging(s *Staging, hashes provider.Hashes) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.durability != DurabilityCrash {
		if err := s.f.Sync(); err != nil {
			return "", fmt.Errorf("journal: fsync staging: %w", err)
		}
	}
	name := s.ID
	for _, ht := range []provider.HashType{provider.HashSHA1, provider.HashMD5, provider.HashSHA256} {
		if v, ok := hashes[ht]; ok && v != "" {
			name = string(ht) + "-" + v
			break
		}
	}
	dst := filepath.Join(j.ObjectsDir(), name)
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return "", err
	}
	j.objectMu.Lock()
	defer j.objectMu.Unlock()
	if err := s.f.Close(); err != nil {
		return "", fmt.Errorf("journal: close staging: %w", err)
	}
	if err := os.Rename(s.Path, dst); err != nil {
		return "", fmt.Errorf("journal: commit staging: %w", err)
	}
	if j.stagedObjects == nil {
		j.stagedObjects = map[string]string{}
		j.stagedRefs = map[string]int{}
	}
	j.stagedObjects[s.ID] = absDst
	j.stagedRefs[absDst]++
	s.releaseObject = func() { j.releaseStagedObject(s.ID, dst) }
	// POSIX rename is a no-op when source and destination are hard links to
	// the same inode. A cached copy can hit that case; consume only the source
	// alias, never the object itself or a different file which replaced it.
	srcAbs, _ := filepath.Abs(s.Path)
	dstAbs, _ := filepath.Abs(dst)
	if srcAbs != dstAbs {
		if source, err := os.Lstat(s.Path); err == nil && source.Mode().IsRegular() {
			if target, err := os.Stat(dst); err == nil && os.SameFile(source, target) {
				if err := os.Remove(s.Path); err != nil {
					return "", fmt.Errorf("journal: consume linked staging: %w", err)
				}
			}
		}
	}
	// The directory fsync that makes the rename durable is done once per
	// group by the committer, before any row pointing at the object exists.
	return dst, nil
}

// Discard removes an uncommitted staging file.
func (s *Staging) Discard() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Close()
	if s.releaseObject != nil {
		s.releaseObject()
		s.releaseObject = nil
	}
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("journal: discard staging: %w", err)
	}
	return nil
}

// Close releases the file descriptor without deleting the file.
func (s *Staging) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// NewID returns a fresh upload id.
func NewID() string { return uuid.NewString() }
