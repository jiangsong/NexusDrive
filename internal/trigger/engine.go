// Package trigger turns VFS changes into at-least-once deliveries of the
// actions configured under triggers[] and agents[] (docs/agent-roadmap.md
// §5.3–5.7). It runs only in the process that owns agent.db: one goroutine
// matches the change stream against the rules and enqueues rows into
// trigger_deliveries, and one serial worker per rule claims due rows and
// runs the exec or webhook with retries. The queue is the durable part —
// a process that dies mid-action leaves a running row, which the next
// start turns back into pending.
package trigger

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/vfs"
)

// Errors the control plane maps to status codes.
var (
	ErrUnknownRule  = errors.New("trigger: no such rule")
	ErrUnknownAgent = errors.New("trigger: no such agent")
	// ErrAlreadyQueued is returned by Invoke when an invocation of the same
	// agent on the same first path is still pending: the debounce index
	// would merge the two and lose the second prompt.
	ErrAlreadyQueued = errors.New("trigger: an invocation for this path is already queued")
)

// Retry policy: a failed delivery waits backoff(attempts) and is dead
// after maxAttempts.
const (
	maxAttempts    = 8
	backoffBase    = time.Second
	backoffCeiling = 5 * time.Minute
)

// agentRulePrefix namespaces agent invocations in the deliveries table:
// rule = "agent:<name>". Config forbids ":" in trigger names, so the two
// never collide.
const agentRulePrefix = "agent:"

// Delivery kinds and origins the engine writes for rows that are not a
// VFS change.
const (
	KindTest      = "test"    // Engine.Test
	KindInvoke    = "invoke"  // Engine.Invoke
	OriginConsole = "console" // both of the above
)

// Options configures New. Only Store is required; an engine with no FS
// (and no Watch) never matches changes but still serves Test and Invoke.
type Options struct {
	FS     *vfs.FS
	Rules  []config.Trigger
	Agents []config.Agent
	Store  *agent.Store
	// Secrets resolves a webhook's keyring:/secretfile: reference.
	Secrets func(ref string) (string, error)
	// Proxy builds webhook clients; nil means a plain client.
	Proxy *proxy.Manager
	// URIFor returns the cloudfs:// resource URI of a path; nil uses the
	// same rule the MCP server's resources use, over FS's mounts.
	URIFor func(path string) string
	Now    func() time.Time
	Logger *slog.Logger
	// Watch replaces FS.WatchChanges as the change source (tests).
	Watch func() (<-chan vfs.Change, func())
	// Tick is how often a worker re-checks for due rows besides being woken
	// by an enqueue; it bounds how late a debounced delivery runs. Default
	// one second.
	Tick time.Duration
}

// Engine is the trigger runtime, see the package comment.
type Engine struct {
	opt    Options
	q      *agent.Deliveries
	rules  map[string]config.Trigger
	agents map[string]config.Agent
	// workers holds one serial worker per rule and per agent.
	workers map[string]*worker
	log     *slog.Logger

	// invocations keeps what Invoke was asked to run, by delivery id. The
	// table holds only the first path; the prompt and the rest live here
	// for the process's lifetime, so a retry works until a restart.
	invMu       sync.Mutex
	invocations map[int64]invocation

	// pending caches the id of the pending row per (rule, path), so a burst
	// of events on one path costs one insert and then map hits instead of
	// an INSERT OR IGNORE per event. Invariant: a cached key has a pending
	// row in the table. Only Claim ends a row's pending state, and it runs
	// under pendMu here, so the cache cannot claim a row exists that a
	// worker has taken; a miss just costs the insert (which then merges).
	// Anything outside the engine must therefore go through the engine to
	// change a pending row, which the control plane does.
	pendMu  sync.Mutex
	pending map[pendKey]int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	// seen counts consumed changes, for tests that wait on the matcher.
	seen atomic.Int64
}

type worker struct {
	rule string
	wake chan struct{}
}

type invocation struct {
	paths  []string
	prompt string
}

type pendKey struct{ rule, path string }

