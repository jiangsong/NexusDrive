package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/control"
)

// uiCall sends what the embedded page sends: same origin, the control header
// on mutations, one JSON object.
func uiCall(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://cloudfs"+target, nil)
	} else {
		r = httptest.NewRequest(method, "http://cloudfs"+target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		r.Header.Set("X-CloudFS-Control", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestUIAPIEndToEnd is the acceptance for the browser's file operations: what
// the page does through the control API is what the kernel mount shows, and
// what the shell does at the mount point is what the page lists. Both are
// adapters over one VFS; this proves neither has a private view.
func TestUIAPIEndToEnd(t *testing.T) {
	s := newStack(t, "writeback")
	s.fake.Seed("Films/a.mkv", []byte("movie"))
	h := control.NewServer(s.d.Collector()).Handler()
	ctx := context.Background()

	// The page creates a directory; the shell sees it.
	if w := uiCall(t, h, "POST", "/fs/mkdir", `{"path":"/Films/2024"}`); w.Code != 200 {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body.String())
	}
	if st, err := os.Stat(filepath.Join(s.dir, "Films", "2024")); err != nil || !st.IsDir() {
		t.Fatalf("directory made through the API is not at the mount point: %v", err)
	}

	// The shell writes a file; the page lists it, with its local state.
	if err := os.WriteFile(filepath.Join(s.dir, "Films", "2024", "notes.txt"), []byte("from the shell"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.settle(t)
	w := uiCall(t, h, "GET", "/fs/list?path=/Films/2024", "")
	if w.Code != 200 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list control.FSListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Name != "notes.txt" || list.Entries[0].Size != 14 {
		t.Fatalf("the page does not see the shell's file: %+v", list)
	}

	// The page renames; the shell reads the new name and not the old.
	if w := uiCall(t, h, "POST", "/fs/rename", `{"from":"/Films/2024/notes.txt","to":"/Films/2024/renamed.txt"}`); w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if b, err := os.ReadFile(filepath.Join(s.dir, "Films", "2024", "renamed.txt")); err != nil || string(b) != "from the shell" {
		t.Fatalf("renamed file at the mount point: %q %v", b, err)
	}
	oldGone := false
	for i := 0; i < 50 && !oldGone; i++ {
		_, err := os.Stat(filepath.Join(s.dir, "Films", "2024", "notes.txt"))
		oldGone = os.IsNotExist(err)
		if !oldGone {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !oldGone {
		t.Fatal("old name still present 500ms after rename; the kernel dentry was not dropped")
	}

	// The page's preview is the file's bytes.
	if w := uiCall(t, h, "GET", "/fs/preview?path=/Films/2024/renamed.txt&length=4", ""); w.Code != 200 || w.Body.String() != "from" {
		t.Fatalf("preview: %d %q", w.Code, w.Body.String())
	}

	// Search knows the name the moment it was listed.
	if w := uiCall(t, h, "GET", "/search?q=renamed", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "/Films/2024/renamed.txt") {
		t.Fatalf("search: %d %s", w.Code, w.Body.String())
	}

	// The page deletes, with confirmation; the shell finds nothing.
	if w := uiCall(t, h, "POST", "/fs/delete", `{"path":"/Films/2024","recursive":true,"confirm":true}`); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	// The kernel is told to drop the name from another goroutine, so give it
	// a moment; well under the one-second entry timeout that would otherwise
	// keep the directory answering.
	gone := false
	for i := 0; i < 50 && !gone; i++ {
		_, err := os.Stat(filepath.Join(s.dir, "Films", "2024"))
		gone = os.IsNotExist(err)
		if !gone {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !gone {
		t.Fatal("deleted directory still at the mount point 500ms later; the kernel dentry was not dropped")
	}
	if _, err := s.d.FS.StatPath(ctx, "/Films/2024"); err == nil {
		t.Fatal("deleted directory still resolves in the VFS")
	}

	// Doctor on the live daemon, as the diagnostics page would call it.
	col := s.d.Collector()
	col.Doctor = s.d.Doctor(nil)
	if w := uiCall(t, control.NewServer(col).Handler(), "POST", "/doctor/run", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"upload_queue"`) {
		t.Fatalf("doctor: %d %s", w.Code, w.Body.String())
	}
}
