package upload

import (
	"sync"
	"time"

	"cloudfs/internal/provider"
)

// listingMemoTTL is how long a parent listing taken for a conflict check is
// reused by the conflict checks of its other files. Short on purpose: the
// memo exists for the burst of rewrites a copy makes into one directory,
// not as a cache of the backend.
const listingMemoTTL = 10 * time.Second

// listingMemo remembers, per directory, the listing the last conflict check
// fetched, keyed by entry name. Uploads that land update their own entry so
// a later rewrite of the same file compares against the version we just
// produced; a delete drops the directory so the next check looks again.
type listingMemo struct {
	mu   sync.Mutex
	dirs map[memoKey]*memoDir
}

type memoKey struct{ remote, parent string }

type memoDir struct {
	at      time.Time
	entries map[string]provider.Entry
}

// get answers for name in the directory when the memo is fresh: found says
// whether the name is in the listing, ok whether the memo could answer.
func (m *listingMemo) get(remote, parent, name string, now time.Time) (entry provider.Entry, found, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dirs[memoKey{remote, parent}]
	if d == nil || now.Sub(d.at) >= listingMemoTTL || now.Before(d.at) {
		return provider.Entry{}, false, false
	}
	entry, found = d.entries[name]
	return entry, found, true
}

func (m *listingMemo) remember(remote, parent string, entries []provider.Entry, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dirs == nil {
		m.dirs = map[memoKey]*memoDir{}
	}
	d := &memoDir{at: now, entries: make(map[string]provider.Entry, len(entries))}
	for _, e := range entries {
		d.entries[e.Name] = e
	}
	m.dirs[memoKey{remote, parent}] = d
}

// put records the entry an upload of ours just produced, when the
// directory is remembered at all. The memo's age is not refreshed: what it
// knows about the other names is as old as it was.
func (m *listingMemo) put(remote, parent string, e provider.Entry) {
	if e.Name == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if d := m.dirs[memoKey{remote, parent}]; d != nil {
		d.entries[e.Name] = e
	}
}

func (m *listingMemo) forget(remote, parent string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.dirs, memoKey{remote, parent})
}
