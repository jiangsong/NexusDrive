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
	UploadWorkers int            `yaml:"upload_workers"`
	Extra         map[string]any `yaml:",inline"`
}

type Layout struct {
	Remote string        `yaml:"remote"`
	Root   string        `yaml:"root"`
	Mode   Mode          `yaml:"mode"`
	Pin    bool          `yaml:"pin"`
	DirTTL time.Duration `yaml:"dir_ttl"`
}

type Mount struct {
	Path   string            `yaml:"path"`
	Layout map[string]Layout `yaml:"layout"`
}

type MCP struct {
	HTTP     string   `yaml:"http"`
	Allow    []string `yaml:"allow"`
	ReadOnly bool     `yaml:"read_only"`
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

type Config struct {
	SourcePath string            `yaml:"-"`
	Secrets    Secrets           `yaml:"secrets"`
	Cache      Cache             `yaml:"cache"`
	Journal    Journal           `yaml:"journal"`
	Proxy      Proxy             `yaml:"proxy"`
	Remotes    map[string]Remote `yaml:"remotes"`
	Mounts     []Mount           `yaml:"mounts"`
	MCP        MCP               `yaml:"mcp"`
	Control    Control           `yaml:"control"`
	WebDAV     WebDAV            `yaml:"webdav"`
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
	}
	for _, m := range c.Mounts {
		if m.Path == "" {
			return fmt.Errorf("config: mount without path")
		}
		for sub, l := range m.Layout {
			if _, ok := c.Remotes[l.Remote]; !ok {
				return fmt.Errorf("config: mount %s%s references unknown remote %q", m.Path, sub, l.Remote)
			}
			switch l.Mode {
			case "", ModeWriteback, ModeStrict, ModeReadonly:
			default:
				return fmt.Errorf("config: mount %s%s has unknown mode %q", m.Path, sub, l.Mode)
			}
		}
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
