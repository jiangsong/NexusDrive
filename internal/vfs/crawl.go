package vfs

import (
	"context"
	"errors"
	"path"
	"sync"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// The crawler lists the directories nobody has opened, so the name index
// covers the whole volume the way Everything's does. It has no table of its
// own: a directory is "done" when dir_state says its listing is complete,
// which is the same fact readdir maintains, so a pass interrupted by a crash
// resumes from wherever the tree was, and a delta event that marks a
// directory stale puts it back on the list without anybody telling us.
//
// A pass is one monotonic sweep of meta.IncompleteDirs in ino order. New
// directories a listing creates get higher inos than the cursor, so the same
// sweep reaches them, and the sweep still terminates because the cursor only
// moves forward.

// CrawlOptions configures the crawler; CrawlOptionsFrom builds one from the
// config block. Zero durations take the defaults.
type CrawlOptions struct {
	// Enabled runs passes on the manager's own schedule. Off means the
	// manager only wakes for KickCrawl and costs nothing.
	Enabled bool
	// Remotes limits the crawl to these remote names; empty means all.
	Remotes []string
	// Exclude holds path.Match patterns tried against a directory's virtual
	// path and against its own name.
	Exclude []string
	// IdleAfter is how long foreground IO must be quiet before a scheduled
	// pass starts. Default 30s.
	IdleAfter time.Duration
	// Rescan is the pause between a finished pass and the next look for
	// stale directories. Default 5m.
	Rescan time.Duration
	// YieldMax bounds how long one directory listing stands aside for
	// foreground IO. Default prefetchYield.
	YieldMax time.Duration
	// RiskSleep is how long the crawler sleeps after provider.ErrRiskControl
	// before retrying the same directory. Default 15m.
	RiskSleep time.Duration
}

// CrawlOptionsFrom maps the config block onto crawler options.
func CrawlOptionsFrom(c config.SearchCrawl) CrawlOptions {
	return CrawlOptions{
		Enabled: c.Enabled, Remotes: c.Remotes, Exclude: c.Exclude,
		IdleAfter: c.IdleAfter, Rescan: c.Rescan,
	}
}

const (
	defaultCrawlIdleAfter = 30 * time.Second
	defaultCrawlRescan    = 5 * time.Minute
	defaultCrawlRiskSleep = 15 * time.Minute
	// crawlPage is how many incomplete directories one IncompleteDirs query
	// hands back.
	crawlPage = 256
	// crawlQuietPoll is how often the manager looks at fgIO while waiting
	// for the foreground to go quiet.
	crawlQuietPoll = 50 * time.Millisecond
)

func (o CrawlOptions) withDefaults() CrawlOptions {
	if o.IdleAfter <= 0 {
		o.IdleAfter = defaultCrawlIdleAfter
	}
	if o.Rescan <= 0 {
		o.Rescan = defaultCrawlRescan
	}
	if o.YieldMax <= 0 {
		o.YieldMax = prefetchYield
	}
	if o.RiskSleep <= 0 {
		o.RiskSleep = defaultCrawlRiskSleep
	}
	return o
}

// CrawlProgress is what `cloudfs status` and the cache screen show. The
// counters are cumulative for the life of the process, so a quiet pass that
// found nothing to do does not zero them.
type CrawlProgress struct {
	Running bool `json:"running"`
	// Listed counts directories listed since this process started.
	Listed int64 `json:"listed"`
	// Skipped counts directories passed over: above every mount, on a
	// remote the crawl does not cover, excluded by pattern, or not yet
	// on the remote at all.
	Skipped int64 `json:"skipped"`
	Failed  int64 `json:"failed"`
	Yields  int64 `json:"yields"`
	// Paused is "" | busy | risk_control.
	Paused   string    `json:"paused,omitempty"`
	ResumeAt time.Time `json:"resume_at,omitzero"`
	// Finished is the end of the last complete pass in this process.
	Finished time.Time `json:"finished,omitzero"`
}

// crawlState is the per-FS crawler state: the published progress, the pass
// lock, and the manager when StartCrawl installed one.
type crawlState struct {
	mu   sync.Mutex
	prog CrawlProgress
	// runMu serialises passes: a kicked pass and a synchronous CrawlOnce
	// must not sweep the same tree at once.
	runMu sync.Mutex

	mgrMu sync.Mutex
	mgr   *crawlManager
}

type crawlManager struct {
	opt    CrawlOptions
	cancel context.CancelFunc
	stop   chan struct{}
	kick   chan struct{}
	done   chan struct{}
}

// errCrawlStopped ends a pass when StopCrawl asked for it.
var errCrawlStopped = errors.New("vfs: crawl stopped")

// StartCrawl installs the manager goroutine. With opt.Enabled it runs a pass
// once the foreground has been quiet for IdleAfter, then again every Rescan;
// without it the manager only wakes for KickCrawl, which is what keeps a
// default configuration at zero provider calls. Calling it twice is a no-op.
func (f *FS) StartCrawl(ctx context.Context, opt CrawlOptions) {
	f.crawl.mgrMu.Lock()
	defer f.crawl.mgrMu.Unlock()
	if f.crawl.mgr != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &crawlManager{
		opt: opt.withDefaults(), cancel: cancel,
		stop: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}),
	}
	f.crawl.mgr = m
	go f.crawlLoop(ctx, m)
}

