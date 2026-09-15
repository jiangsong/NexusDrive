package config

import (
	"testing"
	"time"
)

func TestSearchCrawlIsOffByDefault(t *testing.T) {
	cfg, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Search.Crawl
	if c.Enabled || len(c.Remotes) != 0 || len(c.Exclude) != 0 || c.IdleAfter != 30*time.Second || c.Rescan != 5*time.Minute {
		t.Fatalf("%+v", c)
	}
}

func TestSearchCrawlIsValidated(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown remote":  "search:\n  crawl:\n    enabled: true\n    remotes: [nope]\n",
		"bad glob":        "search:\n  crawl:\n    exclude: [\"[\"]\n",
		"empty glob":      "search:\n  crawl:\n    exclude: [\"\"]\n",
		"negative idle":   "search:\n  crawl:\n    idle_after: -1s\n",
		"negative rescan": "search:\n  crawl:\n    rescan: -1m\n",
	} {
		if _, err := Parse([]byte(example + "\n" + yaml)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	cfg, err := Parse([]byte(example + "\nsearch:\n  crawl:\n    enabled: true\n    remotes: [ali]\n    exclude: [\"*/node_modules\", \".git\"]\n    idle_after: 5s\n"))
	if err != nil || !cfg.Search.Crawl.Enabled || cfg.Search.Crawl.IdleAfter != 5*time.Second {
		t.Fatalf("%+v %v", cfg.Search, err)
	}
	if c := cfg.Search.Crawl; len(c.Remotes) != 1 || len(c.Exclude) != 2 || c.Rescan != 5*time.Minute {
		t.Fatalf("explicit block not kept, or rescan default not filled: %+v", c)
	}
}
