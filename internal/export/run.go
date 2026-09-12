package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// copyBuffer is the buffer a local copy uses when the filesystem has no
// copy-on-write clone to offer.
const copyBuffer = 1 << 20

// syncEvery is how many bytes go to the destination between fsyncs. Often
// enough that a power loss costs seconds, rarely enough that it does not
// serialise the drive.
const syncEvery = 256 << 20

// yieldWait is how long a chunk waits for the kernel's own readers and
// writers to finish before going ahead anyway.
const yieldWait = 5 * time.Second

// errBindingChanged marks a source whose mount no longer matches what the
// job recorded — a re-pointed prefix or a rebound account. Continuing would
// copy different content under the same plan.
var errBindingChanged = errors.New("export: the source mount or account binding changed")

// errHashMismatch marks content that arrived wrong.
var errHashMismatch = errors.New("export: content hash mismatch")

// diskError marks a failure of the destination rather than of the source. It
// pauses the job instead of failing the file: an unplugged drive is a thing
// the user fixes by plugging it back in.
type diskError struct{ err error }

func (e diskError) Error() string { return "export: destination: " + e.err.Error() }
func (e diskError) Unwrap() error { return e.err }

// isDiskFault reports the errors that mean "this destination cannot take the
// bytes right now": full, quota, read-only, or physically gone.
func isDiskFault(err error) bool {
	for _, e := range []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT, syscall.EIO, syscall.ENXIO, syscall.ENODEV, syscall.EROFS} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// asDisk wraps a local IO error as a destination fault when it is one.
func asDisk(err error) error {
	if err == nil {
		return nil
	}
	if isDiskFault(err) {
		return diskError{err}
	}
	return err
}

// checkDest confirms the destination is still the filesystem the plan was
// made against. Without it, a pulled drive whose mount point is an ordinary
// directory again would quietly re-download the whole export onto the root
// disk.
func (m *Manager) checkDest(job Job) error {
	st, err := os.Stat(job.Dest)
	if err != nil {
		return diskError{err}
	}
	if !st.IsDir() {
		return diskError{syscall.ENOTDIR}
	}
	dev, ok := deviceOf(st)
	if ok && job.DestDev != 0 && dev != job.DestDev {
		return diskError{fmt.Errorf("%w: the destination is on a different filesystem than when the job was planned", syscall.ENODEV)}
	}
	return nil
}

// mountFor resolves the mount serving a virtual path, the way the VFS does:
// deepest prefix wins, and vfs.FS.Mounts is already ordered that way.
func (m *Manager) mountFor(p string) (vfs.Mount, bool) {
	p = path.Clean("/" + strings.Trim(p, "/"))
	for _, mt := range m.fs.Mounts() {
		if mt.Prefix == "/" || p == mt.Prefix || strings.HasPrefix(p, mt.Prefix+"/") {
			return mt, true
		}
	}
	return vfs.Mount{}, false
}

// sourceMount resolves the item's mount and checks it against the binding the
// job recorded when it was planned.
func (m *Manager) sourceMount(job Job, it Item) (vfs.Mount, error) {
	mt, ok := m.mountFor(it.VPath)
	if !ok || mt.Remote != it.Remote {
		return vfs.Mount{}, errBindingChanged
	}
	for _, b := range job.Bindings {
		if b.Prefix != mt.Prefix {
			continue
		}
		if b.Remote != mt.Remote || b.RootID != mt.RootID || b.AccountBinding != mt.AccountBinding {
			return vfs.Mount{}, errBindingChanged
		}
		return mt, nil
	}
	return vfs.Mount{}, errBindingChanged
}

// exportFile puts one planned file on the destination.
func (m *Manager) exportFile(ctx context.Context, job Job, it Item) error {
	if err := m.checkDest(job); err != nil {
		return err
	}
	dest := filepath.Join(job.Dest, filepath.FromSlash(it.Rel))
	part := dest + partSuffix
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return asDisk(err)
	}
	local, err := m.copyLocal(ctx, it, part)
	if err != nil {
		return err
	}
	if !local {
		if err := m.fetch(ctx, job, it, part); err != nil {
			return err
		}
	}
	return m.publish(job, it, part, dest)
}

// fetchedBytes is how much of the file the bitmap already accounts for: the
// progress a resumed transfer starts from.
func fetchedBytes(b bitmap, chunks int, size, rangeSize int64) int64 {
	done := int64(0)
	for i := 0; i < chunks; i++ {
		if !b.has(i) {
			continue
		}
		off := int64(i) * rangeSize
		n := rangeSize
		if off+n > size {
			n = size - off
		}
		done += n
	}
	return done
}

