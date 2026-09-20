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
//
// ReadaheadMax and ReadaheadRequest are special: vfs.Mount.Policy carries
// them straight through to vfs (see ResolvedCachePolicy), and 0 there means
// "not configured — fall back to vfs's own Options.ReadAheadBlocks /
// Options.ReadaheadRequest (CLOUDFS_READAHEAD_BLOCKS /
// CLOUDFS_READAHEAD_REQUEST)". So the "none"/"" preset baseline must leave
// both at 0 rather than baking in a resolved default: doing otherwise would
// make every daemon-built mount permanently override the global tuning,
// silently, even with no cache.policy configured at all.
type CachePolicy struct {
	// Preset is one of "", "none", "media", "photos", "code". "" and "none"
	// both mean "no preset defaults, only the built-in defaults".
	Preset             string `yaml:"preset"`
	SmallFileWhole     *bool  `yaml:"small_file_whole"`
	SmallFileThreshold Size   `yaml:"small_file_threshold"` // default 4MiB
	// SmallFileWholeThreshold bounds the foreground whole-file fetch alone.
	// It exists because the two consumers of SmallFileThreshold want opposite
	// things from it. The sibling prefetch wants it small: it is speculative,
	// and a source tree is worth prefetching a megabyte at a time. The
	// foreground fetch only ever pays off above cache.block_size — a file that
	// fits in one block is already one request, and vfs declines to take it
	// whole — so a threshold at or below the block size turns that half of
	// small_file_whole off entirely. 0 means "use SmallFileThreshold".
	SmallFileWholeThreshold Size `yaml:"small_file_whole_threshold"`
	DirReadahead            *int `yaml:"dir_readahead"` // files ahead; default 32; 0 = off
	// ReadaheadMax and ReadaheadRequest: 0 means "not configured"; vfs then
	// falls back to Options.ReadAheadBlocks / Options.ReadaheadRequest,
	// whose own defaults are effectively 64MiB (16 blocks * the 4MiB default
	// block size) and "derive per mount" (Task 4) respectively. Setting
	// either here (directly or via a preset) always overrides that fallback.
	ReadaheadMax     Size          `yaml:"readahead_max"`
	ReadaheadRequest Size          `yaml:"readahead_request"`
	ReadaheadLead    time.Duration `yaml:"readahead_lead"` // default 8s
}

// ResolvedCachePolicy is the effective, fully-defaulted cache policy for one
// mount prefix, after preset defaults and any global/layout overrides have
// been applied. It holds plain values (no pointers): daemon converts it,
// field by field, into vfs.CachePolicy when it builds each vfs.Mount, so
// that vfs never needs to import this package's option types.
//
// ReadaheadMax and ReadaheadRequest keep the "0 = not configured" meaning
// from CachePolicy all the way through resolution — see the comment on
// CachePolicy. Every other field always resolves to a concrete value (there
// is no competing vfs.Options fallback for them to accidentally shadow).
type ResolvedCachePolicy struct {
	SmallFileWhole bool
	// SmallFileThreshold bounds the sibling prefetch; SmallFileWholeThreshold
	// bounds the foreground whole-file fetch and keeps the "0 = fall back to
	// SmallFileThreshold" meaning from CachePolicy, which vfs resolves.
	SmallFileThreshold      Size
	SmallFileWholeThreshold Size
	DirReadahead            int
	ReadaheadMax            Size
	ReadaheadRequest        Size
	ReadaheadLead           time.Duration
}

// knownCachePresets are the only legal values of CachePolicy.Preset.
var knownCachePresets = map[string]bool{
	"": true, "none": true, "media": true, "photos": true, "code": true,
}

// presetCachePolicy returns the built-in defaults for a preset name. "" and
// "none" (and any other value already rejected by validation) fall through
// to the plain defaults, which deliberately leave ReadaheadMax and
// ReadaheadRequest at 0 ("not configured") rather than baking in the 64MiB /
// derive-per-mount behaviour those fallbacks document — that documented
// behaviour lives in vfs's own Options defaults, not here, so it stays a
// single source of truth and CLOUDFS_READAHEAD_BLOCKS keeps working when no
// cache.policy is set at all.
func presetCachePolicy(preset string) ResolvedCachePolicy {
	r := ResolvedCachePolicy{
		SmallFileThreshold: 4 << 20,
		DirReadahead:       32,
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
		// Source trees are read by tools that jump: git walks its index,
		// a build follows imports, an agent opens the three files a symbol
		// appears in. None of that arms the sibling prefetch, which needs
		// three reads in listing order, so without this every such file is
		// fetched a block at a time and its tail costs a second request.
		r.SmallFileWhole = true
		// Above the 4MiB default block size, or the foreground fetch never
		// fires: below it the ordinary path already brings the file down in
		// one request. The megabyte above stays where it is — it is what the
		// speculative sibling prefetch is willing to spend on a guess, and
		// that is a different question from what a read the user is waiting
		// on may take whole.
		r.SmallFileWholeThreshold = 8 << 20
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
	if p.SmallFileWholeThreshold != 0 {
		r.SmallFileWholeThreshold = p.SmallFileWholeThreshold
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
	if r.SmallFileWholeThreshold != 0 && r.SmallFileWholeThreshold%(64<<10) != 0 {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache small_file_whole_threshold must be a multiple of 64KiB, got %s", r.SmallFileWholeThreshold)
	}
	// A whole-file fetch bounded at or below the block size can never fire:
	// vfs leaves a file that fits in one block to the ordinary path, which
	// already brings it down in a single request. Saying so here is better
	// than a knob that resolves cleanly and then does nothing.
	if r.SmallFileWhole && blockSize > 0 {
		whole := r.SmallFileWholeThreshold
		if whole == 0 {
			whole = r.SmallFileThreshold
		}
		if int64(whole) <= blockSize {
			return ResolvedCachePolicy{}, fmt.Errorf("config: cache small_file_whole needs a whole-file threshold above block_size (%d), got %s — set small_file_whole_threshold", blockSize, whole)
		}
	}
	if r.ReadaheadRequest != 0 && blockSize > 0 && int64(r.ReadaheadRequest)%blockSize != 0 {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache readahead_request must be a positive multiple of block_size (%d), got %s", blockSize, r.ReadaheadRequest)
	}
	// 0 means "not configured" (fall back to vfs's own Options.ReadAheadBlocks
	// default), so it is exempt from the >= block_size requirement the same
	// way 0 is exempt from the multiple-of-block_size requirement above.
	if r.ReadaheadMax != 0 && blockSize > 0 && int64(r.ReadaheadMax) < blockSize {
		return ResolvedCachePolicy{}, fmt.Errorf("config: cache readahead_max (%s) must be >= block_size (%d)", r.ReadaheadMax, blockSize)
	}
	return r, nil
}
