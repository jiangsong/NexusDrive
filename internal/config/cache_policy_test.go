package config

import (
	"strings"
	"testing"
	"time"
)

func TestResolveCachePolicyPresetDefaults(t *testing.T) {
	cases := []struct {
		preset string
		want   ResolvedCachePolicy
	}{
		// "", "none" and "code" all leave ReadaheadMax/ReadaheadRequest at 0
		// ("not configured"): vfs falls back to its own Options defaults for
		// them (documented as 64MiB / derive-per-mount), and baking those
		// defaults in here would make every daemon-built mount silently
		// override that fallback even with no cache.policy configured.
		{"", ResolvedCachePolicy{
			SmallFileThreshold: 4 << 20, DirReadahead: 32, ReadaheadMax: 0,
			ReadaheadRequest: 0, ReadaheadLead: 8 * time.Second,
		}},
		{"none", ResolvedCachePolicy{
			SmallFileThreshold: 4 << 20, DirReadahead: 32, ReadaheadMax: 0,
			ReadaheadRequest: 0, ReadaheadLead: 8 * time.Second,
		}},
		{"media", ResolvedCachePolicy{
			SmallFileThreshold: 4 << 20, DirReadahead: 0, ReadaheadMax: 128 << 20,
			ReadaheadRequest: 16 << 20, ReadaheadLead: 8 * time.Second,
		}},
		{"photos", ResolvedCachePolicy{
			SmallFileThreshold: 8 << 20, DirReadahead: 64, ReadaheadMax: 16 << 20,
			ReadaheadRequest: 0, ReadaheadLead: 8 * time.Second,
		}},
		// code is the one preset that turns SmallFileWhole on: the tools that
		// read a source tree jump rather than sweep, so the sibling prefetch
		// never arms for them and a file's tail would cost a second request.
		// The two thresholds differ on purpose: the megabyte is what the
		// speculative prefetch spends on a guess, the 8MiB is the bound on a
		// foreground whole-file fetch, which only pays above block_size.
		{"code", ResolvedCachePolicy{
			SmallFileWhole:          true,
			SmallFileThreshold:      1 << 20,
			SmallFileWholeThreshold: 8 << 20,
			DirReadahead:            128, ReadaheadMax: 0,
			ReadaheadRequest: 0, ReadaheadLead: 8 * time.Second,
		}},
	}
	for _, c := range cases {
		t.Run(c.preset, func(t *testing.T) {
			got, err := ResolveCachePolicy(CachePolicy{Preset: c.preset}, nil, 4<<20)
			if err != nil {
				t.Fatalf("ResolveCachePolicy(%q) error: %v", c.preset, err)
			}
			if got != c.want {
				t.Fatalf("ResolveCachePolicy(%q) = %+v, want %+v", c.preset, got, c.want)
			}
		})
	}
}

