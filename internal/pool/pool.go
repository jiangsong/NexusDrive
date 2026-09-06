// Package pool fuses several remotes into one namespace with N-replica
// placement: a storage pool. It is a provider.Provider like any drive, so the
// VFS, the cache, the journal and the uploader treat it as one backend and
// never learn that a file lives on three of them.
//
// The members mirror the real directory tree: a file the pool shows at
// /photos/2026/a.jpg is stored at <root>/photos/2026/a.jpg on every member
// that holds a replica, with its real name, so the vendor's own app shows the
// same files. The members are the shared truth of the namespace; this
// package keeps only a rebuildable index of which member holds what.
//
// See docs/DESIGN.md §4.11 and docs/pool.md.
package pool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
)

// Type is the remote type under which a pool is registered.
const Type = config.PoolType

// Config keys the daemon uses to hand a pool its members and its settings,
// alongside the shared-machinery keys in package provider. Members are
// built first and passed in already instrumented, so their call counts show
// the pool's traffic per drive.
const (
	ConfigMembers  = "_pool_members"
	ConfigSettings = "_pool_settings"
	ConfigStateDir = "_pool_state_dir"
)

// Member is one backend of a pool as the daemon assembles it.
type Member struct {
	Name     string
	Provider provider.Provider
	// Root is the directory on the member the pool's tree is mirrored
	// under; empty means the member's root.
	Root     string
	Weight   float64
	Capacity int64
	Adopt    bool
}

// Options configures New.
type Options struct {
	Name     string
	Members  []Member
	Settings config.Pool
	// StateDir holds the pool's index database. Empty keeps it in memory,
	// which forgets ids across restarts — fine for a test, not for a mount.
	StateDir string
	Now      func() time.Time
}

// Pool is the composite provider.
type Pool struct {
	name     string
	settings config.Pool
	members  []*member
	byName   map[string]*member
	db       *sql.DB
	stateDir string
	now      func() time.Time
	// mu serialises index updates: one directory merge writes many rows and
	// two merges of the same directory must not interleave.
	mu sync.Mutex

	bg     sync.WaitGroup
	stopBG chan struct{}
	bgMu   sync.Mutex
}

// New assembles a pool over already-built members.
func New(opt Options) (*Pool, error) {
	if opt.Name == "" {
		return nil, errors.New("pool: name is required")
	}
	if len(opt.Members) == 0 {
		return nil, fmt.Errorf("pool %q: no members", opt.Name)
	}
	dbPath := ":memory:"
	if opt.StateDir != "" {
		dbPath = filepath.Join(opt.StateDir, "pool-"+opt.Name+".db")
	}
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	p := &Pool{name: opt.Name, settings: opt.Settings, db: db, stateDir: opt.StateDir, byName: map[string]*member{}, now: opt.Now}
	if p.now == nil {
		p.now = time.Now
	}
	for i, m := range opt.Members {
		if m.Provider == nil {
			db.Close()
			return nil, fmt.Errorf("pool %q: member %q has no provider", opt.Name, m.Name)
		}
		if _, dup := p.byName[m.Name]; dup {
			db.Close()
			return nil, fmt.Errorf("pool %q: member %q listed twice", opt.Name, m.Name)
		}
		root := m.Root
		if root == "" {
			root = "/"
		}
		root, err := cleanPath(root)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("pool %q: member %q: %w", opt.Name, m.Name, err)
		}
		mm := &member{name: m.Name, p: m.Provider, root: root, weight: m.Weight, capacity: m.Capacity, adopt: m.Adopt, order: i, dirIDs: map[string]string{},
			health: provider.NewHealth(provider.HealthOptions{Threshold: 3, OutAfter: opt.Settings.OutAfter, Now: p.now})}
		if mm.weight <= 0 {
			mm.weight = 1
		}
		p.members = append(p.members, mm)
		p.byName[m.Name] = mm
	}
	return p, nil
}

// Close stops the background loops and releases the index database.
func (p *Pool) Close() error {
	p.Stop()
	return p.db.Close()
}

// probeInterval is how often a member that is down is asked again.
func (p *Pool) probeInterval() time.Duration { return p.settings.ProbeInterval }

// MemberStatus is one member as the control plane sees it.
type MemberStatus struct {
	Name      string
	Root      string
	Health    provider.HealthSnapshot
	LatencyMS float64
	Weight    float64
	Capacity  int64
}

// Status reports every member's health, in declaration order.
func (p *Pool) Status() []MemberStatus {
	out := make([]MemberStatus, 0, len(p.members))
	for _, m := range p.members {
		m.mu.Lock()
		lat := m.latency / 1e6
		m.mu.Unlock()
		out = append(out, MemberStatus{Name: m.name, Root: m.root, Health: m.health.Snapshot(), LatencyMS: lat, Weight: m.weight, Capacity: m.capacity})
	}
	return out
}

