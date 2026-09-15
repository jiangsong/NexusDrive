package index

import (
	"sync"
	"time"
)

// budgetWindow is the fixed window the fetch budget is spent over. The
// window opens at the first Take and resets after an hour, matching the
// "<size>/h" the configuration is written in.
const budgetWindow = time.Hour

// Budget meters the bytes the indexer downloads from remotes per hour
// (docs/DESIGN.md §9, config.Index.FetchBudget). It is a fixed hourly
// window, not a sliding one: a refused Take says when the window turns
// over, and that is when the indexer sleeps until. Remotes reached through
// an unofficial API (provider.Caps.Tier == "unofficial") are charged double,
// which halves the budget for them without a second counter.
//
// A Budget is safe for concurrent use.
type Budget struct {
	perHour int64
	now     func() time.Time

	mu          sync.Mutex
	windowStart time.Time
	used        int64
}

// NewBudget builds a budget of perHour bytes per hour; perHour <= 0 means
// unlimited. now supplies the clock, nil meaning time.Now.
func NewBudget(perHour int64, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{perHour: perHour, now: now}
}

// Take spends n bytes (2n for an unofficial remote). It reports whether
// the spend fits the current window; when it does not, resumeAt is the
// instant the window resets and nothing is charged. A single request
// larger than the whole budget is granted when the window is untouched,
// so an oversized file is fetched once per hour rather than deferred for
// ever.
func (b *Budget) Take(n int64, unofficial bool) (ok bool, resumeAt time.Time) {
	cost := n
	if unofficial {
		cost *= 2
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollWindow()
	if b.perHour > 0 && b.used+cost > b.perHour && b.used > 0 {
		return false, b.windowStart.Add(budgetWindow)
	}
	b.used += cost
	return true, time.Time{}
}

// Used reports the bytes charged in the current window and the limit
// (0 when unlimited).
func (b *Budget) Used() (used, limit int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollWindow()
	return b.used, max(b.perHour, 0)
}

// rollWindow opens the window on first use and resets it once an hour has
// passed. Callers hold b.mu.
func (b *Budget) rollWindow() {
	t := b.now()
	if b.windowStart.IsZero() || t.Sub(b.windowStart) >= budgetWindow {
		b.windowStart = t
		b.used = 0
	}
}
