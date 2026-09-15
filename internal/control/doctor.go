package control

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
)

// Level grades one check.
type Level string

const (
	// LevelOK means the check passed.
	LevelOK Level = "ok"
	// LevelWarn means the system works but something is degraded.
	LevelWarn Level = "warn"
	// LevelFail means something is broken and needs attention.
	LevelFail Level = "fail"
)

// Check is one diagnostic result. Detail says what was observed; Fix says what
// to do about it.
type Check struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// Fixable marks checks `doctor --fix` can repair on its own.
	Fixable bool `json:"fixable"`
	// detail and fix are what Detail and Fix were rendered from: catalog
	// keys with their arguments, so the same check can be answered in another
	// language at the HTTP boundary. They are empty for text that came from
	// an error or a remote, which has no translation and is passed through
	// unchanged. A check may add a second fragment; they join with "; ".
	detail []message
	fix    []message
}

// Doctor runs environment and state checks.
type Doctor struct {
	// Config reads the configuration as it is now. It is a function because
	// the control plane republishes a copy after every edit: a Doctor holding
	// the pointer it was built with would report on the accounts the daemon
	// started with and silently omit every drive added since.
	Config func() *config.Config
	// Passthrough reports whether FUSE passthrough is actually usable here,
	// with the reason when it is not. nil falls back to a kernel version test.
	Passthrough func() (bool, string)
	CacheDir    string
	MetaPath    string
	Journal     *journal.Journal
	Meta        *meta.Store
	Cache       *cache.Cache
	Proxy       *proxy.Manager
	MinFree     int64
	FreeSpace   func(string) (int64, error)
	// FUSESupported reports whether a mount is possible here.
	FUSESupported func() (bool, string)
	Now           func() time.Time
	// Pools are the running storage pools by the remote that exposes each;
	// MemberProviders the live member backends, for the marker check.
	Pools           map[string]*pool.Pool
	MemberProviders map[string]provider.Provider
	HoldMaxBytes    int64
	// Index, when set, is the content index to check; nil on a daemon
	// started with index.enabled false, which then has nothing to report.
	Index IndexControl
	// Agent is the open agent.db, nil on a Doctor built without a daemon.
	// AgentDir is its directory, <cache.dir>/agent, where the heartbeats
	// of stdio MCP processes beside the owner live; it is read even when
	// Agent is nil, so a config-only doctor still spots them.
	Agent    *agent.Store
	AgentDir string
}

// config reads the published configuration, tolerating both a Doctor built
// without one and a hook that answers nil.
func (d *Doctor) config() *config.Config {
	if d.Config == nil {
		return nil
	}
	return d.Config()
}

// Run performs every check.
func (d *Doctor) Run(ctx context.Context) []Check {
	var out []Check
	out = append(out, d.checkPlatform()...)
	out = append(out, d.checkCacheDir()...)
	out = append(out, d.checkMeta(ctx)...)
	out = append(out, d.checkJournal(ctx)...)
	out = append(out, d.checkProxy(ctx)...)
	out = append(out, d.checkPools(ctx)...)
	out = append(out, d.checkIndex(ctx)...)
	out = append(out, d.checkAgent(ctx)...)
	out = append(out, d.checkTriggers()...)
	if cfg := d.config(); cfg != nil {
		for name, r := range cfg.Remotes {
			level, detailKey := LevelOK, "doctor.creds.keyring"
			has := false
			for field, value := range r.Extra {
				if !config.IsSecretField(field) {
					continue
				}
				s, ok := value.(string)
				if !ok || s == "" {
					continue
				}
				has = true
				if strings.HasPrefix(s, "secretfile:") {
					if level == LevelOK {
						level, detailKey = LevelWarn, "doctor.creds.secretfile"
					}
				} else if !strings.HasPrefix(s, "keyring:") {
					level, detailKey = LevelWarn, "doctor.creds.plaintext"
				}
			}
			if has {
				c := Check{Name: "credentials/" + name, Level: level, Fix: "cloudfs config auth " + name}
				c.setDetail(detailKey)
				out = append(out, c)
			}
		}
	}
	return out
}

