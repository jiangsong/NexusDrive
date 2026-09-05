// Package upload drains the journal: it tries a hash-only (rapid) upload
// first, falls back to resumable chunked upload, detects remote changes and
// writes a conflict copy instead of overwriting, and dead-letters what cannot
// succeed (docs/DESIGN.md §4.5).
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// Result reports what happened to one upload.
type Result struct {
	UploadID string
	Entry    provider.Entry
	// Rapid is true when the provider accepted the file by hash alone.
	Rapid bool
	// ConflictName is set when the remote changed under us and the data was
	// stored under a different name.
	ConflictName string
}

// Hooks let the VFS react to upload outcomes without importing it here.
type Hooks struct {
	// Authorize verifies the durable local database/mount/account generation
	// before any provider is resolved or called. A failure is retained as a
	// dead letter and must never be treated as a retryable remote error.
	Authorize func(ctx context.Context, u journal.Upload) error
	// OnSuccess updates the metadata tree and adopts the blob into the read
	// cache. Returning an error dead-letters the upload.
	OnSuccess func(ctx context.Context, u journal.Upload, r Result) error
	// OnDead is called when an upload is dead-lettered.
	OnDead func(ctx context.Context, u journal.Upload, cause error)
	// RemoteVersion, when set, returns the remote version this node was
	// last known to have — updated by our own earlier uploads — so that a
	// version we produced ourselves is not mistaken for someone else's
	// edit. Without it, two quick saves of one file produced a conflict
	// copy whenever the first upload landed between the second's open and
	// its send.
	RemoteVersion func(ctx context.Context, u journal.Upload) (string, bool)

	// Exists, when set, reports whether the file an upload carries is still
	// present locally. An upload that fails because the file or its
	// directory is gone from the backend, and gone here too, is finished
	// rather than failed: there is nothing left to send and nothing for an
	// operator to fix.
	Exists func(ctx context.Context, u journal.Upload) bool
}

// Options configures an Uploader.
type Options struct {
	Journal *journal.Journal
	// Providers resolves a remote name to its backend.
	Providers func(remote string) (provider.Provider, bool)
	// Limiters is optional; when set the uploader waits on the upload bucket
	// and reports throttling so the AIMD controller adapts.
	Limiters *ratelimit.Registry
	// Policy governs in-call retries. The journal handles longer backoff.
	Policy retry.Policy
	// MaxAttempts dead-letters an upload after this many journal attempts.
	MaxAttempts int
	// Workers is the per-remote concurrency. WorkersFor, when set, overrides
	// it per remote — the daemon uses it to follow the backend's own
	// UploadParallel recommendation.
	Workers    int
	WorkersFor func(remote string) int
	// PollInterval is how often idle workers re-check the queue.
	PollInterval time.Duration
	Hooks        Hooks
	Now          func() time.Time
	// Clock hook for tests that need deterministic backoff.
	Backoff retry.Backoff
}

// Uploader runs per-remote worker pools over the journal.
type Uploader struct {
	opt        Options
	transferMu sync.Mutex
	transfers  map[string]context.CancelFunc

	mu      sync.Mutex
	running map[string]context.CancelFunc
	// cancelAll stops the supervisor and every worker. Stop must be able to
	// return without the caller cancelling the context it passed to Start.
	cancelAll context.CancelFunc
	runCtx    context.Context
	wg        sync.WaitGroup
	stopped   bool
}

// New builds an Uploader.
func New(opt Options) (*Uploader, error) {
	if opt.Journal == nil {
		return nil, errors.New("upload: Journal is required")
	}
	if opt.Providers == nil {
		return nil, errors.New("upload: Providers is required")
	}
	if opt.Workers <= 0 {
		opt.Workers = 2
	}
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = 8
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = time.Second
	}
	if opt.Policy.MaxAttempts == 0 {
		opt.Policy = retry.Policy{Backoff: retry.DefaultBackoff, MaxAttempts: 3}
	}
	if opt.Backoff.Max == 0 {
		opt.Backoff = retry.DefaultBackoff
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Uploader{opt: opt, running: map[string]context.CancelFunc{}}, nil
}

