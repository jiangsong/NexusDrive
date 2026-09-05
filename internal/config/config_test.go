package config

import (
	"strings"
	"testing"
	"time"
)

const example = `
cache:
  dir: ~/.cache/cloudfs
  max_size: 200GiB
  min_free: 10GiB
  block_size: 4MiB
  max_age: 720h
proxy:
  outbounds:
    - { name: clash, type: socks5, addr: 127.0.0.1:7890 }
    - { name: vps,   type: http,   addr: http://user:pass@1.2.3.4:3128 }
  groups:
    - { name: proxy, type: fallback, members: [clash, vps],
        check_url: https://www.gstatic.com/generate_204, interval: 60s }
  rules:
    - DOMAIN-SUFFIX,googleapis.com,proxy
    - GEOIP,CN,direct
    - FINAL,direct
remotes:
  gdrive: { type: gdrive, proxy: proxy, client_id: abc }
  ali:    { type: aliyun }
  p115:   { type: pan115, qps: { meta: 1, download: 2, upload: 1 } }
mounts:
  - path: /mnt/cloud
    layout:
      /work:  { remote: ali,    root: /work,  mode: writeback, pin: true, dir_ttl: 1m }
      /gd:    { remote: gdrive, root: /,      mode: writeback }
      /media: { remote: p115,   root: /media, mode: readonly,  dir_ttl: 24h }
mcp:
  http: 127.0.0.1:8765
  allow: [/mnt/cloud/work]
control:
  metrics: 127.0.0.1:9101
`

func TestParseExample(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.MaxSize != 200<<30 || c.Cache.BlockSize != 4<<20 || c.Cache.MaxAge != 720*time.Hour {
		t.Fatalf("cache = %+v", c.Cache)
	}
	if strings.HasPrefix(c.Cache.Dir, "~") {
		t.Fatalf("home not expanded: %s", c.Cache.Dir)
	}
	if c.Remotes["gdrive"].Extra["client_id"] != "abc" {
		t.Fatalf("extra keys not preserved: %+v", c.Remotes["gdrive"])
	}
	if q := c.Remotes["p115"].QPS; q == nil || q.Meta != 1 || q.Download != 2 {
		t.Fatalf("qps = %+v", q)
	}
	if c.Mounts[0].Layout["/media"].Mode != ModeReadonly || c.Mounts[0].Layout["/work"].DirTTL != time.Minute {
		t.Fatalf("layout = %+v", c.Mounts[0].Layout)
	}
	if c.Proxy.Groups[0].Interval != 60*time.Second {
		t.Fatalf("group = %+v", c.Proxy.Groups[0])
	}
	if !c.Control.UI {
		t.Fatal("control UI should default to enabled")
	}
	if c.WebDAV.Prefix != "/dav" || c.WebDAV.Root != "/" || c.WebDAV.Strategy != "proxy" {
		t.Fatalf("webdav defaults = %+v", c.WebDAV)
	}
	disabled, err := Parse([]byte("control: {ui: false}"))
	if err != nil || disabled.Control.UI {
		t.Fatalf("explicitly disabled control UI = %+v, %v", disabled.Control, err)
	}
	direct, err := Parse([]byte("webdav: {http: '127.0.0.1:8080', root: /media, strategy: auto}"))
	if err != nil || direct.WebDAV.Strategy != "auto" || direct.WebDAV.Root != "/media" {
		t.Fatalf("webdav strategy = %+v, %v", direct.WebDAV, err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]string{
		"unknown remote":   "remotes: {a: {type: x}}\nmounts: [{path: /m, layout: {/x: {remote: b}}}]",
		"unknown proxy":    "remotes: {a: {type: x, proxy: nope}}",
		"unknown outbound": "proxy: {groups: [{name: g, type: fallback, members: [zzz]}]}",
		"bad rule target":  "proxy: {rules: [\"FINAL,ghost\"]}",
		"bad mode":         "remotes: {a: {type: x}}\nmounts: [{path: /m, layout: {/x: {remote: a, mode: fast}}}]",
		"bad block size":   "cache: {block_size: 100}",
		"unknown field":    "cache: {dirr: /x}",
		"webdav prefix":    "webdav: {http: '127.0.0.1:8080', prefix: /}",
		"webdav root":      "webdav: {http: '127.0.0.1:8080', root: relative}",
		"webdav traversal": "webdav: {http: '127.0.0.1:8080', root: /media/../secret}",
		"webdav strategy":  "webdav: {http: '127.0.0.1:8080', strategy: magic}",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]Size{"4MiB": 4 << 20, "1.5GiB": 3 << 29, "10GB": 10_000_000_000, "512": 512, "64k": 64 << 10} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("abc"); err == nil {
		t.Error("ParseSize(abc) should fail")
	}
}

func TestDurabilityMustBeKnown(t *testing.T) {
	c := Default()
	c.Journal.Durability = "maybe"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "durability") {
		t.Fatalf("an unknown durability passed validation: %v", err)
	}
	for _, d := range []string{"", "power", "crash"} {
		c.Journal.Durability = d
		if err := c.Validate(); err != nil && !strings.Contains(err.Error(), "remote") && !strings.Contains(err.Error(), "mount") {
			t.Fatalf("durability %q rejected: %v", d, err)
		}
	}
}