// New builds an engine; Run starts it.
func New(opt Options) *Engine {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Tick <= 0 {
		opt.Tick = time.Second
	}
	if opt.Watch == nil && opt.FS != nil {
		fs := opt.FS
		opt.Watch = fs.WatchChanges
	}
	e := &Engine{
		opt:         opt,
		rules:       make(map[string]config.Trigger, len(opt.Rules)),
		agents:      make(map[string]config.Agent, len(opt.Agents)),
		workers:     make(map[string]*worker, len(opt.Rules)+len(opt.Agents)),
		log:         opt.Logger,
		invocations: make(map[int64]invocation),
		pending:     make(map[pendKey]int64),
	}
	if opt.Store != nil {
		e.q = opt.Store.Deliveries()
	}
	for _, r := range opt.Rules {
		e.rules[r.Name] = r
		e.workers[r.Name] = &worker{rule: r.Name, wake: make(chan struct{}, 1)}
	}
	for _, a := range opt.Agents {
		e.agents[a.Name] = a
		name := agentRulePrefix + a.Name
		e.workers[name] = &worker{rule: name, wake: make(chan struct{}, 1)}
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	return e
}

// Rules returns the configured trigger rules, for the read-only views.
func (e *Engine) Rules() []config.Trigger { return append([]config.Trigger(nil), e.opt.Rules...) }

// Agents returns the configured agents, for the read-only views.
func (e *Engine) Agents() []config.Agent { return append([]config.Agent(nil), e.opt.Agents...) }

// Run reopens the rows the previous process left running and starts the
// matcher and the workers. They stop when ctx ends or Close is called.
func (e *Engine) Run(ctx context.Context) error {
	if e.q == nil {
		return errors.New("trigger: a store is required")
	}
	if n, err := e.q.ResetRunning(ctx); err != nil {
		return err
	} else if n > 0 {
		e.log.Info("trigger: re-queued deliveries interrupted by the last shutdown", "count", n)
	}
	go func() {
		select {
		case <-ctx.Done():
			e.cancel()
		case <-e.ctx.Done():
		}
	}()
	if e.opt.Watch != nil {
		e.wg.Add(1)
		go e.watch()
	}
	for _, w := range e.workers {
		e.wg.Add(1)
		go e.work(w)
	}
	return nil
}

// Close stops the goroutines and waits for them. An action in flight is
// killed and its row stays running, exactly as after a crash, so the next
// Run delivers it again.
func (e *Engine) Close() {
	e.once.Do(func() {
		e.cancel()
		e.wg.Wait()
	})
}

func (e *Engine) now() time.Time { return e.opt.Now() }

// watch consumes the change stream until it closes or the engine stops.
func (e *Engine) watch() {
	defer e.wg.Done()
	ch, stop := e.opt.Watch()
	defer stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			e.handle(c)
			e.seen.Add(1)
		}
	}
}

// handle matches one change against every rule and enqueues the hits. A
// rescan becomes one path="" row per rule that did not opt out: the
// events it stands for can only be delivered as a rescan.
func (e *Engine) handle(c vfs.Change) {
	now := e.now()
	for _, r := range e.opt.Rules {
		if c.Rescan {
			if r.OnRescan != config.OnRescanIgnore {
				e.enqueue(e.ctx, r.Name, "", vfs.KindRescan.String(), vfs.OriginRemote.String(), now.Add(r.Debounce))
			}
			continue
		}
		for _, p := range matchPaths(r, c) {
			e.enqueue(e.ctx, r.Name, p, c.Kind.String(), c.Origin.String(), now.Add(r.Debounce))
		}
	}
}

// enqueue inserts (or merges) a row and, when it is already due, wakes
// the rule's worker. A key the cache knows as pending is merged without
// touching the table. A row still inside its debounce window is left to
// the ticker: waking on every event would make a burst cost a poll per
// event on top of the insert.
func (e *Engine) enqueue(ctx context.Context, rule, path, kind, origin string, due time.Time) (int64, bool, error) {
	key := pendKey{rule, path}
	e.pendMu.Lock()
	defer e.pendMu.Unlock()
	if id, ok := e.pending[key]; ok {
		return id, true, nil
	}
	id, merged, err := e.q.Enqueue(ctx, rule, path, kind, origin, due)
	if err != nil {
		if e.ctx.Err() == nil {
			e.log.Warn("trigger: enqueue failed", "rule", rule, "path", path, "err", err)
		}
		return 0, false, err
	}
	e.pending[key] = id
	if !merged && !due.After(e.now()) {
		e.wake(rule)
	}
	return id, merged, nil
}