// copyLocal serves the file from bytes this machine already has: a hydrated
// cache object, or the journal blob of a file that has not been uploaded yet.
// It reports false when neither applies.
//
// It copies. Linking or adopting the cache object into the destination would
// hand the user's drive an inode the cache believes it owns, and an edit made
// on the drive would rewrite the cached content of a file nobody touched.
func (m *Manager) copyLocal(ctx context.Context, it Item, part string) (bool, error) {
	if id, ok := vfs.LocalUploadID(it.RemoteID); ok {
		j := m.fs.Journal()
		if j == nil {
			return false, fmt.Errorf("%w: the file is only in the write queue, which is not open", provider.ErrNotFound)
		}
		u, err := j.Get(ctx, id)
		if err != nil {
			return false, fmt.Errorf("%w: the queued write is gone", provider.ErrNotFound)
		}
		if u.Size != it.Size {
			return false, fmt.Errorf("%w: the queued write changed size", provider.ErrConflict)
		}
		return true, m.copyPath(u.BlobPath, part)
	}
	ca := m.fs.Cache()
	if ca == nil {
		return false, nil
	}
	wf, err := ca.OpenWhole(cache.FileKey{Remote: it.Remote, RemoteID: it.RemoteID, Version: it.Version})
	if err != nil {
		return false, nil
	}
	defer wf.Close()
	st, err := wf.Stat()
	if err != nil || st.Size() != it.Size {
		return false, nil
	}
	return true, m.copyPath(wf.Name(), part)
}

// copyPath duplicates a local file, preferring a copy-on-write clone. The
// clone is a separate inode that happens to share blocks, so a later edit on
// the destination is still copy-on-write away from the source.
func (m *Manager) copyPath(src, part string) error {
	if err := cloneFile(src, part); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return asDisk(err)
	}
	buf := make([]byte, copyBuffer)
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		out.Close()
		return asDisk(err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return asDisk(err)
	}
	return asDisk(out.Close())
}

// publish turns a finished part file into the exported file.
func (m *Manager) publish(job Job, it Item, part, dest string) error {
	f, err := os.OpenFile(part, os.O_WRONLY, 0o644)
	if err != nil {
		return asDisk(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return asDisk(err)
	}
	if err := f.Close(); err != nil {
		return asDisk(err)
	}
	if err := os.Rename(part, dest); err != nil {
		return asDisk(err)
	}
	if job.Options.PreserveMTime && !it.MTime.IsZero() {
		if err := os.Chtimes(dest, it.MTime, it.MTime); err != nil {
			return asDisk(err)
		}
	}
	if !job.Options.Verify {
		return nil
	}
	// Read the file back off the destination. This is what catches a drive
	// that accepted the write and stored something else.
	st, err := os.Stat(dest)
	if err != nil {
		return asDisk(err)
	}
	if st.Size() != it.Size {
		return fmt.Errorf("%w: the destination holds %d of %d bytes", errHashMismatch, st.Size(), it.Size)
	}
	if it.HashType == "" || it.Hash == "" {
		return nil
	}
	sum, err := hashFile(dest, it.HashType)
	if errors.Is(err, errUnknownHash) {
		return nil
	}
	if err != nil {
		return asDisk(err)
	}
	if sum != it.Hash {
		return fmt.Errorf("%w: %s on the destination is %s, the source reports %s", errHashMismatch, it.HashType, sum, it.Hash)
	}
	return nil
}

// yield stands aside while the kernel is waiting on the mount. An export is
// background work by definition; a user copying a file out should not make
// their own `ls` slow.
func (m *Manager) yield(ctx context.Context) {
	if !m.cfg.YieldToForeground {
		return
	}
	busy := m.busyFn()
	if busy == nil {
		return
	}
	deadline := m.now().Add(yieldWait)
	for busy() && m.now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// chunkWork is one range of one file.
type chunkWork struct {
	index int
	off   int64
	n     int64
}

// fileWriter serialises the bookkeeping several streams share.
type fileWriter struct {
	mu sync.Mutex
	f  *os.File
	// bits is the durable record of which chunks are on the disk.
	bits      bitmap
	chunks    int
	doneBytes int64
	sinceSync int64
	m         *Manager
	job       Job
	it        Item
}

func (w *fileWriter) writeAt(buf []byte, off int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m.writeFault != nil {
		if err := w.m.writeFault(off); err != nil {
			return asDisk(err)
		}
	}
	if _, err := w.f.WriteAt(buf, off); err != nil {
		return asDisk(err)
	}
	return nil
}

// complete records one finished chunk, fsyncing at intervals so that what the
// bitmap claims is durable really is.
func (w *fileWriter) complete(ctx context.Context, c chunkWork) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sinceSync += c.n
	if w.sinceSync >= syncEvery {
		if err := w.f.Sync(); err != nil {
			return asDisk(err)
		}
		w.sinceSync = 0
	}
	w.bits.set(c.index)
	w.doneBytes += c.n
	return w.m.store.Checkpoint(ctx, w.job.ID, w.it.Rel, w.bits.String(), w.doneBytes)
}
