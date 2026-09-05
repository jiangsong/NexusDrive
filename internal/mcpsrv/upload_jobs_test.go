package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func seedManagedUpload(t *testing.T, e *env, p, content string) (string, uint64) {
	t.Helper()
	ctx := context.Background()
	dir := path.Dir(p)
	if dir != "/" {
		e.fake.Seed(strings.TrimPrefix(path.Join(dir, ".seed"), "/"), []byte("seed"))
		if _, err := e.fs.StatPath(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	a, err := e.fs.WriteFile(ctx, p, []byte(content), false)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.ByIno(ctx, a.Ino)
	if err != nil || len(rows) != 1 {
		t.Fatalf("upload for %s: %+v %v", p, rows, err)
	}
	return rows[0].ID, a.Ino
}

func TestUploadToolsManageCurrentVersionWithoutLeakingPrivateData(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	id, _ := seedManagedUpload(t, e, "/work/a.txt", "retained content")
	if err := e.j.Fail(context.Background(), id, errors.New("token=SECRET /private/journal/blob backend-object")); err != nil {
		t.Fatal(err)
	}
	before := e.fake.TotalCalls()
	var page uploadsOutput
	if res := e.call(t, "list_uploads", uploadsInput{}, &page); res.IsError || len(page.Uploads) != 1 || page.Uploads[0].ID != id || page.Uploads[0].Path != "/work/a.txt" || page.Uploads[0].State != journal.StateDead {
		t.Fatalf("list: %+v %s", page, errText(res))
	}
	var info vfs.UploadInfo
	res := e.call(t, "get_upload", uploadInput{ID: id}, &info)
	if res.IsError || info.ID != id || info.Path != "/work/a.txt" {
		t.Fatalf("get: %+v %s", info, errText(res))
	}
	encoded, _ := json.Marshal([]any{page, info, res})
	for _, secret := range []string{"SECRET", "/private/journal", "backend-object", "token="} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("upload inspection leaked %q", secret)
		}
	}
	var mutation uploadMutationOutput
	if res := e.call(t, "retry_upload", uploadInput{ID: id}, &mutation); res.IsError || !mutation.Accepted || mutation.State != journal.StatePending {
		t.Fatalf("retry: %+v %s", mutation, errText(res))
	}
	if res := e.call(t, "cancel_upload", uploadInput{ID: id}, &mutation); res.IsError || mutation.State != journal.StateCancelled || !strings.Contains(mutation.Warning, "does not undo") {
		t.Fatalf("cancel: %+v %s", mutation, errText(res))
	}
	if res := e.call(t, "resume_upload", confirmedUploadInput{ID: id}, nil); !res.IsError {
		t.Fatal("resume did not require confirmation")
	}
	if res := e.call(t, "resume_upload", confirmedUploadInput{ID: id, Confirm: true}, &mutation); res.IsError || mutation.State != journal.StatePending || !strings.Contains(mutation.Warning, "duplicated") {
		t.Fatalf("resume: %+v %s", mutation, errText(res))
	}
	if res := e.call(t, "cancel_upload", uploadInput{ID: id}, &mutation); res.IsError || mutation.State != journal.StateCancelled {
		t.Fatalf("second cancel: %+v %s", mutation, errText(res))
	}
	if res := e.call(t, "discard_upload", confirmedUploadInput{ID: id, Confirm: true}, nil); !res.IsError || !strings.Contains(errText(res), "unrestricted") {
		t.Fatalf("scoped discard was not refused: %s", errText(res))
	}
	if row, err := e.j.Get(context.Background(), id); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("refused discard changed upload: %+v %v", row, err)
	}
	if e.fake.TotalCalls() != before {
		t.Fatalf("local management contacted provider: before=%d after=%d", before, e.fake.TotalCalls())
	}
}

