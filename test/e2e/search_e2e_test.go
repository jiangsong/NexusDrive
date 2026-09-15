package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/control"
)

// crawlYAML turns the background crawler on with the short quiet period a
// test can wait for. Everything's first rule is that the index is the whole
// volume; the crawler is what makes that true for a drive nobody browsed.
const crawlYAML = "search:\n  crawl:\n    enabled: true\n    idle_after: 200ms\n    rescan: 1s\n"

// TestNeverOpenedDirectoryBecomesSearchable needs a kernel mount: run on
// Linux with /dev/fuse. A file seeded into a directory nobody ever opened
// is not in the index; with the crawler on, the main window's search finds
// it within ten seconds, the mount reads it, and /status reports the pass.
func TestNeverOpenedDirectoryBecomesSearchable(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{extraYAML: crawlYAML})
	s.fake.Seed("archive/2019/never-opened-51c9.txt", []byte("x"))
	h := control.NewServer(s.d.Collector()).Handler()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w := uiCall(t, h, "GET", "/search?q=51c9", "")
		if strings.Contains(w.Body.String(), `"/archive/2019/never-opened-51c9.txt"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not searchable within 10s: %s", w.Body.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if b, err := os.ReadFile(filepath.Join(s.dir, "archive/2019/never-opened-51c9.txt")); err != nil || string(b) != "x" {
		t.Fatalf("the crawled file is not readable through the mount: %v", err)
	}
	var st struct {
		Crawl struct {
			Listed int64 `json:"listed"`
		} `json:"crawl"`
	}
	if err := json.Unmarshal(uiCall(t, h, "GET", "/status", "").Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Crawl.Listed < 3 {
		t.Fatalf("status.crawl does not reflect the pass: %+v", st)
	}
}

// TestNameSearchInTheBrowser renders the main window's search and the
// cache screen in headless Chromium: the result row carries the file's
// size (the column the old page left empty), the hit is marked, the
// coverage line is there, and the cache screen has its coverage card.
// The browser half runs only with CLOUDFS_BROWSER=1.
func TestNameSearchInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{})
	ctx := context.Background()
	s.fake.Seed("notes/plan.md", []byte("browser smoke marker 7f3a")) // 25 bytes, never read: cached=false
	if _, err := s.d.FS.ReadDirPath(ctx, "/notes"); err != nil {
		t.Fatal(err)
	}
	if err := s.d.FS.Meta().FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	h := control.NewServer(s.d.Collector()).Handler()
	if w := uiCall(t, h, "GET", "/search?q=plan", ""); !strings.Contains(w.Body.String(), `"/notes/plan.md"`) || !strings.Contains(w.Body.String(), `"size":25`) {
		t.Fatalf("/search: %s", w.Body.String())
	}
	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	wants := []string{`data-hit="/notes/plan.md"`, `data-size="25"`, "25 B", "<mark>plan</mark>", "known folders"}
	dom := renderedDOM(t, chrome, base+"/?lang=en#/connections?q=plan", wants...)
	for _, want := range wants {
		if !strings.Contains(dom, want) {
			t.Fatalf("the file browser's name search lacks %s:\n%s", want, dom)
		}
	}
	if dom := renderedDOM(t, chrome, base+"/?lang=en#/storage", "Folder coverage"); !strings.Contains(dom, "Folder coverage") {
		t.Fatalf("the cache screen has no coverage card:\n%s", dom)
	}
}