// StopCrawl ends the manager and any pass it is running, and waits for it.
func (f *FS) StopCrawl() {
	f.crawl.mgrMu.Lock()
	defer f.crawl.mgrMu.Unlock()
	m := f.crawl.mgr
	if m == nil {
		return
	}
	close(m.stop)
	m.cancel()
	<-m.done
	f.crawl.mgr = nil
}

// KickCrawl asks the manager for one pass now, whether or not scheduled
// passes are enabled. It reports false when there is no manager (a one-shot
// command opened the daemon with NoBackground), in which case the caller
// runs CrawlOnce itself.
func (f *FS) KickCrawl() bool {
	f.crawl.mgrMu.Lock()
	defer f.crawl.mgrMu.Unlock()
	if f.crawl.mgr == nil {
		return false
	}
	select {
	case f.crawl.mgr.kick <- struct{}{}:
	default:
	}
	return true
}

// CrawlProgress returns a snapshot of the crawler's counters.
func (f *FS) CrawlProgress() CrawlProgress {
	f.crawl.mu.Lock()
	defer f.crawl.mu.Unlock()
	return f.crawl.prog
}

func (f *FS) updateCrawl(fn func(p *CrawlProgress)) {
	f.crawl.mu.Lock()
	fn(&f.crawl.prog)
	f.crawl.mu.Unlock()
}

// CrawlOnce runs one synchronous pass with opt's filters; Enabled and the
// scheduling durations are ignored. `cloudfs warm --all` without a running
// daemon and the tests use it.
func (f *FS) CrawlOnce(ctx context.Context, opt CrawlOptions) (CrawlProgress, error) {
	return f.crawlPass(ctx, opt.withDefaults(), nil)
}

func (f *FS) crawlLoop(ctx context.Context, m *crawlManager) {
	defer close(m.done)
	// The first scheduled pass does not wait for a rescan interval, only
	// for the foreground to go quiet.
	scheduled := m.opt.Enabled
	for {
		kicked := false
		if !scheduled {
			var rescan <-chan time.Time
			var timer *time.Timer
			if m.opt.Enabled {
				timer = time.NewTimer(m.opt.Rescan)
				rescan = timer.C
			}
			stopped := false
			select {
			case <-m.kick:
				kicked = true
			case <-rescan:
			case <-m.stop:
				stopped = true
			case <-ctx.Done():
				stopped = true
			}
			if timer != nil {
				timer.Stop()
			}
			if stopped {
				return
			}
		}
		scheduled = false
		// A kick is a person asking; making them wait for a quiet mount
		// they may be the ones keeping busy would read as nothing happening.
		if !kicked && !f.crawlWaitQuiet(ctx, m) {
			return
		}
		if _, err := f.crawlPass(ctx, m.opt, m.stop); err != nil && (errors.Is(err, errCrawlStopped) || ctx.Err() != nil) {
			return
		}
	}
}

// crawlWaitQuiet blocks until fgIO has been zero for IdleAfter without a
// break. It reports false when the manager was stopped meanwhile.
func (f *FS) crawlWaitQuiet(ctx context.Context, m *crawlManager) bool {
	var quietSince time.Time
	for {
		now := time.Now()
		if f.fgIO.Load() == 0 {
			if quietSince.IsZero() {
				quietSince = now
			}
			if now.Sub(quietSince) >= m.opt.IdleAfter {
				return true
			}
		} else {
			quietSince = time.Time{}
		}
		select {
		case <-m.stop:
			return false
		case <-ctx.Done():
			return false
		case <-time.After(min(crawlQuietPoll, m.opt.IdleAfter)):
		}
	}
}

// crawlPass sweeps every incomplete directory once. Only a cancelled context
// or a stop ends it early; a directory that fails to list is counted and the
// sweep moves on, and risk control pauses the sweep rather than ending it.
func (f *FS) crawlPass(ctx context.Context, opt CrawlOptions, stop <-chan struct{}) (CrawlProgress, error) {
	f.crawl.runMu.Lock()
	defer f.crawl.runMu.Unlock()
	f.updateCrawl(func(p *CrawlProgress) { p.Running = true })
	err := f.crawlSweep(ctx, opt, stop)
	f.updateCrawl(func(p *CrawlProgress) {
		p.Running, p.Paused, p.ResumeAt = false, "", time.Time{}
		if err == nil {
			p.Finished = time.Now()
		}
	})
	return f.CrawlProgress(), err
}

