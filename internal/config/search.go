package config

import (
	"errors"
	"fmt"
	"path"
	"time"
)

// Search holds the name-search settings. The index itself is always on: it
// is the metadata cache, and every listing feeds it. What this block controls
// is the background crawler that lists directories nobody has opened yet, so
// the index covers the whole volume instead of the corners somebody visited.
type Search struct {
	Crawl SearchCrawl `yaml:"crawl"`
}

// SearchCrawl configures the background directory crawler. It is off by
// default because a crawl is a provider call per directory, and an account
// on an unofficial API pays for those in risk-control terms; a person turns
// it on knowing that.
type SearchCrawl struct {
	// Enabled starts the crawler with the daemon. Off means zero provider
	// calls: `cloudfs warm --all` still runs one pass on demand.
	Enabled bool `yaml:"enabled"`
	// Remotes limits the crawl to these remote names; empty means every
	// mounted remote. Each name must exist under remotes.
	Remotes []string `yaml:"remotes"`
	// Exclude holds path.Match patterns that are tried against the virtual
	// path of a directory and against the directory's own name, so both
	// "/work/build" and "node_modules" work.
	Exclude []string `yaml:"exclude"`
	// IdleAfter is how long foreground IO has to be quiet before a pass
	// starts. Default 30s.
	IdleAfter time.Duration `yaml:"idle_after"`
	// Rescan is how often a finished pass looks again for directories a
	// delta event marked stale. Default 5m.
	Rescan time.Duration `yaml:"rescan"`
}

const (
	defaultCrawlIdleAfter = 30 * time.Second
	defaultCrawlRescan    = 5 * time.Minute
)

// Validate fills the crawl defaults and rejects a block the crawler could not
// honour: a remote that is not configured, a pattern path.Match cannot parse,
// or a negative duration. Zero durations take the defaults so a config that
// only says `enabled: true` works.
func (s *Search) Validate(remotes map[string]Remote) error {
	c := &s.Crawl
	if c.IdleAfter == 0 {
		c.IdleAfter = defaultCrawlIdleAfter
	}
	if c.Rescan == 0 {
		c.Rescan = defaultCrawlRescan
	}
	if c.IdleAfter < 0 {
		return fmt.Errorf("config: search.crawl.idle_after must not be negative, got %s", c.IdleAfter)
	}
	if c.Rescan < 0 {
		return fmt.Errorf("config: search.crawl.rescan must not be negative, got %s", c.Rescan)
	}
	for _, name := range c.Remotes {
		if _, ok := remotes[name]; !ok {
			return fmt.Errorf("config: search.crawl.remotes names unknown remote %q", name)
		}
	}
	for _, pat := range c.Exclude {
		if pat == "" {
			return errors.New("config: search.crawl.exclude holds an empty pattern")
		}
		if _, err := path.Match(pat, ""); errors.Is(err, path.ErrBadPattern) {
			return fmt.Errorf("config: search.crawl.exclude pattern %q: %w", pat, err)
		}
	}
	return nil
}