// claim is Deliveries.Claim under pendMu: the row leaves the pending
// state and the cache in one step.
func (e *Engine) claim(rule string) (agent.Delivery, bool, error) {
	e.pendMu.Lock()
	defer e.pendMu.Unlock()
	d, ok, err := e.q.Claim(e.ctx, rule, e.now())
	if ok {
		delete(e.pending, pendKey{d.Rule, d.Path})
	}
	return d, ok, err
}

func (e *Engine) wake(rule string) {
	if w, ok := e.workers[rule]; ok {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// work is one rule's serial worker: claim every due row, run it, and
// sleep until woken or the next tick.
func (e *Engine) work(w *worker) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.opt.Tick)
	defer ticker.Stop()
	for {
		for e.ctx.Err() == nil {
			d, ok, err := e.claim(w.rule)
			if err != nil {
				if e.ctx.Err() == nil {
					e.log.Warn("trigger: claim failed", "rule", w.rule, "err", err)
				}
				break
			}
			if !ok {
				break
			}
			e.deliver(d)
		}
		select {
		case <-e.ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

// deliver runs one claimed row and records the outcome. A failure while
// the engine is stopping is not recorded: the kill caused it, and the row
// must stay running so the next start re-queues it.
func (e *Engine) deliver(d agent.Delivery) {
	output, err := e.run(d)
	if err != nil && e.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch {
	case err == nil:
		err = e.q.Done(ctx, d.ID, output)
	case errors.Is(err, errPermanent) || strings.HasPrefix(d.Rule, agentRulePrefix) || d.Attempts >= maxAttempts:
		// An agent run has a person waiting on it; parking it at once
		// beats retrying an expensive command for an hour.
		e.log.Warn("trigger: delivery dead", "rule", d.Rule, "path", d.Path, "attempts", d.Attempts, "err", err)
		err = e.q.Dead(ctx, d.ID, err.Error(), output)
	default:
		wait := backoff(d.Attempts)
		e.log.Warn("trigger: delivery failed", "rule", d.Rule, "path", d.Path, "attempt", d.Attempts, "retry_in", wait, "err", err)
		e.pendMu.Lock()
		err = e.q.Fail(ctx, d.ID, err.Error(), output, e.now().Add(wait))
		if err == nil {
			e.pending[pendKey{d.Rule, d.Path}] = d.ID
		}
		e.pendMu.Unlock()
	}
	if err != nil {
		e.log.Error("trigger: could not record the delivery outcome", "id", d.ID, "err", err)
	}
}

// errPermanent marks a failure no retry can fix.
var errPermanent = errors.New("trigger: not retryable")

// run executes the action behind d.
func (e *Engine) run(d agent.Delivery) (string, error) {
	if name, ok := strings.CutPrefix(d.Rule, agentRulePrefix); ok {
		return e.runAgent(name, d)
	}
	rule, ok := e.rules[d.Rule]
	if !ok {
		return "", fmt.Errorf("%w: rule %q is no longer configured", errPermanent, d.Rule)
	}
	switch {
	case rule.Action.Exec != nil:
		vars := map[string]string{"{path}": d.Path, "{kind}": d.Kind, "{uri}": ""}
		if d.Path != "" {
			vars["{uri}"] = e.uriFor(d.Path)
		}
		stdout, stderr, err := runExec(e.ctx, *rule.Action.Exec, vars)
		return joinOutput(stdout, stderr), err
	case rule.Action.Webhook != nil:
		return e.runWebhook(e.ctx, rule, d)
	}
	return "", fmt.Errorf("%w: rule %q has no action", errPermanent, d.Rule)
}

// runAgent runs an Invoke: {prompt} substituted, every path appended as
// its own argv element.
func (e *Engine) runAgent(name string, d agent.Delivery) (string, error) {
	a, ok := e.agents[name]
	if !ok {
		return "", fmt.Errorf("%w: agent %q is no longer configured", errPermanent, name)
	}
	e.invMu.Lock()
	inv, ok := e.invocations[d.ID]
	e.invMu.Unlock()
	if !ok {
		return "", fmt.Errorf("%w: the prompt of an agent run is not kept across restarts; run it again from the console", errPermanent)
	}
	exec := a.Exec
	exec.Command = append(append([]string(nil), a.Exec.Command...), inv.paths...)
	vars := map[string]string{"{prompt}": inv.prompt, "{path}": d.Path, "{kind}": d.Kind, "{uri}": ""}
	if d.Path != "" {
		vars["{uri}"] = e.uriFor(d.Path)
	}
	stdout, stderr, err := runExec(e.ctx, exec, vars)
	return joinOutput(stdout, stderr), err
}

// Test queues a delivery of rule for path, due now, and returns its id. It
// goes through the queue like any other, so the console can follow it;
// the kind is "test" and the origin "console". A pending row for the same
// path absorbs it.
func (e *Engine) Test(ctx context.Context, rule, path string) (int64, error) {
	if _, ok := e.rules[rule]; !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownRule, rule)
	}
	id, _, err := e.enqueue(ctx, rule, path, KindTest, OriginConsole, e.now())
	return id, err
}

// Invoke runs agent name on paths with prompt (docs/agent-roadmap.md
// §5.7), recorded as a delivery under rule "agent:<name>" whose path is
// the first of paths. It is not retried on failure; a person is waiting.
func (e *Engine) Invoke(ctx context.Context, name string, paths []string, prompt string) (int64, error) {
	if _, ok := e.agents[name]; !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownAgent, name)
	}
	first := ""
	if len(paths) > 0 {
		first = paths[0]
	}
	// Hold invMu across the enqueue so the worker, which locks it to read
	// the invocation, cannot claim the row before the prompt is stored.
	e.invMu.Lock()
	defer e.invMu.Unlock()
	id, merged, err := e.enqueue(ctx, agentRulePrefix+name, first, KindInvoke, OriginConsole, e.now())
	if err != nil {
		return 0, err
	}
	if merged {
		return id, fmt.Errorf("%w (delivery %d)", ErrAlreadyQueued, id)
	}
	e.invocations[id] = invocation{paths: append([]string(nil), paths...), prompt: prompt}
	return id, nil
}

