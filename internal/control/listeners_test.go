package control

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/i18n"
)

func socketPath(t *testing.T) string {
	t.Helper()
	// macOS Unix socket names must fit in 104 bytes; t.TempDir can exceed it.
	dir, err := os.MkdirTemp("", "cfs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}

func TestControlSocketLifecycle(t *testing.T) {
	f := newFixture(t)
	p := socketPath(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	r, err := NewServer(f.coll).Start(context.Background(), p, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("socket mode: %v, %v", st, err)
	}
	got, online, err := FetchStatus(context.Background(), p, "")
	if err != nil || !online || got.Version != "test" {
		t.Fatalf("status: %+v %v %v", got, online, err)
	}
	if other, err := NewServer(f.coll).Start(context.Background(), p, ""); err == nil {
		other.Close()
		t.Fatal("second server replaced live socket")
	}
	// Failed duplicate startup must not disrupt the first server.
	if _, online, err := FetchStatus(context.Background(), p, ""); err != nil || !online {
		t.Fatalf("original socket lost: %v", err)
	}
	r.Close()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("socket not cleaned up: %v", err)
	}
	// The persistent lock file is reusable after normal or crash shutdown.
	r2, err := NewServer(f.coll).Start(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	r2.Close()
}

func TestControlSocketRefusesFilesAndPublicTCP(t *testing.T) {
	f := newFixture(t)
	p := socketPath(t)
	if err := os.WriteFile(p, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := NewServer(f.coll).Start(context.Background(), p, ""); err == nil {
		r.Close()
		t.Fatal("replaced regular file")
	}
	if b, _ := os.ReadFile(p); string(b) != "keep me" {
		t.Fatal("file changed")
	}
	if r, err := NewServer(f.coll).Start(context.Background(), "", "0.0.0.0:9101"); err == nil {
		r.Close()
		t.Fatal("accepted public control endpoint")
	}
}

func TestControlStatusFallsBackToTCP(t *testing.T) {
	f := newFixture(t)
	r, err := NewServer(f.coll).Start(context.Background(), "", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, online, err := FetchStatus(context.Background(), socketPath(t), r.listeners[0].Addr().String())
	if err != nil || !online || got.Version != "test" {
		t.Fatalf("fallback status: %+v %v %v", got, online, err)
	}
}

func TestFetchStatusCarriesTheRequestedLanguage(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("lang"); got != "en" {
			t.Errorf("status language = %q, want en", got)
		}
		_ = json.NewEncoder(w).Encode(Status{Version: "localized"})
	})}
	go srv.Serve(l)
	t.Cleanup(func() {
		_ = srv.Close()
		_ = l.Close()
	})

	got, online, err := FetchStatusInLanguage(context.Background(), "", l.Addr().String(), i18n.EN)
	if err != nil || !online || got.Version != "localized" {
		t.Fatalf("localized status: %+v online=%v err=%v", got, online, err)
	}
}