func (d *Doctor) checkPlatform() []Check {
	var out []Check
	plat := Check{Name: "platform", Level: LevelOK}
	plat.setDetail("doctor.platform", runtime.GOOS, runtime.GOARCH, runtime.Version())
	out = append(out, plat)

	if d.FUSESupported != nil {
		ok, why := d.FUSESupported()
		c := Check{Name: "fuse"}
		if ok {
			c.Level = LevelOK
			c.setDetail("doctor.fuse.ok")
		} else {
			c.Level, c.Detail = LevelFail, why
			switch runtime.GOOS {
			case "linux":
				c.setFix("doctor.fuse.fix.linux")
			case "darwin":
				c.setFix("doctor.fuse.fix.darwin")
			}
		}
		out = append(out, c)
	}

	switch runtime.GOOS {
	case "linux":
		if b, err := os.ReadFile("/etc/fuse.conf"); err == nil {
			allowed := false
			for _, line := range strings.Split(string(b), "\n") {
				if strings.TrimSpace(line) == "user_allow_other" {
					allowed = true
				}
			}
			c := Check{Name: "allow_other"}
			if allowed {
				c.Level = LevelOK
				c.setDetail("doctor.allow_other.ok")
			} else {
				c.Level = LevelWarn
				c.setDetail("doctor.allow_other.warn")
				c.setFix("doctor.allow_other.fix")
			}
			out = append(out, c)
		}
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			rel := strings.TrimSpace(string(b))
			c := Check{Name: "kernel"}
			// reason is the platform probe's own words, which are
			// pass-through text like any provider error. Our own "kernel is
			// too old" reason is a key instead: rendering it here would
			// freeze one language into an argument that Localize cannot
			// reach.
			ok, reason := false, ""
			if d.Passthrough != nil {
				ok, reason = d.Passthrough()
			} else if kernelAtLeast(rel, 6, 9) {
				ok = true
			}
			switch {
			case ok:
				c.Level = LevelOK
				c.setDetail("doctor.kernel.ok", rel)
			case reason != "":
				c.Level = LevelWarn
				c.setDetail("doctor.kernel.warn", rel, reason)
				c.setFix("doctor.kernel.fix")
			default:
				c.Level = LevelWarn
				c.setDetail("doctor.kernel.warn_old", rel)
				c.setFix("doctor.kernel.fix")
			}
			out = append(out, c)
		}
	case "darwin":
		fs := ""
		for _, p := range []string{"/Library/Filesystems/macfuse.fs", "/Library/Filesystems/fuse-t.fs"} {
			if _, err := os.Stat(p); err == nil {
				fs = p
				break
			}
		}
		c := Check{Name: "macfuse"}
		if fs != "" {
			c.Level = LevelOK
			c.setDetail("doctor.macfuse.ok", fs)
		} else {
			c.Level = LevelFail
			c.setDetail("doctor.macfuse.absent")
			c.setFix("doctor.macfuse.fix")
		}
		out = append(out, c)
	}
	return out
}

func (d *Doctor) checkCacheDir() []Check {
	if d.CacheDir == "" {
		return nil
	}
	var out []Check
	c := Check{Name: "cache_dir"}
	if err := os.MkdirAll(d.CacheDir, 0o700); err != nil {
		c.Level = LevelFail
		c.setDetail("doctor.cache.mkdir_failed", d.CacheDir, err)
		c.setFix("doctor.cache.fix.location")
		return append(out, c)
	}
	probe := filepath.Join(d.CacheDir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		c.Level = LevelFail
		c.setDetail("doctor.cache.unwritable", d.CacheDir, err)
		c.setFix("doctor.cache.fix.perms")
		return append(out, c)
	}
	os.Remove(probe)
	c.Level = LevelOK
	c.setDetail("doctor.cache.writable", d.CacheDir)
	out = append(out, c)

	if d.FreeSpace != nil {
		free, err := d.FreeSpace(d.CacheDir)
		fc := Check{Name: "cache_free_space"}
		switch {
		case err != nil:
			fc.Level = LevelWarn
			fc.setDetail("doctor.free.unmeasurable", err)
		case d.MinFree > 0 && free < d.MinFree:
			fc.Level = LevelFail
			fc.setDetail("doctor.free.below_min", humanBytes(free), humanBytes(d.MinFree))
			fc.setFix("doctor.free.fix.below_min")
			fc.Fixable = true
		case free < 1<<30:
			fc.Level = LevelWarn
			fc.setDetail("doctor.free.low", humanBytes(free))
			fc.setFix("doctor.free.fix.low")
			fc.Fixable = true
		default:
			fc.Level = LevelOK
			fc.setDetail("doctor.free.ok", humanBytes(free))
		}
		out = append(out, fc)
	}
	return out
}