func TestUploadToolsFilterPathsAndAuthenticateCursors(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}, Limits: Limits{MaxEntries: 1}})
	// Seed both directories before the first root listing makes its TTL fresh.
	e.fake.Seed("work/.seed", []byte("seed"))
	e.fake.Seed("secret/.seed", []byte("seed"))
	if _, err := e.fs.StatPath(context.Background(), "/work"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(context.Background(), "/secret"); err != nil {
		t.Fatal(err)
	}
	visible, _ := seedManagedUpload(t, e, "/work/a", "a")
	hidden, _ := seedManagedUpload(t, e, "/secret/b", "b")
	second, _ := seedManagedUpload(t, e, "/work/c", "c")
	want := map[string]bool{visible: true, second: true}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 4 {
			t.Fatal("upload cursor did not make progress")
		}
		var out uploadsOutput
		if res := e.call(t, "list_uploads", uploadsInput{Cursor: cursor}, &out); res.IsError {
			t.Fatal(errText(res))
		}
		for _, info := range out.Uploads {
			if !want[info.ID] || info.ID == hidden || !strings.HasPrefix(info.Path, "/work/") {
				t.Fatalf("visible page leaked or duplicated an upload: %+v", out)
			}
			delete(want, info.ID)
		}
		if !out.Truncated {
			break
		}
		if out.NextCursor == "" || strings.Contains(out.NextCursor, visible) || strings.Contains(out.NextCursor, hidden) {
			t.Fatalf("cursor is empty or exposes an ID: %q", out.NextCursor)
		}
		cursor = out.NextCursor
	}
	if len(want) != 0 {
		t.Fatalf("visible uploads missing: %v", want)
	}
	for _, tool := range []string{"get_upload", "retry_upload", "cancel_upload", "resume_upload"} {
		var in any = uploadInput{ID: hidden}
		if tool == "resume_upload" {
			in = confirmedUploadInput{ID: hidden, Confirm: true}
		}
		if res := e.call(t, tool, in, nil); !res.IsError || errText(res) != "upload not found or inaccessible" {
			t.Fatalf("%s leaked hidden upload: %s", tool, errText(res))
		}
	}
	if res := e.call(t, "list_uploads", uploadsInput{Cursor: cursor + "XX"}, nil); !res.IsError {
		t.Fatal("tampered upload cursor was accepted")
	}
	if res := e.call(t, "flush_uploads", struct{}{}, nil); !res.IsError || !strings.Contains(errText(res), "unrestricted") {
		t.Fatalf("scoped flush accepted: %s", errText(res))
	}
}

func TestUploadDiscardAndFlushRequireWritableUnrestrictedServer(t *testing.T) {
	e := newEnv(t, Options{})
	id, ino := seedManagedUpload(t, e, "/discard-me", "local only")
	var mutation uploadMutationOutput
	if res := e.call(t, "cancel_upload", uploadInput{ID: id}, &mutation); res.IsError || mutation.State != journal.StateCancelled {
		t.Fatalf("cancel: %+v %s", mutation, errText(res))
	}
	if res := e.call(t, "discard_upload", confirmedUploadInput{ID: id}, nil); !res.IsError {
		t.Fatal("discard did not require confirmation")
	}
	before := e.fake.TotalCalls()
	if res := e.call(t, "discard_upload", confirmedUploadInput{ID: id, Confirm: true}, &mutation); res.IsError || !mutation.Accepted || !strings.Contains(mutation.Warning, "not undone") {
		t.Fatalf("discard: %+v %s", mutation, errText(res))
	}
	if _, err := e.j.Get(context.Background(), id); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("discarded row remains: %v", err)
	}
	if _, err := e.fs.Meta().Get(context.Background(), ino); err == nil {
		t.Fatal("discarded local version remains in metadata")
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("discard contacted provider")
	}

	flushID, _ := seedManagedUpload(t, e, "/flush-me", "upload this")
	var flushed uploadFlushOutput
	if res := e.call(t, "flush_uploads", struct{}{}, &flushed); res.IsError || !flushed.Completed {
		t.Fatalf("flush: %+v %s", flushed, errText(res))
	}
	if row, err := e.j.Get(context.Background(), flushID); err != nil || row.State != journal.StateDone {
		t.Fatalf("flush did not complete upload: %+v %v", row, err)
	}

	ro := newEnv(t, Options{ReadOnly: true})
	roID, _ := seedManagedUpload(t, ro, "/readonly", "keep")
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"retry_upload", uploadInput{ID: roID}},
		{"cancel_upload", uploadInput{ID: roID}},
		{"resume_upload", confirmedUploadInput{ID: roID, Confirm: true}},
		{"discard_upload", confirmedUploadInput{ID: roID, Confirm: true}},
		{"flush_uploads", struct{}{}},
	} {
		if res := ro.call(t, tc.name, tc.in, nil); !res.IsError || !strings.Contains(errText(res), "read-only") {
			t.Fatalf("read-only %s: %s", tc.name, errText(res))
		}
	}
}

