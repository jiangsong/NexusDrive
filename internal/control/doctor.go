package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
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
}

// Doctor runs environment and state checks.
type Doctor struct {
	Config *config.Config
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
	if d.Config != nil {
		for name, r := range d.Config.Remotes {
			level, detail := LevelOK, "credentials use the system keyring"
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
						level, detail = LevelWarn, "credentials use private 0600 files (keyring fallback)"
					}
				} else if !strings.HasPrefix(s, "keyring:") {
					level, detail = LevelWarn, "configuration contains plaintext credentials"
				}
			}
			if has {
				out = append(out, Check{Name: "credentials/" + name, Level: level, Detail: detail, Fix: "cloudfs config auth " + name})
			}
		}
	}
	return out
}

func (d *Doctor) checkPlatform() []Check {
	var out []Check
	out = append(out, Check{
		Name:   "platform",
		Level:  LevelOK,
		Detail: fmt.Sprintf("%s/%s, Go %s", runtime.GOOS, runtime.GOARCH, runtime.Version()),
	})

	if d.FUSESupported != nil {
		ok, why := d.FUSESupported()
		c := Check{Name: "fuse"}
		if ok {
			c.Level, c.Detail = LevelOK, "the kernel FUSE device is present and usable"
		} else {
			c.Level, c.Detail = LevelFail, why
			switch runtime.GOOS {
			case "linux":
				c.Fix = "install the fuse3 package and make sure your user can open /dev/fuse (usually by joining the 'fuse' group or running as root)"
			case "darwin":
				c.Fix = "install macFUSE from https://macfuse.io and approve the system extension in System Settings, then reboot"
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
				c.Level, c.Detail = LevelOK, "other users may access the mount when allow_other is set"
			} else {
				c.Level = LevelWarn
				c.Detail = "user_allow_other is not enabled, so only your own user can read the mount"
				c.Fix = "add the line 'user_allow_other' to /etc/fuse.conf if another user or a container needs the mount"
			}
			out = append(out, c)
		}
		if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			rel := strings.TrimSpace(string(b))
			c := Check{Name: "kernel", Detail: "kernel " + rel}
			ok, why := false, "kernel older than 6.9"
			if d.Passthrough != nil {
				ok, why = d.Passthrough()
			} else if kernelAtLeast(rel, 6, 9) {
				ok = true
			}
			if ok {
				c.Level = LevelOK
				c.Detail += "; experimental passthrough is eligible; successful kernel negotiation is still required"
			} else {
				c.Level = LevelWarn
				c.Detail += "; FUSE passthrough is off (" + why + "); cached reads use the normal VFS path"
				c.Fix = "keep experimental passthrough disabled for normal workloads until mixed IO modes are validated"
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
			c.Level, c.Detail = LevelOK, "found "+fs
		} else {
			c.Level = LevelFail
			c.Detail = "neither macFUSE nor Fuse-T is installed"
			c.Fix = "install macFUSE from https://macfuse.io, or Fuse-T from https://www.fuse-t.org for a kext-free option"
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
		c.Detail = fmt.Sprintf("cannot create %s: %v", d.CacheDir, err)
		c.Fix = "point cache.dir at a writable location"
		return append(out, c)
	}
	probe := filepath.Join(d.CacheDir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		c.Level = LevelFail
		c.Detail = fmt.Sprintf("%s is not writable: %v", d.CacheDir, err)
		c.Fix = "fix the permissions on the cache directory or choose another one"
		return append(out, c)
	}
	os.Remove(probe)
	c.Level = LevelOK
	c.Detail = d.CacheDir + " is writable"
	out = append(out, c)

	if d.FreeSpace != nil {
		free, err := d.FreeSpace(d.CacheDir)
		fc := Check{Name: "cache_free_space"}
		switch {
		case err != nil:
			fc.Level = LevelWarn
			fc.Detail = fmt.Sprintf("cannot measure free space: %v", err)
		case d.MinFree > 0 && free < d.MinFree:
			fc.Level = LevelFail
			fc.Detail = fmt.Sprintf("%s free, below the configured minimum of %s; writes will fail with ENOSPC", humanBytes(free), humanBytes(d.MinFree))
			fc.Fix = "free disk space, lower cache.min_free, or run 'cloudfs cache gc'"
			fc.Fixable = true
		case free < 1<<30:
			fc.Level = LevelWarn
			fc.Detail = fmt.Sprintf("only %s free on the cache filesystem", humanBytes(free))
			fc.Fix = "free disk space or run 'cloudfs cache gc'"
			fc.Fixable = true
		default:
			fc.Level = LevelOK
			fc.Detail = humanBytes(free) + " free"
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
		c.Fix = "stop cloudfs and delete the metadata database; it is a cache and will rebuild from the remotes"
		return []Check{c}
	}
	st, err := d.Meta.Stats(ctx)
	if err != nil {
		c.Level = LevelFail
		c.Detail = err.Error()
		return []Check{c}
	}
	c.Level = LevelOK
	c.Detail = fmt.Sprintf("integrity ok, %d entries cached across %d directories", st.Nodes, st.CompleteDs)
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
		c.Detail = fmt.Sprintf("%d uploads failed permanently; their data is still on local disk", st.Dead)
		c.Fix = "run 'cloudfs uploads list' to see why, then 'cloudfs uploads retry' once the cause is fixed"
		c.Fixable = true
	case st.Purging > 0:
		c.Level = LevelWarn
		c.Detail = fmt.Sprintf("%d uploads have unfinished local cleanup; remote results are not reconciled", st.Purging)
		c.Fix = "inspect 'cloudfs uploads list'; coordinated local cleanup is required, not upload retry or doctor --fix"
	case st.Cancelled+st.Cancelling > 0:
		c.Level = LevelWarn
		c.Detail = fmt.Sprintf("%d uploads stopping, %d cancelled; local content retained, remote effects not reconciled", st.Cancelling, st.Cancelled)
		c.Fix = "inspect 'cloudfs uploads list'; cancellation is not remote rollback and cannot be reset by doctor --fix"
	case st.OldestAge > 30*time.Minute:
		c.Level = LevelWarn
		c.Detail = fmt.Sprintf("%d uploads queued, the oldest waiting %s", st.Pending+st.Uploading, st.OldestAge.Round(time.Second))
		c.Fix = "check the remote's health and rate limits with 'cloudfs status'"
	case st.Pending+st.Uploading > 0:
		c.Level = LevelOK
		c.Detail = fmt.Sprintf("%d uploads in flight, %s queued", st.Pending+st.Uploading, humanBytes(st.Bytes))
	default:
		c.Level = LevelOK
		c.Detail = "no queued uploads"
	}
	out = append(out, c)
	switch d.Journal.Durability() {
	case journal.DurabilityCrash:
		out = append(out, Check{Name: "durability", Level: LevelWarn,
			Detail: "crash: close() returns before data is fsynced; a power loss can lose the last seconds of writes (they are reported as dead letters)",
			Fix:    "set journal.durability: power if that trade is not wanted"})
	default:
		out = append(out, Check{Name: "durability", Level: LevelOK,
			Detail: "power: close() returns only once the data is fsynced to local disk"})
	}

	// Payloads no row names. Normally none: they appear when a crash lands
	// between removing a row and unlinking its object, and the next start
	// clears them. If they persist, the queue has stopped reclaiming content —
	// and these bytes are in no other report, being outside the cache budget
	// and outside the queued total.
	if n, held, err := d.Journal.OrphanObjects(ctx); err == nil && n > 0 {
		out = append(out, Check{
			Name:   "queue_objects",
			Level:  LevelWarn,
			Detail: fmt.Sprintf("%d upload payloads (%s) are on disk with no queue entry naming them", n, humanBytes(held)),
			Fix:    "restart the daemon; startup recovery reclaims them, and they are not counted against cache.max_size",
		})
	}

	// Orphan staging files mean a crash left partial writes behind.
	entries, err := os.ReadDir(d.Journal.StagingDir())
	if err == nil && len(entries) > 0 {
		out = append(out, Check{
			Name:    "staging_files",
			Level:   LevelWarn,
			Detail:  fmt.Sprintf("%d incomplete staging files from an earlier run", len(entries)),
			Fix:     "run 'cloudfs doctor --fix' to remove them; they are writes that never reached a commit",
			Fixable: true,
		})
	}
	return out
}

func (d *Doctor) checkProxy(ctx context.Context) []Check {
	if d.Proxy == nil {
		return nil
	}
	health := d.Proxy.CheckNow(ctx)
	if len(health) == 0 {
		return []Check{{Name: "proxy", Level: LevelOK, Detail: "no proxy groups configured; all traffic is direct"}}
	}
	var out []Check
	for _, h := range health {
		c := Check{Name: "proxy/" + h.Name}
		if h.Healthy {
			c.Level = LevelOK
			c.Detail = fmt.Sprintf("reachable, %dms", h.Latency.Milliseconds())
		} else {
			c.Level = LevelFail
			c.Detail = h.Err
			c.Fix = "check that the proxy is running and reachable; remotes routed through it will fail until it is"
		}
		out = append(out, c)
	}
	return out
}

// Fix repairs what it safely can and reports what it did.
func (d *Doctor) Fix(ctx context.Context) []string {
	var done []string
	if d.Journal != nil {
		// Remove staging files with no journal row: they are writes that never
		// reached a commit and can never be completed.
		if rec, err := d.Journal.Recover(ctx); err == nil {
			if n := len(rec.OrphanStaging); n > 0 {
				done = append(done, fmt.Sprintf("removed %d incomplete staging files", n))
			}
			if n := len(rec.Requeued); n > 0 {
				done = append(done, fmt.Sprintf("requeued %d uploads interrupted by a restart", n))
			}
			if n := len(rec.Lost); n > 0 {
				done = append(done, fmt.Sprintf("dead-lettered %d uploads whose local data is gone", n))
			}
		}
		if n, err := d.Journal.Purge(ctx, 24*time.Hour); err == nil && n > 0 {
			done = append(done, fmt.Sprintf("purged %d completed uploads older than a day", n))
		}
	}
	if d.Cache != nil {
		before := d.Cache.Stats()
		if err := d.Cache.GC(); err == nil {
			after := d.Cache.Stats()
			if freed := before.Bytes - after.Bytes; freed > 0 {
				done = append(done, fmt.Sprintf("evicted %s from the block cache", humanBytes(freed)))
			}
		}
	}
	if d.Meta != nil {
		if err := d.Meta.Vacuum(ctx); err == nil {
			done = append(done, "checkpointed the metadata database")
		}
	}
	if len(done) == 0 {
		done = append(done, "nothing needed fixing")
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