func TestResolveCachePolicyGlobalExplicitBeatsPreset(t *testing.T) {
	global := CachePolicy{Preset: "media", ReadaheadMax: mustSize(t, "32MiB")}
	got, err := ResolveCachePolicy(global, nil, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReadaheadMax != 32<<20 {
		t.Fatalf("global explicit readahead_max lost: %+v", got)
	}
	// Fields the global didn't touch keep the preset's value.
	if got.ReadaheadRequest != 16<<20 || got.DirReadahead != 0 {
		t.Fatalf("untouched preset fields changed: %+v", got)
	}
}

func TestResolveCachePolicyLayoutBeatsGlobalAndPreset(t *testing.T) {
	global := CachePolicy{Preset: "media", ReadaheadMax: mustSize(t, "32MiB")}
	dirReadahead := 7
	layout := &CachePolicy{DirReadahead: &dirReadahead}
	got, err := ResolveCachePolicy(global, layout, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.DirReadahead != 7 {
		t.Fatalf("layout explicit dir_readahead lost: %+v", got)
	}
	// Global explicit value still wins over the preset for the field the
	// layout left alone.
	if got.ReadaheadMax != 32<<20 {
		t.Fatalf("global explicit field overridden by preset re-applying: %+v", got)
	}
}

func TestResolveCachePolicyLayoutOwnPreset(t *testing.T) {
	global := CachePolicy{Preset: "media"}
	layout := &CachePolicy{Preset: "code"}
	got, err := ResolveCachePolicy(global, layout, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := ResolveCachePolicy(CachePolicy{Preset: "code"}, nil, 4<<20)
	if got != want {
		t.Fatalf("layout's own preset should win over the global one: %+v, want %+v", got, want)
	}
}

// TestCodePresetCanActuallyFetchWhole: the preset turns small_file_whole on,
// and vfs declines to take a file that fits in one block, so the preset's
// whole-file bound has to be above the default block size or the knob it turns
// on does nothing at all. It was 1MiB against a 4MiB block.
func TestCodePresetCanActuallyFetchWhole(t *testing.T) {
	const defaultBlockSize = 4 << 20
	got, err := ResolveCachePolicy(CachePolicy{Preset: "code"}, nil, defaultBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SmallFileWhole {
		t.Fatal("the code preset stopped turning small_file_whole on")
	}
	if int64(got.SmallFileWholeThreshold) <= defaultBlockSize {
		t.Fatalf("the code preset bounds whole-file fetches at %s, which is not above the default block size (%d): the foreground fetch can never fire",
			got.SmallFileWholeThreshold, defaultBlockSize)
	}
}

func TestResolveCachePolicyValidationErrors(t *testing.T) {
	cases := map[string]struct {
		global    CachePolicy
		layout    *CachePolicy
		blockSize int64
	}{
		"unknown global preset": {global: CachePolicy{Preset: "bogus"}, blockSize: 4 << 20},
		"unknown layout preset": {global: CachePolicy{}, layout: &CachePolicy{Preset: "bogus"}, blockSize: 4 << 20},
		"threshold not multiple of 64KiB": {
			global:    CachePolicy{SmallFileThreshold: mustSize(nil, "100KiB")},
			blockSize: 4 << 20,
		},
		"readahead_request not multiple of block_size": {
			global:    CachePolicy{ReadaheadRequest: mustSize(nil, "3MiB")},
			blockSize: 4 << 20,
		},
		"readahead_max below block_size": {
			global:    CachePolicy{ReadaheadMax: mustSize(nil, "1MiB")},
			blockSize: 4 << 20,
		},
		"whole_threshold not multiple of 64KiB": {
			global:    CachePolicy{SmallFileWholeThreshold: mustSize(nil, "100KiB")},
			blockSize: 4 << 20,
		},
		// vfs leaves a file that fits in one block to the ordinary path, which
		// already fetches it in one request, so a whole-file bound at or below
		// block_size can never fire. Refusing it beats resolving a knob that
		// silently does nothing — which is what the code preset's 1MiB
		// threshold did against the 4MiB default block size.
		"small_file_whole bounded at block_size": {
			global:    CachePolicy{SmallFileWhole: boolPtr(true), SmallFileThreshold: mustSize(nil, "4MiB")},
			blockSize: 4 << 20,
		},
		"small_file_whole bounded below block_size": {
			global: CachePolicy{
				SmallFileWhole:          boolPtr(true),
				SmallFileWholeThreshold: mustSize(nil, "1MiB"),
			},
			blockSize: 4 << 20,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveCachePolicy(c.global, c.layout, c.blockSize); err == nil {
				t.Fatalf("%s: expected an error", name)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func mustSize(t *testing.T, s string) Size {
	v, err := ParseSize(s)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	return v
}

func TestCachePolicyYAMLDecoding(t *testing.T) {
	c, err := Parse([]byte(`
cache:
  policy:
    preset: media
    small_file_whole: true
    small_file_whole_threshold: 8MiB
    dir_readahead: 10
    readahead_lead: 3s
remotes:
  a: {type: x}
mounts:
  - path: /m
    layout:
      /x: {remote: a, cache: {preset: code, small_file_threshold: 2MiB}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache.Policy.Preset != "media" {
		t.Fatalf("global preset not decoded: %+v", c.Cache.Policy)
	}
	if c.Cache.Policy.SmallFileWhole == nil || !*c.Cache.Policy.SmallFileWhole {
		t.Fatalf("small_file_whole not decoded: %+v", c.Cache.Policy)
	}
	if c.Cache.Policy.DirReadahead == nil || *c.Cache.Policy.DirReadahead != 10 {
		t.Fatalf("dir_readahead not decoded: %+v", c.Cache.Policy)
	}
	if c.Cache.Policy.ReadaheadLead != 3*time.Second {
		t.Fatalf("readahead_lead not decoded: %+v", c.Cache.Policy)
	}
	l := c.Mounts[0].Layout["/x"]
	if l.Cache == nil || l.Cache.Preset != "code" || l.Cache.SmallFileThreshold != 2<<20 {
		t.Fatalf("layout cache override not decoded: %+v", l.Cache)
	}
}

func TestLayoutCacheValidationErrorsSurfaceFromConfig(t *testing.T) {
	cases := map[string]string{
		"unknown preset on layout": "remotes: {a: {type: x}}\nmounts: [{path: /m, layout: {/x: {remote: a, cache: {preset: bogus}}}}]",
		"bad readahead_max":        "remotes: {a: {type: x}}\nmounts: [{path: /m, layout: {/x: {remote: a, cache: {readahead_max: 1KiB}}}}]",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPresetDirTTLAppliesOnlyWhenLayoutUnset(t *testing.T) {
	c, err := Parse([]byte(`
remotes:
  a: {type: x}
mounts:
  - path: /m
    layout:
      /media:   {remote: a, cache: {preset: media}}
      /code:    {remote: a, cache: {preset: code}}
      /explicit: {remote: a, cache: {preset: media}, dir_ttl: 5m}
`))
	if err != nil {
		t.Fatal(err)
	}
	l := c.Mounts[0].Layout
	if l["/media"].DirTTL != 24*time.Hour {
		t.Fatalf("media preset should default dir_ttl to 24h, got %s", l["/media"].DirTTL)
	}
	if l["/code"].DirTTL != time.Minute {
		t.Fatalf("code preset should default dir_ttl to 1m, got %s", l["/code"].DirTTL)
	}
	if l["/explicit"].DirTTL != 5*time.Minute {
		t.Fatalf("an explicit dir_ttl must not be overridden by the preset, got %s", l["/explicit"].DirTTL)
	}
}

func TestGlobalCachePolicyValidatedEvenWithoutMounts(t *testing.T) {
	_, err := Parse([]byte(`cache: {policy: {preset: bogus}}`))
	if err == nil || !strings.Contains(err.Error(), "preset") {
		t.Fatalf("an unknown global preset should fail config validation, got %v", err)
	}
}
