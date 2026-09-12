// Package config parses and validates ~/.config/cloudfs/config.yaml. The
// schema mirrors docs/DESIGN.md §6.
package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Size is a byte count that accepts "4MiB", "200GiB", "10GB", "1024".
type Size int64

var units = map[string]int64{
	"":  1,
	"b": 1,
	"k": 1 << 10, "kib": 1 << 10, "kb": 1000,
	"m": 1 << 20, "mib": 1 << 20, "mb": 1000 * 1000,
	"g": 1 << 30, "gib": 1 << 30, "gb": 1000 * 1000 * 1000,
	"t": 1 << 40, "tib": 1 << 40, "tb": 1000 * 1000 * 1000 * 1000,
}

// ParseSize converts a human size string to bytes.
func ParseSize(s string) (Size, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	if num == "" {
		return 0, fmt.Errorf("config: invalid size %q", s)
	}
	mult, ok := units[unit]
	if !ok {
		return 0, fmt.Errorf("config: unknown size unit %q", unit)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("config: invalid size %q: %w", s, err)
	}
	return Size(v * float64(mult)), nil
}

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

func (s Size) String() string {
	switch {
	case s >= 1<<40:
		return fmt.Sprintf("%.1fTiB", float64(s)/(1<<40))
	case s >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(s)/(1<<30))
	case s >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(s)/(1<<20))
	case s >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(s)/(1<<10))
	}
	return strconv.FormatInt(int64(s), 10) + "B"
}

// Mode is a per-mount consistency mode.
type Mode string

const (
	ModeWriteback Mode = "writeback"
	ModeStrict    Mode = "strict"
	ModeReadonly  Mode = "readonly"
)

type Cache struct {
	Dir       string `yaml:"dir"`
	MaxSize   Size   `yaml:"max_size"`
	MinFree   Size   `yaml:"min_free"`
	BlockSize Size   `yaml:"block_size"`
	// SubBlockSize is the granularity a random read is fetched at when its
	// block is not cached. The kernel asks for 16 KiB at a time on a random
	// read, so that is the default; a whole block is what a sequential
	// reader gets.
	SubBlockSize Size `yaml:"sub_block_size"`
	// WriteBehind bounds the memory that holds fetched blocks until the
	// background writers have put them on disk (0 = 256 MiB).
	WriteBehind Size          `yaml:"write_behind"`
	MaxAge      time.Duration `yaml:"max_age"`
	// Policy is the global cache policy default; a layout's own `cache` key
	// overrides it per prefix. See CachePolicy and ResolveCachePolicy.
	Policy CachePolicy `yaml:"policy"`
}

// Journal configures the local write journal.
type Journal struct {
	// Durability is what close(2) promises once it returns.
	//   power: the data is fsynced to local disk first (default).
	//   crash: the data is in the kernel's page cache and the journal row is
	//          committed; a daemon crash or kill -9 loses nothing, a power
	//          loss may lose the last seconds and reports them as dead
	//          letters. This is the semantics of JuiceFS --writeback and
	//          rclone --vfs-cache-mode writes.
	Durability string `yaml:"durability"`
}

type Outbound struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // direct | http | socks5
	Addr string `yaml:"addr"`
}