// Start launches worker pools for every remote with queued work and keeps
// watching for new remotes. Stop shuts everything down without needing the
// caller to cancel ctx.
func (u *Uploader) Start(ctx context.Context) {
	u.mu.Lock()
	if u.stopped || u.cancelAll != nil {
		u.mu.Unlock()
		return
	}
	rootCtx, cancel := context.WithCancel(ctx)
	u.cancelAll = cancel
	u.runCtx = rootCtx
	u.wg.Add(1)
	u.mu.Unlock()

	go func() {
		defer u.wg.Done()
		t := time.NewTicker(u.opt.PollInterval)
		defer t.Stop()
		for {
			u.syncRemotes(rootCtx)
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (u *Uploader) syncRemotes(ctx context.Context) {
	remotes, err := u.opt.Journal.Remotes(ctx)
	if err != nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopped {
		return
	}
	for _, r := range remotes {
		if _, ok := u.running[r]; ok {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		u.running[r] = cancel
		workers := u.opt.Workers
		if u.opt.WorkersFor != nil {
			if n := u.opt.WorkersFor(r); n > 0 {
				workers = n
			}
		}
		for i := 0; i < workers; i++ {
			u.wg.Add(1)
			go func(remote string) {
				defer u.wg.Done()
				u.worker(rctx, remote)
			}(r)
		}
	}
}

// Stop cancels the supervisor and every worker, then waits for them.
func (u *Uploader) Stop() {
	u.mu.Lock()
	if u.stopped {
		u.mu.Unlock()
		u.wg.Wait()
		return
	}
	u.stopped = true
	for _, cancel := range u.running {
		cancel()
	}
	u.running = map[string]context.CancelFunc{}
	if u.cancelAll != nil {
		u.cancelAll()
		u.cancelAll = nil
	}
	u.mu.Unlock()
	u.wg.Wait()
}

func (u *Uploader) worker(ctx context.Context, remote string) {
	t := time.NewTicker(u.opt.PollInterval)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := u.drainOnce(ctx, remote)
		if err == nil && n > 0 {
			continue // more work may be due immediately
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (u *Uploader) drainOnce(ctx context.Context, remote string) (int, error) {
	claimed, err := u.opt.Journal.Claim(ctx, remote, 1)
	if err != nil || len(claimed) == 0 {
		return 0, err
	}
	for _, up := range claimed {
		u.process(ctx, up)
	}
	return len(claimed), nil
}

// DrainOnce processes at most one due upload for a remote. Tests and
// `cloudfs uploads flush` use it to run the pipeline synchronously.
func (u *Uploader) DrainOnce(ctx context.Context, remote string) (int, error) {
	return u.drainOnce(ctx, remote)
}

// DrainAll repeatedly drains every remote until nothing is due. It returns the
// number of uploads processed.
func (u *Uploader) DrainAll(ctx context.Context) (int, error) {
	total := 0
	for {
		remotes, err := u.opt.Journal.Remotes(ctx)
		if err != nil {
			return total, err
		}
		progressed := false
		for _, r := range remotes {
			n, err := u.drainOnce(ctx, r)
			if err != nil {
				return total, err
			}
			if n > 0 {
				progressed = true
				total += n
			}
		}
		if !progressed {
			return total, nil
		}
	}
}

// process runs one upload to completion, retry or dead letter.
// errWaitingForOlder is recorded when an upload steps aside for an earlier
// version of the same file. It is not a failure and does not count attempts.
var errWaitingForOlder = errors.New("upload: waiting for an earlier version of this file")

func (u *Uploader) process(ctx context.Context, up journal.Upload) {
	ctx, release, ok := u.registerTransfer(ctx, up)
	if !ok {
		return
	}
	defer release()
	if u.opt.Hooks.Authorize != nil {
		if err := u.opt.Hooks.Authorize(ctx, up); err != nil {
			u.dead(ctx, up, fmt.Errorf("upload: local account binding rejected this task: %w", err))
			return
		}
	}
	// A newer version of this file has been committed since: sending this
	// one would either be overwritten immediately or, worse, land after the
	// newer one and leave the backend holding the older content.
	if done, err := u.opt.Journal.Superseded(ctx, up); err == nil && done {
		_ = u.opt.Journal.Succeed(ctx, up.ID)
		return
	}
	// An earlier version of the same file is still going out. Wait for it:
	// it is about to change the very version this upload's conflict check
	// compares against, and racing it turns our own rewrite into someone
	// else's conflicting edit.
	if busy, err := u.opt.Journal.OlderInFlight(ctx, up); err == nil && busy {
		_ = u.opt.Journal.Defer(ctx, up.ID, 20*time.Millisecond, errWaitingForOlder.Error())
		return
	}
	p, ok := u.opt.Providers(up.Remote)
	if !ok {
		u.dead(ctx, up, fmt.Errorf("upload: no provider for remote %q", up.Remote))
		return
	}
	res, err := u.runOne(ctx, p, up)
	if err != nil && errors.Is(err, errSessionExpired) {
		// The resumed session is no longer valid server-side. Drop it and the
		// recorded parts so the next attempt starts a fresh upload.
		_ = u.opt.Journal.SetSession(ctx, up.ID, nil)
		for _, pt := range mustParts(ctx, u.opt.Journal, up.ID) {
			_ = u.opt.Journal.RecordPart(ctx, up.ID, journal.Part{Index: pt.Index, State: "stale"})
		}
		_ = u.opt.Journal.Retry(ctx, up.ID, err, 0)
		return
	}
	if err == nil {
		// Serialize cancellation with final publication. Whichever wins this
		// lock defines whether the task is completed or retained as cancelled.
		u.transferMu.Lock()
		defer u.transferMu.Unlock()
		check, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		row, gerr := u.opt.Journal.Get(check, up.ID)
		stop()
		if gerr != nil || row.State == journal.StateCancelling || row.State == journal.StateCancelled || row.State == journal.StatePurging {
			return
		}
		// Deleted locally while we were sending it: the file must not come
		// back to life on the backend.
		if row, gerr := u.opt.Journal.Get(ctx, up.ID); gerr == nil && row.Tombstone {
			if res.Entry.ID != "" {
				if derr := p.Delete(ctx, res.Entry.ID); derr != nil && !errors.Is(derr, provider.ErrNotFound) {
					_ = u.opt.Journal.Retry(ctx, up.ID, fmt.Errorf("upload: removing tombstoned file: %w", derr), time.Second)
					return
				}
			}
			_ = u.opt.Journal.Drop(ctx, up.ID)
			return
		}
		if u.opt.Hooks.OnSuccess != nil {
			if herr := u.opt.Hooks.OnSuccess(ctx, up, res); herr != nil {
				u.dead(ctx, up, fmt.Errorf("upload: post-upload bookkeeping failed: %w", herr))
				return
			}
		}
		_ = u.opt.Journal.Succeed(ctx, up.ID)
		return
	}
	if errors.Is(err, provider.ErrNotFound) && u.goneLocally(ctx, up) {
		// The file, or the directory it lived in, was deleted here while
		// this upload waited. Sending it is neither possible nor wanted,
		// and dead-lettering it asks an operator to look at a file that no
		// longer exists.
		_ = u.opt.Journal.Drop(ctx, up.ID)
		return
	}
	switch class := retry.Classify(err); class {
	case retry.ClassCanceled:
		// The transfer context has already ended. Persist its release using
		// a separate bounded context, without spending the failure budget.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = u.opt.Journal.Defer(cleanup, up.ID, 0, err.Error())
	case retry.ClassRetryable, retry.ClassAuth, retry.ClassRiskControl, retry.ClassLinkExpired:
		if up.Attempt+1 >= u.opt.MaxAttempts {
			u.dead(ctx, up, fmt.Errorf("upload: giving up after %d attempts: %w", up.Attempt+1, err))
			return
		}
		delay := u.opt.Backoff.Delay(up.Attempt, retry.RetryAfter(err))
		if class == retry.ClassRiskControl {
			// Risk control means back off hard, not just retry.
			if delay < 30*time.Second {
				delay = 30 * time.Second
			}
		}
		_ = u.opt.Journal.Retry(ctx, up.ID, err, delay)
	default:
		u.dead(ctx, up, err)
	}
}

// goneLocally reports whether the file this upload carries no longer exists
// on this machine. Without a hook to ask, it assumes it still does.
func (u *Uploader) goneLocally(ctx context.Context, up journal.Upload) bool {
	if u.opt.Hooks.Exists == nil || up.Ino == 0 {
		return false
	}
	return !u.opt.Hooks.Exists(ctx, up)
}

func (u *Uploader) dead(ctx context.Context, up journal.Upload, cause error) {
	if err := u.opt.Journal.Fail(ctx, up.ID, cause); err != nil {
		return
	}
	if u.opt.Hooks.OnDead != nil {
		u.opt.Hooks.OnDead(ctx, up, cause)
	}
}

// runOne performs the actual transfer.
func (u *Uploader) runOne(ctx context.Context, p provider.Provider, up journal.Upload) (Result, error) {
	caps := p.Capabilities()
	name := up.Name

	// Conflict check: if the remote version moved since the write started,
	// store the data beside it instead of overwriting someone else's edit.
	if up.ExpectedVersion != "" {
		if changed, err := u.remoteChanged(ctx, p, up); err == nil && changed {
			name = conflictName(up.Name, u.opt.Now())
		}
	}

	if err := u.waitLimit(ctx, up.Remote, ratelimit.Upload); err != nil {
		return Result{}, err
	}

	// A small file goes up in one request when the backend offers it: no
	// session to open, no part to record, no completion call. Nothing to
	// resume either, so a retry simply sends it again.
	if sp, ok := p.(provider.SinglePutter); ok && caps.SinglePutMax > 0 && up.Size <= caps.SinglePutMax {
		if _, resumed := restoreSession(up.Session); !resumed {
			return u.putWhole(ctx, sp, up, name)
		}
	}

	// Resume an interrupted upload with the session the journal kept, so the
	// parts already recorded stay valid. Only start a new session when there
	// is none.
	sess, resumed := restoreSession(up.Session)
	if !resumed {
		var err error
		sess, err = u.begin(ctx, p, up, name)
		if err != nil {
			return Result{}, err
		}
		if sess.RapidDone {
			u.succeeded(up.Remote, ratelimit.Upload)
			r := Result{UploadID: up.ID, Rapid: true}
			if sess.Entry != nil {
				r.Entry = *sess.Entry
			}
			if name != up.Name {
				r.ConflictName = name
			}
			return r, nil
		}
		if err := u.opt.Journal.SetSession(ctx, up.ID, persistSession(sess)); err != nil {
			return Result{}, err
		}
	}

	partSize := sess.PartSize
	if partSize <= 0 {
		partSize = caps.PartSize
	}
	if partSize <= 0 {
		partSize = 4 << 20
	}

	f, err := os.Open(up.BlobPath)
	if err != nil {
		// The blob is gone: no retry can fix this.
		return Result{}, fmt.Errorf("upload: %w: %w", provider.ErrNotFound, err)
	}
	defer f.Close()

	// Resume: skip parts the journal already recorded.
	done := map[int]provider.PartToken{}
	if recorded, err := u.opt.Journal.Parts(ctx, up.ID); err == nil {
		for _, pt := range recorded {
			if pt.State == "done" {
				done[pt.Index] = provider.PartToken{Index: pt.Index, ETag: pt.ETag}
			}
		}
	}

	nParts := int((up.Size + partSize - 1) / partSize)
	if nParts == 0 {
		nParts = 1 // an empty file is still one (zero-length) part
	}
	parts := make([]provider.PartToken, 0, nParts)
	for i := 0; i < nParts; i++ {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if pt, ok := done[i]; ok {
			parts = append(parts, pt)
			continue
		}
		off := int64(i) * partSize
		n := partSize
		if off+n > up.Size {
			n = up.Size - off
		}
		if n < 0 {
			n = 0
		}
		if err := u.waitLimit(ctx, up.Remote, ratelimit.Upload); err != nil {
			return Result{}, err
		}
		var pt provider.PartToken
		err := u.opt.Policy.Do(ctx, func() error {
			sr := io.NewSectionReader(f, off, n)
			var e error
			pt, e = p.UploadPart(ctx, sess, i, sr, n)
			return e
		}, func(err error, _ retry.Class, _ time.Duration) { u.throttled(up.Remote, ratelimit.Upload, err) })
		if err != nil {
			if resumed && errors.Is(err, provider.ErrNotFound) {
				return Result{}, fmt.Errorf("%w: %w", errSessionExpired, err)
			}
			return Result{}, err
		}
		u.succeeded(up.Remote, ratelimit.Upload)
		_ = u.opt.Journal.RecordPart(ctx, up.ID, journal.Part{Index: i, ETag: pt.ETag, State: "done"})
		parts = append(parts, pt)
	}

	var entry provider.Entry
	err = u.opt.Policy.Do(ctx, func() error {
		var e error
		entry, e = p.CompleteUpload(ctx, sess, parts)
		return e
	}, func(err error, _ retry.Class, _ time.Duration) { u.throttled(up.Remote, ratelimit.Upload, err) })
	if err != nil {
		if resumed && errors.Is(err, provider.ErrNotFound) {
			return Result{}, fmt.Errorf("%w: %w", errSessionExpired, err)
		}
		return Result{}, err
	}
	u.succeeded(up.Remote, ratelimit.Upload)
	r := Result{UploadID: up.ID, Entry: entry}
	if name != up.Name {
		r.ConflictName = name
	}
	return r, nil
}

// putWhole sends a small file in a single request.
func (u *Uploader) putWhole(ctx context.Context, sp provider.SinglePutter, up journal.Upload, name string) (Result, error) {
	f, err := os.Open(up.BlobPath)
	if err != nil {
		return Result{}, fmt.Errorf("upload: %w: %w", provider.ErrNotFound, err)
	}
	defer f.Close()
	var entry provider.Entry
	err = u.opt.Policy.Do(ctx, func() error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		var e error
		entry, e = sp.PutFile(ctx, up.RemoteParentID, name, io.LimitReader(f, up.Size), up.Size, up.Hashes)
		return e
	}, func(err error, _ retry.Class, _ time.Duration) { u.throttled(up.Remote, ratelimit.Upload, err) })
	if err != nil {
		return Result{}, err
	}
	u.succeeded(up.Remote, ratelimit.Upload)
	r := Result{UploadID: up.ID, Entry: entry}
	if name != up.Name {
		r.ConflictName = name
	}
	return r, nil
}

// errSessionExpired marks a resumed session the provider no longer knows.
// The uploader clears the session and starts over rather than dead-lettering.
var errSessionExpired = errors.New("upload: resumed session is no longer valid")

func mustParts(ctx context.Context, j *journal.Journal, id string) []journal.Part {
	parts, err := j.Parts(ctx, id)
	if err != nil {
		return nil
	}
	return parts
}

// sessionIDKey and sessionPartSizeKey namespace the fields the uploader adds
// to a provider's opaque session state before persisting it.
const (
	sessionIDKey       = "cloudfs.session_id"
	sessionPartSizeKey = "cloudfs.part_size"
)

// persistSession flattens a session so it survives a restart.
func persistSession(s provider.UploadSession) map[string]string {
	out := map[string]string{}
	for k, v := range s.Opaque {
		out[k] = v
	}
	out[sessionIDKey] = s.ID
	out[sessionPartSizeKey] = strconv.FormatInt(s.PartSize, 10)
	return out
}

// restoreSession rebuilds a session persisted by persistSession. It reports
// false when there is nothing to resume.
func restoreSession(m map[string]string) (provider.UploadSession, bool) {
	id, ok := m[sessionIDKey]
	if !ok || id == "" {
		return provider.UploadSession{}, false
	}
	partSize, _ := strconv.ParseInt(m[sessionPartSizeKey], 10, 64)
	opaque := map[string]string{}
	for k, v := range m {
		if k == sessionIDKey || k == sessionPartSizeKey {
			continue
		}
		opaque[k] = v
	}
	return provider.UploadSession{ID: id, PartSize: partSize, Opaque: opaque}, true
}

func (u *Uploader) begin(ctx context.Context, p provider.Provider, up journal.Upload, name string) (provider.UploadSession, error) {
	content, err := os.Open(up.BlobPath)
	if err != nil {
		return provider.UploadSession{}, fmt.Errorf("upload: open proof content: %w", err)
	}
	defer content.Close()
	ctx = provider.WithUploadRangeHasher(ctx, provider.ContentRangeHasher(content, up.Size))
	ctx = provider.WithUploadContentReader(ctx, provider.ContentRangeReader(content, up.Size))
	var sess provider.UploadSession
	err = u.opt.Policy.Do(ctx, func() error {
		var e error
		sess, e = p.BeginUpload(ctx, up.RemoteParentID, name, up.Size, up.Hashes)
		return e
	}, func(err error, _ retry.Class, _ time.Duration) { u.throttled(up.Remote, ratelimit.Upload, err) })
	return sess, err
}

// remoteChanged reports whether the remote file's version differs from what
// the writer saw when it opened the file.
func (u *Uploader) remoteChanged(ctx context.Context, p provider.Provider, up journal.Upload) (bool, error) {
	entries, _, err := p.List(ctx, up.RemoteParentID, "")
	if err != nil {
		return false, err
	}
	expected := up.ExpectedVersion
	if u.opt.Hooks.RemoteVersion != nil {
		if v, ok := u.opt.Hooks.RemoteVersion(ctx, up); ok && v != "" {
			expected = v
		}
	}
	for _, e := range entries {
		if e.Name == up.Name {
			return e.Version != "" && e.Version != expected, nil
		}
	}
	// The file is gone; recreating it is not a conflict.
	return false, nil
}

// conflictName renders "notes.md" as "notes (conflict 2026-09-02 host).md".
func conflictName(name string, now time.Time) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	suffix := fmt.Sprintf(" (conflict %s %s)", now.Format("2006-01-02"), host)
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' {
			return name[:i] + suffix + name[i:]
		}
	}
	return name + suffix
}

func (u *Uploader) waitLimit(ctx context.Context, remote string, class ratelimit.Class) error {
	if u.opt.Limiters == nil {
		return nil
	}
	return u.opt.Limiters.Limiter(ratelimit.Key{Remote: remote, Class: class}).Wait(ctx)
}

func (u *Uploader) throttled(remote string, class ratelimit.Class, err error) {
	if u.opt.Limiters == nil {
		return
	}
	switch retry.Classify(err) {
	case retry.ClassRetryable, retry.ClassRiskControl:
		u.opt.Limiters.Limiter(ratelimit.Key{Remote: remote, Class: class}).Throttled(retry.RetryAfter(err))
		if retry.Classify(err) == retry.ClassRiskControl {
			u.opt.Limiters.Breaker(remote, "").Trip()
		}
	}
}

func (u *Uploader) succeeded(remote string, class ratelimit.Class) {
	if u.opt.Limiters == nil {
		return
	}
	u.opt.Limiters.Limiter(ratelimit.Key{Remote: remote, Class: class}).Succeeded()
}
