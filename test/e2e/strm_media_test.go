package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/daemon"
	"cloudfs/internal/provider"
	"cloudfs/internal/strmgen"
	"cloudfs/internal/webdavsrv"
	"cloudfs/test/fakeprovider"
)

// mediaLibrary starts a daemon over the fake provider holding films laid out
// the way a scraper expects — one directory per title — and a read-only WebDAV
// endpoint over it. No FUSE mount: a media server reaching CloudFS this way is
// the case that does not need one.
func mediaLibrary(t *testing.T, films int, dirTTL time.Duration, background bool) (*daemon.Daemon, *fakeprovider.Fake, *webdavsrv.Running, map[string]string) {
	t.Helper()
	base := t.TempDir()
	cfg := config.Default()
	cfg.SourcePath = filepath.Join(base, "config.yaml")
	cfg.Cache.Dir = filepath.Join(base, "cache")
	cfg.Cache.MaxSize = 64 << 20
	cfg.Cache.MinFree = 0
	cfg.Cache.BlockSize = 64 << 10
	cfg.Cache.SubBlockSize = 16 << 10
	cfg.Control.Socket = ""
	cfg.Remotes = map[string]config.Remote{"demo": {Type: "fake"}}
	cfg.Mounts = []config.Mount{{Path: filepath.Join(base, "unused-mount"), Layout: map[string]config.Layout{
		// A media library is the case dir_ttl exists for: the tree barely
		// changes and a scraper walks all of it, repeatedly.
		"/": {Remote: "demo", Root: fakeprovider.RootID, Mode: config.ModeReadonly, DirTTL: dirTTL},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d, err := daemon.Open(ctx, daemon.Options{Config: &cfg, Version: "strm-e2e", NoBackground: !background})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	fake, _ := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	if fake == nil {
		t.Fatal("fake provider was not assembled")
	}
	titleDirs := make(map[string]string, films)
	for i := 0; i < films; i++ {
		title := fmt.Sprintf("Film %04d (20%02d)", i, i%20+10)
		entry := fake.Seed("Films/"+title+"/"+title+".mkv", []byte("movie bytes"))
		titleDirs[title] = entry.ParentID
		// A scraper also sees the sidecars; they must not become .strm files.
		fake.Seed("Films/"+title+"/poster.jpg", []byte("art"))
	}
	running, err := webdavsrv.Start(ctx, webdavsrv.Options{
		FS: d.FS, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/Films", Token: "strm-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { running.Close() })
	return d, fake, running, titleDirs
}

// TestSTRMGenerationOverAThousandFilmsHasABoundedBackendCost is the T-20
// acceptance: generating a media library's .strm files must cost a bounded
// number of provider calls, and repeating the scan — which is what a media
// server does on a schedule — must cost none at all.
//
// The bound that matters is one listing per directory and no downloads: a
// .strm file is a pointer, and pulling a movie's bytes to write one would turn
// a library scan into a full mirror.
func TestSTRMGenerationOverAThousandFilmsHasABoundedBackendCost(t *testing.T) {
	if testing.Short() {
		t.Skip("1000-film library scan")
	}
	const films = 1000
	// The cost of the scan is measured without the daemon's background work:
	// the delta poller and the prefetcher make provider calls of their own, and
	// counting those as the walk's cost measures the wrong thing.
	d, fake, running, _ := mediaLibrary(t, films, 24*time.Hour, false)
	out := t.TempDir()
	client := &http.Client{Timeout: 60 * time.Second}
	generate := func(prune bool) strmgen.Stats {
		t.Helper()
		stats, err := strmgen.Generate(context.Background(), strmgen.Options{
			Client: client, StartURL: "http://" + running.Addr() + "/dav",
			OutputDir: out, Token: "strm-token", Prune: prune,
		})
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}

	before := fake.TotalCalls()
	stats := generate(false)
	cold := fake.TotalCalls() - before
	if stats.Media != films || stats.Written != films {
		ctx := context.Background()
		titles, derr := d.FS.ReadDirPath(ctx, "/Films")
		short := []string{}
		for _, title := range titles {
			kids, err := d.FS.ReadDirPath(ctx, "/Films/"+title.Name)
			if err != nil || len(kids) != 2 {
				short = append(short, fmt.Sprintf("%s:%d/%v", title.Name, len(kids), err))
			}
		}
		missing := []string{}
		for i := 0; i < films; i++ {
			title := fmt.Sprintf("Film %04d (20%02d)", i, i%20+10)
			if _, err := os.Stat(filepath.Join(out, title, title+".strm")); err != nil {
				missing = append(missing, title)
			}
		}
		t.Fatalf("generated %d media entries and wrote %d, want %d; stats=%+v; VFS lists %d titles (%v);"+
			" short dirs: %v; .strm missing: %v",
			stats.Media, stats.Written, films, stats, len(titles), derr, short, missing)
	}

	// The bound is one enumeration of the tree and nothing more: each directory
	// listed once, paged at whatever the backend pages at. Anything above it
	// means the walk is re-listing directories it has already seen, which is
	// what turns a library scan into an O(n^2) traffic problem on a real
	// account.
	pages := func(children int) int { return (children + fakeprovider.ListPageSize - 1) / fakeprovider.ListPageSize }
	// The mount root holds "Films"; "Films" holds one directory per title;
	// each title directory holds the film and its sidecar.
	enumeration := pages(1) + pages(films) + films*pages(2)
	if cold > enumeration {
		t.Fatalf("generating .strm files made %d provider calls where one enumeration of the tree is %d "+
			"(List=%d Stat=%d ReadRange=%d DownloadURL=%d)",
			cold, enumeration, fake.Calls("List"), fake.Calls("Stat"), fake.Calls("ReadRange"), fake.Calls("DownloadURL"))
	}
	t.Logf("cold scan of %d films: %d provider calls; one full enumeration is %d", films, cold, enumeration)

	// Every call must be a directory listing. A .strm file is a pointer; opening
	// the media to write one would turn a library scan into a full mirror.
	if fake.Calls("ReadRange") != 0 || fake.Calls("DownloadURL") != 0 {
		t.Fatalf("the scan touched file contents: ReadRange=%d DownloadURL=%d",
			fake.Calls("ReadRange"), fake.Calls("DownloadURL"))
	}

	// A scraper re-scans on a schedule. With dir_ttl at 24h the second walk is
	// answered entirely from local metadata.
	before = fake.TotalCalls()
	stats = generate(false)
	if warm := fake.TotalCalls() - before; warm != 0 {
		t.Fatalf("re-scanning a library with dir_ttl 24h made %d provider calls, want 0", warm)
	}
	if stats.Written != 0 || stats.Unchanged != films {
		t.Fatalf("re-scan wrote %d and left %d unchanged, want 0 and %d", stats.Written, stats.Unchanged, films)
	}

	// Spot-check the output: one .strm per film, pointing at the DAV URL, and
	// no .strm for the sidecar art.
	title := "Film 0007 (2017)"
	content, err := os.ReadFile(filepath.Join(out, title, title+".strm"))
	if err != nil {
		t.Fatal(err)
	}
	segment := url.PathEscape(title)
	want := "http://" + running.Addr() + "/dav/" + segment + "/" + url.PathEscape(title+".mkv") + "\n"
	if string(content) != want {
		t.Fatalf(".strm content = %q, want %q", content, want)
	}
	if _, err := os.Stat(filepath.Join(out, title, "poster.strm")); !os.IsNotExist(err) {
		t.Fatalf("a non-media sidecar produced a .strm file: %v", err)
	}
}

// TestSTRMPruneRemovesOnlyWhatItGenerated: a title removed upstream should stop
// appearing in the library, but a scraper's own files living beside the .strm
// output must survive. Deleting those would be the kind of damage that is only
// noticed later.
func TestSTRMPruneRemovesOnlyWhatItGenerated(t *testing.T) {
	d, fake, running, titleDirs := mediaLibrary(t, 3, 24*time.Hour, false)
	out := t.TempDir()
	client := &http.Client{Timeout: 30 * time.Second}
	generate := func(prune bool) strmgen.Stats {
		t.Helper()
		stats, err := strmgen.Generate(context.Background(), strmgen.Options{
			Client: client, StartURL: "http://" + running.Addr() + "/dav",
			OutputDir: out, Token: "strm-token", Prune: prune,
		})
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}
	// The first prune run only records a baseline; it must not delete.
	generate(true)

	// Something the scraper wrote, which the generator did not.
	scraped := filepath.Join(out, "Film 0000 (2010)", "movie.nfo")
	if err := os.WriteFile(scraped, []byte("<movie/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The title disappears upstream. With dir_ttl at 24h — the setting a media
	// library wants — the tree is kept current by the change feed, not by
	// re-listing, so the refresh is made explicit here rather than waiting for
	// the background poll.
	ctx := context.Background()
	if err := fake.Delete(ctx, titleDirs["Film 0001 (2011)"]); err != nil {
		t.Fatal(err)
	}
	films, err := d.FS.StatPath(ctx, "/Films")
	if err != nil {
		t.Fatal(err)
	}
	// The refresh fence protects a child whose own listing is not older than
	// the start of this one, and those timestamps have second resolution. A
	// title listed by the walk moments ago is therefore kept until the next
	// second — the fence errs towards keeping data, which is the right way for
	// it to err, but it has to be waited out here to observe the removal.
	time.Sleep(1200 * time.Millisecond)
	if err := d.FS.Refresh(ctx, films.Ino); err != nil {
		t.Fatal(err)
	}
	if entries, err := d.FS.ReadDirPath(ctx, "/Films"); err != nil || len(entries) != 2 {
		t.Fatalf("after the removal the library lists %d titles (%v), want 2", len(entries), err)
	}
	stats := generate(true)
	if stats.Pruned != 1 {
		t.Fatalf("pruned %d entries (dirs=%d media=%d retained=%d), want the one whose source is gone",
			stats.Pruned, stats.Directories, stats.Media, stats.Retained)
	}
	if _, err := os.Stat(filepath.Join(out, "Film 0001 (2011)", "Film 0001 (2011).strm")); !os.IsNotExist(err) {
		t.Fatalf("the .strm for a removed title survived: %v", err)
	}
	if _, err := os.Stat(scraped); err != nil {
		t.Fatalf("prune deleted a file it did not generate: %v", err)
	}
}