func (d *Doctor) checkMeta(ctx context.Context) []Check {
	if d.Meta == nil {
		return nil
	}
	c := Check{Name: "metadata_db"}
	if err := d.Meta.Vacuum(ctx); err != nil {
		c.Level = LevelFail
		c.Detail = err.Error()
		c.setFix("doctor.meta.fix")
		return []Check{c}
	}
	st, err := d.Meta.Stats(ctx)
	if err != nil {
		c.Level = LevelFail
		c.Detail = err.Error()
		return []Check{c}
	}
	c.Level = LevelOK
	c.setDetail("doctor.meta.ok", st.Nodes, st.CompleteDs)
	return []Check{c}
}

func (d *Doctor) checkJournal(ctx context.Context) []Check {
	if d.Journal == nil {
		return nil
	}
	st, err := d.Journal.Stats(ctx)
	if err != nil {
		return []Check{{Name: "upload_queue", Level: LevelFail, Detail: err.Error()}}
	}
	var out []Check
	c := Check{Name: "upload_queue"}
	switch {
	case st.Dead > 0:
		c.Level = LevelFail
		c.setDetail("doctor.queue.dead", st.Dead)
		c.setFix("doctor.queue.fix.dead")
		c.Fixable = true
	case st.Purging > 0:
		c.Level = LevelWarn
		c.setDetail("doctor.queue.purging", st.Purging)
		c.setFix("doctor.queue.fix.purging")
	case st.Cancelled+st.Cancelling > 0:
		c.Level = LevelWarn
		c.setDetail("doctor.queue.cancelled", st.Cancelling, st.Cancelled)
		c.setFix("doctor.queue.fix.cancel")
	case st.OldestAge > 30*time.Minute:
		c.Level = LevelWarn
		c.setDetail("doctor.queue.slow", st.Pending+st.Uploading, st.OldestAge.Round(time.Second))
		c.setFix("doctor.queue.fix.slow")
	case st.Pending+st.Uploading > 0:
		c.Level = LevelOK
		c.setDetail("doctor.queue.inflight", st.Pending+st.Uploading, humanBytes(st.Bytes))
	default:
		c.Level = LevelOK
		c.setDetail("doctor.queue.idle")
	}
	out = append(out, c)
	switch d.Journal.Durability() {
	case journal.DurabilityCrash:
		dc := Check{Name: "durability", Level: LevelWarn}
		dc.setDetail("doctor.durability.crash")
		dc.setFix("doctor.durability.fix")
		out = append(out, dc)
	default:
		dc := Check{Name: "durability", Level: LevelOK}
		dc.setDetail("doctor.durability.power")
		out = append(out, dc)
	}

	// Payloads no row names. Normally none: they appear when a crash lands
	// between removing a row and unlinking its object, and the next start
	// clears them. If they persist, the queue has stopped reclaiming content —
	// and these bytes are in no other report, being outside the cache budget
	// and outside the queued total.
	if n, held, err := d.Journal.OrphanObjects(ctx); err == nil && n > 0 {
		oc := Check{Name: "queue_objects", Level: LevelWarn}
		oc.setDetail("doctor.queue.orphans", n, humanBytes(held))
		oc.setFix("doctor.queue.fix.orphans")
		out = append(out, oc)
	}

	// Orphan staging files mean a crash left partial writes behind.
	entries, err := os.ReadDir(d.Journal.StagingDir())
	if err == nil && len(entries) > 0 {
		sc := Check{Name: "staging_files", Level: LevelWarn, Fixable: true}
		sc.setDetail("doctor.staging.orphans", len(entries))
		sc.setFix("doctor.staging.fix")
		out = append(out, sc)
	}
	return out
}