func (p *Pool) Name() string { return p.name }

// RootID is the id the daemon lists a mount of this pool from.
func (p *Pool) RootID() string { return rootID }

// Capabilities derives the pool's matrix from its members: it can do what
// every member can do, and its rate budget is the sum of theirs, because the
// members each keep their own limiter and the pool's must not throttle
// below what they allow together.
func (p *Pool) Capabilities() provider.Caps {
	c := provider.Caps{
		StreamList:   true,
		ServerMove:   true,
		ServerRename: true,
		RangeRead:    true,
		Tier:         provider.TierOfficial,
	}
	hashes := map[provider.HashType]bool{}
	rapid := map[provider.HashType]bool{provider.HashSHA1: true}
	first := true
	for _, m := range p.members {
		mc := m.p.Capabilities()
		for _, h := range mc.HashTypes {
			hashes[h] = true
		}
		for _, h := range mc.RapidUpload {
			rapid[h] = true
		}
		if !mc.RangeRead {
			c.RangeRead = false
		}
		if mc.PartSize > c.PartSize {
			c.PartSize = mc.PartSize
		}
		if first || (mc.MaxParts > 0 && mc.MaxParts < c.MaxParts) {
			c.MaxParts = mc.MaxParts
		}
		if first || (mc.UploadParallel > 0 && mc.UploadParallel < c.UploadParallel) {
			c.UploadParallel = mc.UploadParallel
		}
		if first || (mc.LinkTTL > 0 && mc.LinkTTL < c.LinkTTL) {
			c.LinkTTL = mc.LinkTTL
		}
		if first {
			c.LinkShareable = mc.LinkShareable
		} else if !mc.LinkShareable {
			c.LinkShareable = false
		}
		c.QPS.Meta += mc.QPS.Meta
		c.QPS.Download += mc.QPS.Download
		c.QPS.Upload += mc.QPS.Upload
		c.MaxConnsPerHost += mc.MaxConnsPerHost
		first = false
	}
	c.HashTypes = sortedHashes(hashes)
	c.RapidUpload = sortedHashes(rapid)
	if c.PartSize == 0 {
		c.PartSize = 4 << 20
	}
	if c.MaxParts == 0 {
		c.MaxParts = 10000
	}
	if c.UploadParallel == 0 {
		c.UploadParallel = 2
	}
	return c
}

func sortedHashes(set map[provider.HashType]bool) []provider.HashType {
	out := make([]provider.HashType, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Members reports the member names in declaration order.
func (p *Pool) Members() []string {
	out := make([]string, len(p.members))
	for i, m := range p.members {
		out[i] = m.name
	}
	return out
}

func init() {
	// A pool has no credentials of its own: its members carry theirs. The
	// one key on the remote is the pool it exposes; the pool's settings
	// (members, replicas…) live under `pools:` so that adding a member
	// never changes the remote's identity.
	provider.RegisterFields(Type, []provider.Field{
		{Name: "pool", Prompt: "Pool name (its members and replica count live under pools.<name>)", Required: true, Example: "home"},
	}, provider.Credentials{Note: "none: a pool authenticates through its member remotes"})
	provider.Register(Type, func(name string, cfg map[string]any) (provider.Provider, error) {
		members, _ := cfg[ConfigMembers].(map[string]provider.Provider)
		settings, _ := cfg[ConfigSettings].(config.Pool)
		stateDir, _ := cfg[ConfigStateDir].(string)
		if len(settings.Members) == 0 {
			return nil, fmt.Errorf("pool %q: no settings; the daemon assembles pools after their members", name)
		}
		opt := Options{Name: name, Settings: settings, StateDir: stateDir}
		for _, m := range settings.Members {
			mp, ok := members[m.Remote]
			if !ok {
				return nil, fmt.Errorf("pool %q: member %q was not built", name, m.Remote)
			}
			adopt := true
			if m.Adopt != nil {
				adopt = *m.Adopt
			}
			opt.Members = append(opt.Members, Member{Name: m.Remote, Provider: mp, Root: m.Root, Weight: m.Weight, Capacity: int64(m.Capacity), Adopt: adopt})
		}
		return New(opt)
	})
}

var (
	_ provider.Provider      = (*Pool)(nil)
	_ provider.StreamLister  = (*Pool)(nil)
	_ provider.RangeReaderAt = (*Pool)(nil)
)

// unused-import guard for context in this file's future methods.
var _ = context.Background
