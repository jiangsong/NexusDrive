package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
	"cloudfs/internal/provider/webdav"
)

type streamDirectoryProvider struct {
	provider.Provider
	stream   func(context.Context, string, func(provider.Entry) error) error
	disabled bool
}

func (p *streamDirectoryProvider) Capabilities() provider.Caps {
	caps := p.Provider.Capabilities()
	caps.StreamList = !p.disabled
	return caps
}

func (p *streamDirectoryProvider) ListStream(ctx context.Context, id string, visit func(provider.Entry) error) error {
	return p.stream(ctx, id, visit)
}

func TestStreamingProviderCollectionUsesAtomicTEMPAndNeverReplaysPrefix(t *testing.T) {
	for _, mode := range []string{"complete", "partial-error", "late-unsupported", "early-unsupported", "disabled", "ignored-visitor-error", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.fake.Seed("original", []byte("old"))
			if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
				t.Fatal(err)
			}
			root, err := e.store.Resolve(ctx, "/ali")
			if err != nil {
				t.Fatal(err)
			}
			before, err := e.store.DirState(ctx, root.Ino)
			if err != nil {
				t.Fatal(err)
			}
			e.clk.advance(2 * time.Minute)
			stats := provider.NewStats()
			streams, partialChecked := 0, false
			p := &streamDirectoryProvider{Provider: provider.Instrument(e.fake, stats), disabled: mode == "disabled"}
			p.stream = func(ctx context.Context, id string, visit func(provider.Entry) error) error {
				streams++
				if mode == "early-unsupported" {
					return provider.ErrUnsupported
				}
				for i := range 1200 {
					name := fmt.Sprintf("new-%04d", i)
					if mode == "ignored-visitor-error" {
						name = "duplicate"
					}
					err := visit(provider.Entry{ID: name, Name: name, Kind: provider.KindFile, Size: 1, Version: "v1"})
					if err != nil && mode != "ignored-visitor-error" {
						return err
					}
					if i == 250 {
						partialChecked = true
						if _, err := e.store.Lookup(ctx, root.Ino, "original"); err != nil {
							t.Fatal("prefix deleted the old visible collection")
						}
						if _, err := e.store.Lookup(ctx, root.Ino, "new-0000"); !errors.Is(err, meta.ErrNotFound) {
							t.Fatal("prefix published before stream completion")
						}
						if state, err := e.store.DirState(ctx, root.Ino); err != nil || state != before {
							t.Fatal("prefix advanced directory freshness")
						}
						switch mode {
						case "partial-error":
							return provider.ErrTransient
						case "late-unsupported":
							return provider.ErrUnsupported
						case "cancelled":
							cancel()
							return nil
						}
					}
				}
				return nil
			}
			m := e.mount()
			m.Provider = p
			err = e.fs.fetchDir(ctx, m, root.Ino, root, true)
			fallback := mode == "early-unsupported" || mode == "disabled"
			if mode == "complete" || fallback {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("incomplete stream published successfully")
			}
			wantStreams := 1
			if mode == "disabled" {
				wantStreams = 0
			}
			if streams != wantStreams || (stats.Snapshot()["list"] != 0) != fallback {
				t.Fatalf("capability/replay contract: streams=%d calls=%v", streams, stats.Snapshot())
			}
			if !fallback && !partialChecked {
				t.Fatal("fixture did not reach a stored TEMP batch")
			}
			check := context.Background()
			if mode == "complete" {
				if count, err := e.store.ChildrenCount(check, root.Ino); err != nil || count != 1200 {
					t.Fatalf("complete stream: count=%d err=%v", count, err)
				}
			} else {
				if count, err := e.store.ChildrenCount(check, root.Ino); err != nil || count != 1 {
					t.Fatalf("incomplete stream replaced old directory: count=%d err=%v", count, err)
				}
			}
		})
	}
}

func TestWebDAVStreamThroughInstrumentationAndVFS(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	var complete atomic.Bool
	complete.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<d:multistatus xmlns:d="DAV:">`)
		for i := range 500 {
			fmt.Fprintf(w, `<d:response><d:href>/dav/f-%04d</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:getcontentlength>1</d:getcontentlength><d:getetag>v1</d:getetag></d:prop></d:propstat></d:response>`, i)
		}
		if complete.Load() {
			io.WriteString(w, `</d:multistatus>`)
		}
	}))
	defer srv.Close()
	p, err := webdav.New(webdav.Options{Name: "ali", BaseURL: srv.URL + "/dav", Client: httpx.New(httpx.Options{HTTP: srv.Client()})})
	if err != nil {
		t.Fatal(err)
	}
	stats := provider.NewStats()
	e.fs.mounts[0].Provider = provider.Instrument(p, stats)
	e.fs.mounts[0].RootID = "/"
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	root.RemoteID = "/"
	if _, err := e.store.Upsert(ctx, root); err != nil {
		t.Fatal(err)
	}
	page, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10, Count: true})
	if err != nil || len(page.Entries) != 10 || page.Total != 500 || stats.Snapshot()["list"] != 1 {
		t.Fatalf("streaming cold page: %+v calls=%v err=%v", page, stats.Snapshot(), err)
	}
	before, err := e.store.DirState(ctx, root.Ino)
	if err != nil {
		t.Fatal(err)
	}
	// An incomplete tail after multiple TEMP batches is not a new snapshot.
	complete.Store(false)
	e.clk.advance(2 * time.Minute)
	page, err = e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10, Count: true})
	if err != nil || page.Total != 500 || stats.Snapshot()["list"] != 2 {
		t.Fatalf("stale page fallback: %+v calls=%v err=%v", page, stats.Snapshot(), err)
	}
	if state, err := e.store.DirState(ctx, root.Ino); err != nil || state != before {
		t.Fatalf("truncated XML advanced freshness: %+v %v", state, err)
	}
}