// Retry reopens a dead delivery and wakes its worker.
func (e *Engine) Retry(ctx context.Context, id int64) error {
	e.pendMu.Lock()
	defer e.pendMu.Unlock()
	if err := e.q.Retry(ctx, id); err != nil {
		return err
	}
	if d, err := e.q.Get(ctx, id); err == nil {
		e.pending[pendKey{d.Rule, d.Path}] = d.ID
		e.wake(d.Rule)
	}
	return nil
}

// Counts reports pending and dead rows, for status and the console badge.
func (e *Engine) Counts(ctx context.Context) (pending, dead int, err error) {
	return e.q.Counts(ctx)
}

// DeadCount is the console badge: deliveries waiting for a human.
func (e *Engine) DeadCount(ctx context.Context) (int, error) {
	_, dead, err := e.q.Counts(ctx)
	return dead, err
}

// backoff is the wait before attempt n+1 after n failures: 1 s doubling
// to a 5 minute ceiling.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts-1 >= 10 {
		return backoffCeiling
	}
	d := backoffBase << (attempts - 1)
	if d > backoffCeiling {
		d = backoffCeiling
	}
	return d
}

// uriFor returns the cloudfs:// URI of p, through Options.URIFor or the
// same rule the MCP server's resources use (internal/mcpsrv/resources.go
// resourceURI): the remote owning the path is the authority, hex-escaped
// when its name is not URL-safe.
func (e *Engine) uriFor(p string) string {
	if e.opt.URIFor != nil {
		return e.opt.URIFor(p)
	}
	remote := ""
	if e.opt.FS != nil {
		// Mounts are ordered deepest first, so the first match owns the path.
		for _, m := range e.opt.FS.Mounts() {
			if m.Prefix == "/" || p == m.Prefix || strings.HasPrefix(p, m.Prefix+"/") {
				remote = m.Remote
				break
			}
		}
	}
	return (&url.URL{Scheme: "cloudfs", Host: uriAuthority(remote), Path: p}).String()
}

func uriAuthority(remote string) string {
	if remote != "" {
		safe := true
		for _, r := range remote {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				safe = false
				break
			}
		}
		if safe {
			return remote
		}
	}
	return "r~" + hex.EncodeToString([]byte(remote))
}
