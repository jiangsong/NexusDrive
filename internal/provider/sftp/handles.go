package sftp

import (
	"sync"
	"sync/atomic"
	"time"

	psftp "github.com/pkg/sftp"
)

// Remote handle reuse.
//
// SFTP reads are addressed by handle, and a handle costs an OPEN round trip
// to get and a CLOSE to give back. Opening the file for every range turned a
// 4 KiB random read into four round trips, and made a cold 4 MiB block a
// serial OPEN, 128 READs, CLOSE. sshfs keeps the handle for as long as the
// application keeps the file open; this cache keeps it a little longer, keyed
// by path and version, so a random reader and the read-ahead behind it share
// one handle and each miss is a single READ.
//
// A handle pins the inode it was opened on: after our own upload replaces
// the path, a cached handle would keep reading the old content. Every
// mutation of a path therefore evicts its handles first, and the version in
// the key catches changes made by anyone else.

const (
	handleCacheMax  = 32
	handleCacheIdle = 45 * time.Second
)

type handleKey struct {
	path    string
	version string
	cli     *psftp.Client
}

type cachedHandle struct {
	key      handleKey
	f        *psftp.File
	leases   int
	lastUsed time.Time
	// dead marks a handle that must close once its last lease is returned:
	// evicted, superseded, or belonging to a connection that is gone.
	dead bool
}

type handleCache struct {
	mu  sync.Mutex
	m   map[handleKey]*cachedHandle
	now func() time.Time
	// opens and closes count what the cache did, for tests and status.
	opens  atomic.Int64
	closes atomic.Int64
}

func newHandleCache() *handleCache {
	return &handleCache{m: map[handleKey]*cachedHandle{}, now: time.Now}
}

// lease returns the cached handle for key, or nil. A leased handle is not
// closed until release.
func (h *handleCache) lease(key handleKey) *cachedHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	ch := h.m[key]
	if ch == nil || ch.dead {
		return nil
	}
	ch.leases++
	ch.lastUsed = h.now()
	return ch
}

// add caches a freshly opened handle and leases it. If another caller won
// the race the newer file is closed and the existing handle leased instead.
func (h *handleCache) add(key handleKey, f *psftp.File) *cachedHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opens.Add(1)
	if old := h.m[key]; old != nil && !old.dead {
		old.leases++
		old.lastUsed = h.now()
		h.mu.Unlock()
		f.Close()
		h.closes.Add(1)
		h.mu.Lock()
		return old
	}
	if len(h.m) >= handleCacheMax {
		h.evictOldestLocked()
	}
	ch := &cachedHandle{key: key, f: f, leases: 1, lastUsed: h.now()}
	h.m[key] = ch
	return ch
}

// release returns a lease; a handle marked dead closes on its last one.
func (h *handleCache) release(ch *cachedHandle) {
	h.mu.Lock()
	ch.leases--
	closeNow := ch.dead && ch.leases == 0
	h.mu.Unlock()
	if closeNow {
		ch.f.Close()
		h.closes.Add(1)
	}
}

// evictPath drops every handle on path, whatever its version: the caller is
// about to replace, move or delete what the handles point at.
func (h *handleCache) evictPath(path string) {
	h.mu.Lock()
	var toClose []*cachedHandle
	for k, ch := range h.m {
		if k.path != path {
			continue
		}
		delete(h.m, k)
		ch.dead = true
		if ch.leases == 0 {
			toClose = append(toClose, ch)
		}
	}
	h.mu.Unlock()
	for _, ch := range toClose {
		ch.f.Close()
		h.closes.Add(1)
	}
}

// purge drops everything; the connection under the handles is gone, so the
// files are not closed over the wire, only forgotten.
func (h *handleCache) purge() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, ch := range h.m {
		ch.dead = true
		delete(h.m, k)
	}
}

// closeAll closes every unleased handle; used on provider shutdown.
func (h *handleCache) closeAll() {
	h.mu.Lock()
	var toClose []*cachedHandle
	for k, ch := range h.m {
		ch.dead = true
		delete(h.m, k)
		if ch.leases == 0 {
			toClose = append(toClose, ch)
		}
	}
	h.mu.Unlock()
	for _, ch := range toClose {
		ch.f.Close()
		h.closes.Add(1)
	}
}

func (h *handleCache) sweepLocked() {
	cutoff := h.now().Add(-handleCacheIdle)
	for k, ch := range h.m {
		if ch.leases == 0 && ch.lastUsed.Before(cutoff) {
			delete(h.m, k)
			ch.f.Close()
			h.closes.Add(1)
		}
	}
}

func (h *handleCache) evictOldestLocked() {
	var oldest *cachedHandle
	for _, ch := range h.m {
		if ch.leases != 0 {
			continue
		}
		if oldest == nil || ch.lastUsed.Before(oldest.lastUsed) {
			oldest = ch
		}
	}
	if oldest == nil {
		return
	}
	delete(h.m, oldest.key)
	oldest.f.Close()
	h.closes.Add(1)
}

// Stats reports cache activity: handles opened and closed so far.
func (h *handleCache) stats() (opens, closes int64) { return h.opens.Load(), h.closes.Load() }