func TestUploadToolsUseAuthenticatedHTTPTransport(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	id, _ := seedManagedUpload(t, e, "/work/http", "retained")
	h := httptest.NewServer(requireBearer(newMCPHTTPHandler(e.server), "resource-test-token"))
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unauthorized := mcp.NewClient(&mcp.Implementation{Name: "unauthorized", Version: "1"}, nil)
	if cs, err := unauthorized.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil); err == nil {
		cs.Close()
		t.Fatal("unauthenticated upload-management client connected")
	}
	c := newSubscriptionClient(t, e.server, &mcp.StreamableClientTransport{
		Endpoint: h.URL, HTTPClient: &http.Client{Transport: resourceAuthTransport{base: h.Client().Transport}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	})
	var page uploadsOutput
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: "list_uploads", Arguments: uploadsInput{}})
	if err != nil || res.IsError {
		t.Fatalf("HTTP list_uploads: %+v %v", res, err)
	}
	data, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(data, &page); err != nil || len(page.Uploads) != 1 || page.Uploads[0].ID != id {
		t.Fatalf("HTTP upload page: %+v %v", page, err)
	}
	res, err = c.session.CallTool(ctx, &mcp.CallToolParams{Name: "cancel_upload", Arguments: uploadInput{ID: id}})
	if err != nil || res.IsError {
		t.Fatalf("HTTP cancel_upload: %+v %v", res, err)
	}
	if row, err := e.j.Get(ctx, id); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("HTTP did not reach VFS upload management: %+v %v", row, err)
	}
}

func TestUploadHiddenPageRemainsBoundedAndDoesNotExposeIDs(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("secret/.seed", []byte("seed"))
	if _, err := e.fs.StatPath(context.Background(), "/secret"); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for i := 0; i < 201; i++ {
		a, err := e.fs.WriteFile(context.Background(), fmt.Sprintf("/secret/hidden-%03d", i), []byte("x"), false)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := e.j.ByIno(context.Background(), a.Ino)
		if err != nil || len(rows) != 1 {
			t.Fatalf("hidden upload %d: %+v %v", i, rows, err)
		}
		ids[rows[0].ID] = true
	}
	before := e.fake.TotalCalls()
	var page uploadsOutput
	if res := e.call(t, "list_uploads", uploadsInput{}, &page); res.IsError || len(page.Uploads) != 0 || !page.Truncated || page.NextCursor == "" {
		t.Fatalf("hidden upload page was not bounded: %+v %s", page, errText(res))
	}
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil || ids[string(raw)] {
		t.Fatalf("upload cursor exposed a raw ID: %q %v", raw, err)
	}
	var last uploadsOutput
	if res := e.call(t, "list_uploads", uploadsInput{Cursor: page.NextCursor}, &last); res.IsError || last.Truncated || len(last.Uploads) != 0 {
		t.Fatalf("hidden upload continuation: %+v %s", last, errText(res))
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("hidden upload pagination contacted provider")
	}
}