func (d *Doctor) checkProxy(ctx context.Context) []Check {
	if d.Proxy == nil {
		return nil
	}
	health := d.Proxy.CheckNow(ctx)
	if len(health) == 0 {
		pc := Check{Name: "proxy", Level: LevelOK}
		pc.setDetail("doctor.proxy.none")
		return []Check{pc}
	}
	var out []Check
	for _, h := range health {
		c := Check{Name: "proxy/" + h.Name}
		if h.Healthy {
			c.Level = LevelOK
			c.setDetail("doctor.proxy.ok", h.Latency.Milliseconds())
		} else {
			c.Level = LevelFail
			c.Detail = h.Err
			c.setFix("doctor.proxy.fix")
		}
		out = append(out, c)
	}
	return out
}

// checkIndex reports on the content index: whether index.db opens and
// answers, whether it was built against the meta store this daemon runs
// on, how many documents failed to extract and how close the text is to
// its budget.
// checkAgent reports agent.db and the topology docs/mcp.md advises
// against: a stdio MCP server started beside the cache owner. That server
// has its own VFS and no uploader (T-43), so it announces itself with a
// heartbeat under AgentDir, and the warning names the HTTP transport as the
// way out.
func (d *Doctor) checkAgent(ctx context.Context) []Check {
	var out []Check
	if d.Agent != nil {
		c := Check{Name: "agent_db"}
		file, build, err := d.Agent.SchemaVersions(ctx)
		switch {
		case err != nil:
			c.Level = LevelFail
			c.passDetail(err.Error())
			c.setFix("doctor.agent.db.fix")
		case file != build:
			c.Level = LevelFail
			c.setDetail("doctor.agent.schema", file, build)
			c.setFix("doctor.agent.db.fix")
		default:
			c.Level = LevelOK
			c.setDetail("doctor.agent.ok", file)
		}
		out = append(out, c)
	}
	dir := d.AgentDir
	if dir == "" && d.Agent != nil {
		dir = d.Agent.Dir()
	}
	if dir == "" {
		return out
	}
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	c := Check{Name: "agent_stdio"}
	if pids := agent.LiveStdioProcesses(dir, now); len(pids) > 0 {
		c.Level = LevelWarn
		c.setDetail("doctor.agent.stdio.warn", len(pids), pidList(pids))
		c.setFix("doctor.agent.stdio.fix")
	} else {
		c.Level = LevelOK
		c.setDetail("doctor.agent.stdio.ok")
	}
	return append(out, c)
}

func pidList(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ", ")
}

func (d *Doctor) checkIndex(ctx context.Context) []Check {
	if d.Index == nil {
		return nil
	}
	st, err := d.Index.Status(ctx, "")
	if err != nil {
		c := Check{Name: "index_db", Level: LevelFail, Detail: err.Error()}
		c.setFix("doctor.index.db.fix")
		return []Check{c}
	}
	var out []Check
	c := Check{Name: "index_db", Level: LevelOK}
	c.setDetail("doctor.index.ok", st.Docs.OK, st.ChunksTotal)
	out = append(out, c)

	if d.Meta != nil {
		if got, err := d.Index.Identity(ctx); err == nil && got != "" {
			ic := Check{Name: "index_identity"}
			if want, err := d.Meta.Identity(ctx); err == nil && want != got {
				ic.Level = LevelWarn
				ic.setDetail("doctor.index.identity.mismatch")
				ic.setFix("doctor.index.identity.fix")
			} else {
				ic.Level = LevelOK
				ic.setDetail("doctor.index.identity.ok")
			}
			out = append(out, ic)
		}
	}

	if st.Docs.Failed > 0 {
		fc := Check{Name: "index_failed", Level: LevelWarn, Fixable: true}
		fc.setDetail("doctor.index.failed", st.Docs.Failed)
		fc.setFix("doctor.index.failed.fix")
		out = append(out, fc)
	}
	if st.MaxTotalText > 0 && st.TextBytes*10 >= st.MaxTotalText*9 {
		bc := Check{Name: "index_text_budget", Level: LevelWarn}
		bc.setDetail("doctor.index.budget", humanBytes(st.TextBytes), humanBytes(st.MaxTotalText))
		bc.setFix("doctor.index.budget.fix")
		out = append(out, bc)
	}
	return out
}

