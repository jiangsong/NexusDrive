package vfs

import (
	"sync"
	"time"

	"cloudfs/internal/meta"
)

// feedCoverage records, per remote, the stretch of time over which the
// change feed has been polled without a gap: every event the backend saw
// since coveredSince has been applied to the tree, and the last poll was at
// lastOK. A complete directory listing taken inside that stretch is still
// the truth, however old — a change to it would have arrived as an event —
// so the directory TTL is not consulted for it while the feed stays live.
// The TTL remains the bound for a backend without a feed, and comes back
// into force as soon as a poll fails or the cursor is reset: from then on
// the tree cannot say what it missed. The instant coverage began is kept
// in the metadata store beside the cursor, so a restart continues the
// stretch instead of starting one: without that, every listing older than
// the TTL was fetched again after each restart, a round trip per directory.
type feedCoverage struct {
	mu       sync.Mutex
	interval time.Duration
	remotes  map[string]feedWindow
}

type feedWindow struct {
	coveredSince time.Time
	lastOK       time.Time
}

// feedLiveFor is how long after its last successful poll a feed is still
// taken to be live, in poll intervals: two missed ticks and the TTL is back.
const feedLiveFor = 2

// polled records the outcome of one poll of remote. now is when the poll
// began: events the backend produced up to then are applied once it
// returns, so a listing taken at or after it is covered from here on.
//
// When coverage starts in this process, stored is asked for the instant
// the previous process had covered from: the cursor is durable, so the
// first poll after a restart delivers everything that happened while the
// daemon was down, and the stretch continues rather than restarting. It
// reports the instant coverage now begins, and whether that is news for
// the caller to store — a start, a continuation across a restart, or a
// loss (the zero time).
func (c *feedCoverage) polled(remote string, now time.Time, interval time.Duration, ok bool, stored func() time.Time) (since time.Time, changed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remotes == nil {
		c.remotes = map[string]feedWindow{}
	}
	if !ok {
		_, had := c.remotes[remote]
		delete(c.remotes, remote)
		return time.Time{}, had
	}
	c.interval = interval
	w := c.remotes[remote]
	if w.coveredSince.IsZero() {
		if stored != nil {
			w.coveredSince = stored()
		}
		if w.coveredSince.IsZero() {
			w.coveredSince = now
		}
		changed = true
	}
	w.lastOK = now
	c.remotes[remote] = w
	return w.coveredSince, changed
}

// covers reports whether a complete listing of remote taken at st.ListedAt
// is still current at now on the strength of the feed alone.
func (c *feedCoverage) covers(remote string, st meta.DirState, now time.Time) bool {
	if !st.Complete {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.remotes[remote]
	if !ok || c.interval <= 0 {
		return false
	}
	// Truncated to the second, like the store keeps ListedAt: a listing
	// taken in the same second as the first covering poll counts.
	return !st.ListedAt.Before(w.coveredSince.Truncate(time.Second)) && now.Sub(w.lastOK) <= feedLiveFor*c.interval
}
