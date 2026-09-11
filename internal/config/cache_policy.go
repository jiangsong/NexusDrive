package config

import (
	"fmt"
	"time"
)

// CachePolicy tunes readahead and small-file behaviour. It appears twice:
// as Cache.Policy (the global default) and as Layout.Cache (a per-prefix
// override). Explicit keys always beat the preset named by Preset, and a
// layout's explicit keys beat the global ones. SmallFileWhole and
// DirReadahead are pointers so "not set" (nil) is distinguishable from an
// explicit false/0; the Size and time.Duration fields use their zero value
// for "not set", which is why 0 is never a meaningful explicit override for
// them (a caller who wants readahead disabled sets Options.ReadAheadBlocks
// to 0 at the daemon level instead).
type CachePolicy struct {
	// Preset is one of "", "none", "media", "photos", "code". "" and "none"
	// both mean "no preset defaults, only the built-in defaults".
	Preset             string        `yaml:"preset"`
	SmallFileWhole     *bool         `yaml:"small_file_whole"`
	SmallFileThreshold Size          `yaml:"small_file_threshold"` // default 4MiB
	DirReadahead       *int          `yaml:"dir_readahead"`        // files ahead; default 32; 0 = off
	ReadaheadMax       Size          `yaml:"readahead_max"`        // default 64MiB
	ReadaheadRequest   Size          `yaml:"readahead_request"`    // default: derive per mount (Task 4)
	ReadaheadLead      time.Duration `yaml:"readahead_lead"`       // default 8s
}

// ResolvedCachePolicy is the effective, fully-defaulted cache policy for one
// mount prefix, after preset defaults and any global/layout overrides have
// been applied. It holds plain values (no pointers): daemon converts it,
// field by field, into vfs.CachePolicy when it builds each vfs.Mount, so
// that vfs never needs to import this package's option types.
type ResolvedCachePolicy struct {
	SmallFileWhole     bool
	SmallFileThreshold Size
	DirReadahead       int
	ReadaheadMax       Size
	ReadaheadRequest   Size
	ReadaheadLead      time.Duration
}

// knownCachePresets are the only legal values of CachePolicy.Preset.
var knownCachePresets = map[string]bool{
	"": true, "none": true, "media": true, "photos": true, "code": true,
}

// presetCachePolicy returns the built-in defaults for a preset name. "" and
// "none" (and any other value already rejected by validation) fall through
// to the plain defaults.
func presetCachePolicy(preset string) ResolvedCachePolicy {
	r := ResolvedCachePolicy{
		SmallFileThreshold: 4 << 20,
		DirReadahead:       32,
		ReadaheadMax:       64 << 20,
		ReadaheadLead:      8 * time.Second,
	}
	switch preset {
	case "media":
		r.ReadaheadMax = 128 << 20
		r.ReadaheadRequest = 16 << 20
		r.DirReadahead = 0
	case "photos":
		r.DirReadahead = 64
		r.SmallFileThreshold = 8 << 20
		r.ReadaheadMax = 16 << 20
	case "code":
		r.DirReadahead = 128
		r.SmallFileThreshold = 1 << 20
	}
	return r
}

// presetDirTTL is the layout dir_ttl a preset implies, applied only when the
// layout itself leaves dir_ttl unset (zero). Presets other than media and
// code make no claim on dir_ttl.
func presetDirTTL(preset string) time.Duration {
	switch preset {
	case "media":
		return 24 * time.Hour
	case "code":
		return time.Minute
	default:
		return 0
	}
}

// applyExplicitCachePolicy overlays the explicit fields of p (those that are
// non-nil / non-zero) onto r, which starts as either the preset baseline or
// the result of a previous, lower-precedence overlay.
func applyExplicitCachePolicy(r ResolvedCachePolicy, p CachePolicy) ResolvedCachePolicy {
	if p.SmallFileWhole != nil {
		r.SmallFileWhole = *p.SmallFileWhole
	}
	if p.SmallFileThreshold != 0 {
		r.SmallFileThreshold = p.SmallFileThreshold
	}
	if p.DirReadahead != nil {
		r.DirReadahead = *p.DirReadahead
	}
	if p.ReadaheadMax != 0 {
		r.ReadaheadMax = p.ReadaheadMax
	}
	if p.ReadaheadRequest != 0 {
		r.ReadaheadRequest = p.ReadaheadRequest
	}
	if p.ReadaheadLead != 0 {
		r.ReadaheadLead = p.ReadaheadLead
	}
	return r
}

// ResolveCachePolicy computes the effective cache policy for one mount
// prefix: preset defaults, then global's explicit keys, then layout's
// explicit keys (layout may also name its own preset, which then wins over
// global's for the baseline). blockSize is the cache's global block size
// (internal/config.Cache.BlockSize), used to validate readahead_request and
// readahead_max. It is pure and returns an error instead of a partially
// resolved value when the inputs (preset name, or the resolved values) are
// invalid.
func ResolveCachePolicy(global CachePolicy, layout *CachePolicy, blockSize int64) (ResolvedCachePolicy, error) {
	if !knownCachePresets[global.Preset] {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache.policy preset %q is unknown (want media, photos, code, or none)", global.Preset)
	}
	preset := global.Preset
	if layout != nil {
		if !knownCachePresets[layout.Preset] {
			return ResolvedCachePolicy{}, fmt.Errorf("config: layout cache preset %q is unknown (want media, photos, code, or none)", layout.Preset)
		}
		if layout.Preset != "" {
			preset = layout.Preset
		}
	}
	r := presetCachePolicy(preset)
	r = applyExplicitCachePolicy(r, global)
	if layout != nil {
		r = applyExplicitCachePolicy(r, *layout)
	}
	if r.SmallFileThreshold != 0 && r.SmallFileThreshold%(64<<10) != 0 {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache small_file_threshold must be a multiple of 64KiB, got %s", r.SmallFileThreshold)
	}
	if r.ReadaheadRequest != 0 && blockSize > 0 && int64(r.ReadaheadRequest)%blockSize != 0 {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache readahead_request must be a positive multiple of block_size (%d), got %s", blockSize, r.ReadaheadRequest)
	}
	if blockSize > 0 && int64(r.ReadaheadMax) < blockSize {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache readahead_max (%s) must be >= block_size (%d)", r.ReadaheadMax, blockSize)
	}
	return r, nil
}