// checkTriggers repeats what Validate accepted with a warning — an exec
// rule the agent's own writes can fire — so it is seen on the diagnostics
// page after the mount's log line has scrolled away. A configuration with
// no rules and no agents has nothing to report.
func (d *Doctor) checkTriggers() []Check {
	cfg := d.config()
	if cfg == nil || (len(cfg.Triggers) == 0 && len(cfg.Agents) == 0 && len(cfg.Warnings) == 0) {
		return nil
	}
	c := Check{Name: "triggers_config"}
	if len(cfg.Warnings) == 0 {
		c.Level = LevelOK
		c.setDetail("doctor.triggers.ok", len(cfg.Triggers), len(cfg.Agents))
		return []Check{c}
	}
	// The warnings are Validate's own sentences, which name the rule and
	// the fix; they are passed through as the argument of one key.
	c.Level = LevelWarn
	c.setDetail("doctor.triggers.warn", strings.Join(cfg.Warnings, "; "))
	c.setFix("doctor.triggers.fix")
	return []Check{c}
}

// Fix repairs what it safely can and reports what it did, in lang: the
// report goes straight to a person, in the UI toast or the terminal, so it is
// the one place in the doctor where the language is an argument rather than a
// post-processing step.
func (d *Doctor) Fix(ctx context.Context, lang i18n.Lang) []string {
	var done []string
	if d.Journal != nil {
		// Remove staging files with no journal row: they are writes that never
		// reached a commit and can never be completed.
		if rec, err := d.Journal.Recover(ctx); err == nil {
			if n := len(rec.OrphanStaging); n > 0 {
				done = append(done, i18n.T(lang, "fix.staging_removed", n))
			}
			if n := len(rec.Requeued); n > 0 {
				done = append(done, i18n.T(lang, "fix.requeued", n))
			}
			if n := len(rec.Lost); n > 0 {
				done = append(done, i18n.T(lang, "fix.dead_lettered", n))
			}
		}
		if n, err := d.Journal.Purge(ctx, 24*time.Hour); err == nil && n > 0 {
			done = append(done, i18n.T(lang, "fix.purged", n))
		}
	}
	if d.Cache != nil {
		before := d.Cache.Stats()
		if err := d.Cache.GC(); err == nil {
			after := d.Cache.Stats()
			if freed := before.Bytes - after.Bytes; freed > 0 {
				done = append(done, i18n.T(lang, "fix.evicted", humanBytes(freed)))
			}
		}
	}
	if d.Meta != nil {
		if err := d.Meta.Vacuum(ctx); err == nil {
			done = append(done, i18n.T(lang, "fix.checkpointed"))
		}
	}
	if d.Index != nil {
		if n, err := d.Index.Retry(ctx, ""); err == nil && n > 0 {
			done = append(done, i18n.T(lang, "fix.index_requeued", n))
		}
	}
	if len(done) == 0 {
		done = append(done, i18n.T(lang, "fix.nothing"))
	}
	return done
}

// Summary counts checks by level.
func Summary(checks []Check) (ok, warn, fail int) {
	for _, c := range checks {
		switch c.Level {
		case LevelOK:
			ok++
		case LevelWarn:
			warn++
		case LevelFail:
			fail++
		}
	}
	return
}

func kernelAtLeast(rel string, major, minor int) bool {
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '.' || r == '-' })
	if len(parts) < 2 {
		return false
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return a > major || (a == major && b >= minor)
}