func (f *FS) crawlSweep(ctx context.Context, opt CrawlOptions, stop <-chan struct{}) error {
	var allowed map[string]bool
	if len(opt.Remotes) > 0 {
		allowed = make(map[string]bool, len(opt.Remotes))
		for _, r := range opt.Remotes {
			allowed[r] = true
		}
	}
	for after := uint64(0); ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := f.meta.IncompleteDirs(ctx, after, crawlPage)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		after = page[len(page)-1].Ino
		// One serial worker per remote: the queue is split by the mount
		// that serves each directory and each remote lists its share in
		// order. An unofficial API (provider.TierUnofficial) must not see
		// more than one listing at a time from us, and this release keeps
		// official ones at the same fanout until it is aligned with the
		// prefetcher's; the split is what makes raising it a local change.
		queues := map[string][]meta.Node{}
		var skipped int64
		for _, n := range page {
			p, err := f.pathOf(ctx, n.Ino)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue // removed since the page was read
			}
			m, ok := f.mountFor(p)
			switch {
			case !ok, allowed != nil && !allowed[m.Remote], crawlExcluded(opt.Exclude, p):
				skipped++
			case n.RemoteID == "" && !f.isMountRoot(p):
				// Not on the remote yet (a local mkdir whose upload is
				// queued): nothing to list.
				skipped++
			default:
				queues[m.Remote] = append(queues[m.Remote], n)
			}
		}
		f.updateCrawl(func(p *CrawlProgress) { p.Skipped += skipped })
		var wg sync.WaitGroup
		errs := make(chan error, len(queues))
		for _, dirs := range queues {
			wg.Add(1)
			go func(dirs []meta.Node) {
				defer wg.Done()
				if err := f.crawlDirs(ctx, opt, stop, dirs); err != nil {
					errs <- err
				}
			}(dirs)
		}
		wg.Wait()
		close(errs)
		if err := <-errs; err != nil {
			return err
		}
	}
}

// crawlDirs lists dirs one after another, standing aside for foreground IO
// before each and sleeping through risk control. It returns only the errors
// that end a pass.
func (f *FS) crawlDirs(ctx context.Context, opt CrawlOptions, stop <-chan struct{}, dirs []meta.Node) error {
	for _, n := range dirs {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			select {
			case <-stop:
				return errCrawlStopped
			default:
			}
			if f.fgIO.Load() > 0 {
				f.updateCrawl(func(p *CrawlProgress) { p.Paused = "busy" })
				if f.yieldToForeground(stop, opt.YieldMax) {
					f.updateCrawl(func(p *CrawlProgress) { p.Yields++ })
				}
				f.updateCrawl(func(p *CrawlProgress) {
					if p.Paused == "busy" {
						p.Paused = ""
					}
				})
			}
			// want=false: the listing is committed to meta, which is all
			// the index needs; loading the children would only copy them.
			_, err := f.dirListing(ctx, n.Ino, false, false)
			switch {
			case err == nil:
				f.updateCrawl(func(p *CrawlProgress) { p.Listed++ })
			case errors.Is(err, provider.ErrRiskControl):
				// The account is being watched; every further call makes
				// it worse. Sleep, then retry this same directory, since
				// nothing about it was wrong.
				if err := f.crawlRiskSleep(ctx, opt, stop); err != nil {
					return err
				}
				continue
			case ctx.Err() != nil:
				return ctx.Err()
			case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotDir):
				// Gone or replaced since the page was read.
			default:
				f.updateCrawl(func(p *CrawlProgress) { p.Failed++ })
			}
			break
		}
	}
	return nil
}

// crawlRiskSleep pauses the pass for RiskSleep, publishing when it resumes.
// UNVERIFIED: whether a real quark risk-control response reaches here as
// provider.ErrRiskControl through the driver's error mapping, rather than as
// ErrTransient; the fake provider does. Check on a real account before
// trusting the pause.
func (f *FS) crawlRiskSleep(ctx context.Context, opt CrawlOptions, stop <-chan struct{}) error {
	resume := time.Now().Add(opt.RiskSleep)
	f.updateCrawl(func(p *CrawlProgress) { p.Paused, p.ResumeAt = "risk_control", resume })
	defer f.updateCrawl(func(p *CrawlProgress) {
		if p.Paused == "risk_control" {
			p.Paused, p.ResumeAt = "", time.Time{}
		}
	})
	t := time.NewTimer(opt.RiskSleep)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-stop:
		return errCrawlStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// crawlExcluded reports whether p matches an exclude pattern, tried against
// the whole virtual path and against the directory's own name.
func crawlExcluded(patterns []string, p string) bool {
	if len(patterns) == 0 {
		return false
	}
	base := path.Base(p)
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// yieldToForeground blocks while foreground reads or writes are in flight,
// for at most limit. It reports whether it waited at all. The prefetcher and
// the crawler share it: both spend a provider round trip and a metadata
// write transaction that the kernel's request is queued behind.
func (f *FS) yieldToForeground(stop <-chan struct{}, limit time.Duration) bool {
	if f.fgIO.Load() == 0 {
		return false
	}
	deadline := time.Now().Add(limit)
	// Backing off keeps a long wait from costing thousands of timer
	// wake-ups: starting background listing a few tens of milliseconds late
	// is free, and up to nine of these can be waiting at once.
	for wait := time.Millisecond; ; {
		if f.fgIO.Load() == 0 {
			return true
		}
		select {
		case <-stop:
			return true
		default:
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(wait)
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}