type Group struct {
	Name     string        `yaml:"name"`
	Type     string        `yaml:"type"` // fallback | url-test
	Members  []string      `yaml:"members"`
	CheckURL string        `yaml:"check_url"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

type Proxy struct {
	Outbounds []Outbound `yaml:"outbounds"`
	Groups    []Group    `yaml:"groups"`
	Rules     []string   `yaml:"rules"`
}

type QPS struct {
	Meta     float64 `yaml:"meta"`
	Download float64 `yaml:"download"`
	Upload   float64 `yaml:"upload"`
	// Transfer overrides the CDN byte-stream GET rate. Zero (the default)
	// means it shares the Download bucket instead of getting its own.
	Transfer float64 `yaml:"transfer"`
}

// Remote is one backend account. Unknown keys are kept in Extra and handed to
// the provider factory.
type Remote struct {
	Type  string `yaml:"type"`
	Proxy string `yaml:"proxy"`
	QPS   *QPS   `yaml:"qps"`
	// AccountBinding is a local authorization generation, not a provider user
	// id or secret. Runtime also fingerprints non-secret account locator fields.
	// Explicit reauthorization rotates it; automatic token refresh preserves it.
	AccountBinding string `yaml:"account_binding,omitempty"`
	// UploadWorkers overrides the backend's recommended upload concurrency.
	UploadWorkers int `yaml:"upload_workers"`
	// MaxConns overrides the backend's advertised Caps.MaxConnsPerHost,
	// bounding both idle and in-flight HTTP connections to this remote. Zero
	// (the default) leaves the driver's own limit in effect.
	MaxConns int            `yaml:"max_conns"`
	Extra    map[string]any `yaml:",inline"`
}

type Layout struct {
	Remote string        `yaml:"remote"`
	Root   string        `yaml:"root"`
	Mode   Mode          `yaml:"mode"`
	Pin    bool          `yaml:"pin"`
	DirTTL time.Duration `yaml:"dir_ttl"`
	// Cache overrides cache.policy for this prefix; nil means "use the
	// global policy unchanged". See CachePolicy and ResolveCachePolicy.
	Cache *CachePolicy `yaml:"cache"`
}

type Mount struct {
	Path   string            `yaml:"path"`
	Layout map[string]Layout `yaml:"layout"`
}

type MCP struct {
	HTTP     string   `yaml:"http"`
	Allow    []string `yaml:"allow"`
	ReadOnly bool     `yaml:"read_only"`
	// ExportRoots bounds the MCP export tool: a destination outside every
	// one of these local directories is refused. Allow is about what an
	// agent may read out of the mount; this is about where it may write on
	// this machine, which is a different question and needs its own answer.
	// An empty list means the tool refuses every destination.
	ExportRoots []string `yaml:"export_roots"`
}

type Control struct {
	Socket  string `yaml:"socket"`
	Metrics string `yaml:"metrics"`
	UI      bool   `yaml:"ui"`
}

// WebDAV exposes one VFS subtree through a WebDAV endpoint. Token material is
// intentionally not part of YAML; the server reads it from the
// CLOUDFS_WEBDAV_TOKEN environment variable when enabled.
//
// Writable defaults to false. An endpoint that can delete a subtree is a
// different exposure from one that can only read it, so enabling the mutating
// verbs is a decision the configuration has to state.
type WebDAV struct {
	HTTP     string `yaml:"http"`
	Prefix   string `yaml:"prefix"`
	Root     string `yaml:"root"`
	Strategy string `yaml:"strategy"`
	Writable bool   `yaml:"writable"`
}

// PoolMember is one backend that stores replicas for a storage pool.
type PoolMember struct {
	Remote string `yaml:"remote"`
	// Root is the directory on the member under which the pool mirrors its
	// tree. Empty means the member's own root.
	Root string `yaml:"root"`
	// Weight biases placement between members of equal health and space.
	Weight float64 `yaml:"weight"`
	// Capacity is the member's total space when the backend cannot report
	// it. Zero means unknown.
	Capacity Size `yaml:"capacity"`
	// Adopt lets files already on the member enter the namespace as
	// single-replica files. Nil means true: adding a drive adds its content.
	Adopt *bool `yaml:"adopt"`
	// Class labels this member for PoolRule's prefer/avoid/require. A name
	// used by any rule must appear on at least one member.
	Class []string `yaml:"class"`
}

// Pool fuses several remotes into one namespace with N-replica placement.
// It is referenced from a remote of type "pool" through that remote's `pool`
// key; the pool settings live here, outside the remote's Extra, so that
// adding a member never changes the remote's account binding.
type Pool struct {
	Members     []PoolMember `yaml:"members"`
	Replicas    int          `yaml:"replicas"`
	MinReplicas int          `yaml:"min_replicas"`
	// OutAfter is how long a member stays down before it is declared out
	// and its replicas are rebuilt elsewhere.
	OutAfter          time.Duration `yaml:"out_after"`
	ProbeInterval     time.Duration `yaml:"probe_interval"`
	GCGrace           time.Duration `yaml:"gc_grace"`
	OpTTL             time.Duration `yaml:"op_ttl"`
	TrimGrace         time.Duration `yaml:"trim_grace"`
	HoldMaxBytes      Size          `yaml:"hold_max_bytes"`
	HoldMaxAge        time.Duration `yaml:"hold_max_age"`
	RepairConcurrency int           `yaml:"repair_concurrency"`
	ScrubInterval     time.Duration `yaml:"scrub_interval"`
	ScrubSample       float64       `yaml:"scrub_sample"`
	// ReadFanout decides how block reads of one file spread across its
	// replicas: off (one ordered stream, the v1 behaviour), auto (spread by
	// load and latency, but a member on an unofficial API serves at most
	// one stream per file) or all (spread with no tier restriction).
	ReadFanout string `yaml:"read_fanout"`
	// Rules override replicas and placement bias per path prefix; the
	// longest matching prefix applies, and the pool's own settings above are
	// the implicit default rule. See docs/pool-v2.md §6.1.
	Rules []PoolRule `yaml:"rules"`
	// FailureDomain is what "spread across" means when placing replicas:
	// account (default), provider, or member. See PoolFailureDomain*.
	FailureDomain string `yaml:"failure_domain"`
	// WriteMode is relaxed (default: min_replicas is an alert threshold) or
	// strict (close() waits up to MinReplicasTimeout for it). See
	// PoolWriteMode*.
	WriteMode string `yaml:"write_mode"`
	// MinReplicasTimeout bounds how long a strict-mode close() waits for
	// min_replicas before returning anyway.
	MinReplicasTimeout time.Duration `yaml:"min_replicas_timeout"`
	// Rebalance configures automatic backfill and skew correction.
	Rebalance PoolRebalance `yaml:"rebalance"`
}

// PoolType is the remote type that exposes a Pool as a backend.
const PoolType = "pool"

// Values of Pool.ReadFanout.
const (
	ReadFanoutOff  = "off"
	ReadFanoutAuto = "auto"
	ReadFanoutAll  = "all"
)

// PoolOf returns the pool a remote refers to, or "" when it is not a pool.
func (r Remote) PoolOf() string {
	if r.Type != PoolType {
		return ""
	}
	name, _ := r.Extra["pool"].(string)
	return name
}

type Config struct {
	SourcePath string            `yaml:"-"`
	Secrets    Secrets           `yaml:"secrets"`
	Cache      Cache             `yaml:"cache"`
	Journal    Journal           `yaml:"journal"`
	Proxy      Proxy             `yaml:"proxy"`
	Remotes    map[string]Remote `yaml:"remotes"`
	Pools      map[string]Pool   `yaml:"pools"`
	Mounts     []Mount           `yaml:"mounts"`
	MCP        MCP               `yaml:"mcp"`
	Control    Control           `yaml:"control"`
	WebDAV     WebDAV            `yaml:"webdav"`
	Export     Export            `yaml:"export"`
}

// Default returns the built-in defaults applied before the file is decoded.
func Default() Config {
	return Config{
		Cache: Cache{
			Dir:          "~/.cache/cloudfs",
			MaxSize:      50 << 30,
			MinFree:      5 << 30,
			BlockSize:    4 << 20,
			SubBlockSize: 16 << 10,
			MaxAge:       30 * 24 * time.Hour,
		},
		Journal: Journal{Durability: "power"},
		Control: Control{Socket: "~/.cache/cloudfs/control.sock", UI: true},
		WebDAV:  WebDAV{Prefix: "/dav", Root: "/", Strategy: "proxy"},
		Export:  DefaultExport(),
	}
}

// Load reads, decodes and validates the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(ExpandHome(path))
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err == nil {
		c.SourcePath, err = filepath.Abs(ExpandHome(path))
	}
	return c, err
}

// Parse decodes YAML bytes over Default() and validates the result.
func Parse(b []byte) (*Config, error) {
	c := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.Cache.Dir = ExpandHome(c.Cache.Dir)
	c.Control.Socket = ExpandHome(c.Control.Socket)
	// A mount path is a path like the two above, and a person writing one by
	// hand has no reason to expect it to be the exception. Left unexpanded,
	// "~/CloudFS" made the daemon create a directory literally named "~" in
	// whatever it was started from, while ~/CloudFS in the shell stayed empty.
	for i := range c.Mounts {
		c.Mounts[i].Path = ExpandHome(c.Mounts[i].Path)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks cross references: mounts → remotes, remotes → proxy names,
// groups → outbounds, modes, block size.
func (c *Config) Validate() error {
	if c.WebDAV.Prefix == "" {
		c.WebDAV.Prefix = "/dav"
	}
	if c.WebDAV.Root == "" {
		c.WebDAV.Root = "/"
	}
	if c.WebDAV.Strategy == "" {
		c.WebDAV.Strategy = "proxy"
	}
	if c.WebDAV.Strategy != "proxy" && c.WebDAV.Strategy != "redirect" && c.WebDAV.Strategy != "auto" {
		return fmt.Errorf("config: webdav.strategy must be proxy, redirect, or auto")
	}
	if !strings.HasPrefix(c.WebDAV.Prefix, "/") || path.Clean(c.WebDAV.Prefix) != c.WebDAV.Prefix || c.WebDAV.Prefix == "/" || strings.ContainsAny(c.WebDAV.Prefix, "\x00\\") {
		return fmt.Errorf("config: webdav.prefix must be a canonical non-root URL path")
	}
	if !strings.HasPrefix(c.WebDAV.Root, "/") || path.Clean(c.WebDAV.Root) != c.WebDAV.Root || strings.ContainsAny(c.WebDAV.Root, "\x00\\") {
		return fmt.Errorf("config: webdav.root must be a canonical absolute virtual path")
	}
	switch c.Secrets.Backend {
	case "", "auto", "keyring", "file":
	default:
		return fmt.Errorf("config: secrets.backend must be auto, keyring, or file")
	}
	switch c.Journal.Durability {
	case "", "power", "crash":
	default:
		return fmt.Errorf("config: journal.durability must be power or crash, got %q", c.Journal.Durability)
	}
	if c.Cache.SubBlockSize > 0 && (c.Cache.SubBlockSize%(4<<10) != 0 || c.Cache.BlockSize%c.Cache.SubBlockSize != 0) {
		return fmt.Errorf("config: cache.sub_block_size must be a multiple of 4KiB that divides block_size, got %s", c.Cache.SubBlockSize)
	}
	if c.Cache.BlockSize <= 0 || c.Cache.BlockSize%(64<<10) != 0 {
		return fmt.Errorf("config: cache.block_size must be a positive multiple of 64KiB, got %s", c.Cache.BlockSize)
	}
	if err := c.Export.Validate(); err != nil {
		return err
	}
	// Validate the global cache policy on its own, so an unknown preset or a
	// bad size fails even for a config with no mounts yet.
	if _, err := ResolveCachePolicy(c.Cache.Policy, nil, int64(c.Cache.BlockSize)); err != nil {
		return err
	}
	names := map[string]bool{"direct": true}
	for _, o := range c.Proxy.Outbounds {
		switch o.Type {
		case "direct", "http", "socks5":
		default:
			return fmt.Errorf("config: outbound %q has unknown type %q", o.Name, o.Type)
		}
		if o.Name == "" {
			return fmt.Errorf("config: outbound without name")
		}
		if o.Type != "direct" && o.Addr == "" {
			return fmt.Errorf("config: outbound %q needs addr", o.Name)
		}
		names[o.Name] = true
	}
	for _, g := range c.Proxy.Groups {
		if g.Type != "fallback" && g.Type != "url-test" {
			return fmt.Errorf("config: group %q has unknown type %q", g.Name, g.Type)
		}
		for _, m := range g.Members {
			if !names[m] {
				return fmt.Errorf("config: group %q references unknown outbound %q", g.Name, m)
			}
		}
		names[g.Name] = true
	}
	for i, r := range c.Proxy.Rules {
		parts := strings.Split(r, ",")
		if len(parts) < 2 {
			return fmt.Errorf("config: rule %d %q is malformed", i, r)
		}
		target := parts[len(parts)-1]
		if !names[target] {
			return fmt.Errorf("config: rule %d targets unknown outbound %q", i, target)
		}
	}
	for name, r := range c.Remotes {
		if r.Type == "" {
			return fmt.Errorf("config: remote %q has no type", name)
		}
		if r.AccountBinding != "" && !ValidAccountBinding(r.AccountBinding) {
			return fmt.Errorf("config: remote %q has an invalid account_binding", name)
		}
		if r.Proxy != "" && !names[r.Proxy] {
			return fmt.Errorf("config: remote %q references unknown proxy %q", name, r.Proxy)
		}
		if r.MaxConns < 0 {
			return fmt.Errorf("config: remote %q has a negative max_conns", name)
		}
	}
	if err := c.validatePools(); err != nil {
		return err
	}
	mountedAt := make(map[string]bool, len(c.Mounts))
	for _, m := range c.Mounts {
		if m.Path == "" {
			return fmt.Errorf("config: mount without path")
		}
		// Two entries for one directory is never deliberate, and it fails
		// silently: cloudfs mount takes Mounts[0], so everything bound under
		// the second entry is invisible with nothing reported anywhere. The
		// comparison is on the expanded path because ~/CloudFS and its
		// expansion are the same directory written two ways.
		canonical := ExpandHome(m.Path)
		if mountedAt[canonical] {
			return fmt.Errorf("config: %s is mounted twice; one mount entry per directory, with one layout holding every prefix", m.Path)
		}
		mountedAt[canonical] = true
		for sub, l := range m.Layout {
			if _, ok := c.Remotes[l.Remote]; !ok {
				return fmt.Errorf("config: mount %s%s references unknown remote %q", m.Path, sub, l.Remote)
			}
			switch l.Mode {
			case "", ModeWriteback, ModeStrict, ModeReadonly:
			default:
				return fmt.Errorf("config: mount %s%s has unknown mode %q", m.Path, sub, l.Mode)
			}
			if _, err := ResolveCachePolicy(c.Cache.Policy, l.Cache, int64(c.Cache.BlockSize)); err != nil {
				return fmt.Errorf("config: mount %s%s: %w", m.Path, sub, err)
			}
			// A media/code preset implies a dir_ttl, but only when the
			// layout did not already set one of its own.
			if l.DirTTL == 0 {
				preset := c.Cache.Policy.Preset
				if l.Cache != nil && l.Cache.Preset != "" {
					preset = l.Cache.Preset
				}
				if d := presetDirTTL(preset); d != 0 {
					l.DirTTL = d
					m.Layout[sub] = l
				}
			}
		}
	}
	return nil
}

// validatePools checks the pools section and the remotes that expose them,
// and fills in the pool defaults. A pool remote must name an existing pool,
// each pool is exposed by at most one remote, members are ordinary remotes
// (never pools) and belong to at most one pool.
func (c *Config) validatePools() error {
	exposed := map[string]string{}
	for name, r := range c.Remotes {
		if r.Type != PoolType {
			continue
		}
		pool := r.PoolOf()
		if pool == "" {
			return fmt.Errorf("config: remote %q is a pool but names no pool (set `pool: <name>`)", name)
		}
		if _, ok := c.Pools[pool]; !ok {
			return fmt.Errorf("config: remote %q references unknown pool %q", name, pool)
		}
		if other, dup := exposed[pool]; dup {
			return fmt.Errorf("config: pool %q is exposed by both %q and %q", pool, other, name)
		}
		exposed[pool] = name
		for k := range r.Extra {
			if k != "pool" {
				return fmt.Errorf("config: remote %q: pool settings belong under pools.%s, not on the remote (%q)", name, pool, k)
			}
		}
	}
	owner := map[string]string{}
	for name, p := range c.Pools {
		if len(p.Members) == 0 {
			return fmt.Errorf("config: pool %q has no members", name)
		}
		seen := map[string]bool{}
		for _, m := range p.Members {
			mr, ok := c.Remotes[m.Remote]
			if !ok {
				return fmt.Errorf("config: pool %q references unknown remote %q", name, m.Remote)
			}
			if mr.Type == PoolType {
				return fmt.Errorf("config: pool %q member %q is itself a pool; pools do not nest", name, m.Remote)
			}
			if seen[m.Remote] {
				return fmt.Errorf("config: pool %q lists member %q twice", name, m.Remote)
			}
			seen[m.Remote] = true
			if other, dup := owner[m.Remote]; dup {
				return fmt.Errorf("config: remote %q belongs to both pool %q and pool %q", m.Remote, other, name)
			}
			owner[m.Remote] = name
			if m.Root != "" && (!strings.HasPrefix(m.Root, "/") || path.Clean(m.Root) != m.Root || strings.ContainsAny(m.Root, "\x00\\")) {
				return fmt.Errorf("config: pool %q member %q root must be a canonical absolute path", name, m.Remote)
			}
			if m.Weight < 0 {
				return fmt.Errorf("config: pool %q member %q has a negative weight", name, m.Remote)
			}
		}
		if p.Replicas == 0 {
			p.Replicas = 3
		}
		if p.MinReplicas == 0 {
			p.MinReplicas = 1
		}
		if p.Replicas < 1 || p.MinReplicas < 1 || p.MinReplicas > p.Replicas {
			return fmt.Errorf("config: pool %q needs replicas >= min_replicas >= 1 (got %d, %d)", name, p.Replicas, p.MinReplicas)
		}
		if p.OutAfter == 0 {
			p.OutAfter = 10 * time.Minute
		}
		if p.ProbeInterval == 0 {
			p.ProbeInterval = 30 * time.Second
		}
		if p.GCGrace == 0 {
			p.GCGrace = 24 * time.Hour
		}
		if p.OpTTL == 0 {
			p.OpTTL = 7 * 24 * time.Hour
		}
		if p.TrimGrace == 0 {
			p.TrimGrace = time.Hour
		}
		if p.HoldMaxBytes == 0 {
			p.HoldMaxBytes = 8 << 30
		}
		if p.HoldMaxAge == 0 {
			p.HoldMaxAge = 24 * time.Hour
		}
		if p.RepairConcurrency == 0 {
			p.RepairConcurrency = 1
		}
		if p.ScrubInterval == 0 {
			p.ScrubInterval = 24 * time.Hour
		}
		if p.ScrubSample == 0 {
			p.ScrubSample = 0.05
		}
		if p.ScrubSample < 0 || p.ScrubSample > 1 {
			return fmt.Errorf("config: pool %q scrub_sample must be within [0, 1]", name)
		}
		switch p.ReadFanout {
		case "":
			p.ReadFanout = ReadFanoutAuto
		case ReadFanoutOff, ReadFanoutAuto, ReadFanoutAll:
		default:
			return fmt.Errorf("config: pool %q read_fanout must be off, auto or all (got %q)", name, p.ReadFanout)
		}
		if err := validatePoolPlacement(name, &p); err != nil {
			return err
		}
		c.Pools[name] = p
	}
	return nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
