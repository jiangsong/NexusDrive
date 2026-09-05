package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"cloudfs/internal/journal"
)

func TestCopyManagementControlsLiveOwner(t *testing.T) {
	f, _ := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	ctx := context.Background()
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := f.meta.Resolve(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	mount := f.coll.FS.Mounts()[0]
	c, err := f.j.BeginCopy(ctx, journal.CopySpec{MetaIdentity: identity, TargetParentIno: parent.Ino,
		SourceMount: "/", SourceRootID: "root", SourceAccountBinding: mount.AccountBinding,
		TargetMount: "/", TargetRootID: "root", TargetAccountBinding: mount.AccountBinding,
		SourcePath: "/source", SourceRemote: "ali", SourceID: "source",
		TargetPath: "/dest", TargetRemote: "ali", TargetParentID: "root", Mode: "writeback"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	p := socketPath(t)
	srv, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	out, online, err := CallCopyMutation(ctx, p, "", CopyMutationRequest{Action: "cancel", ID: id})
	if err != nil || !online || len(out.Copies) != 1 || out.Copies[0].State != journal.CopyCancelled {
		t.Fatalf("cancel: %+v %v %v", out, online, err)
	}
	c.Close()
	out, online, err = CallCopyMutation(ctx, p, "", CopyMutationRequest{Action: "retry", ID: id})
	if err != nil || !online || len(out.Copies) != 1 || out.Copies[0].State != journal.CopyReady {
		t.Fatalf("retry: %+v %v %v", out, online, err)
	}
	if _, err := f.coll.FS.ResumeCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"cancel", "retry"} {
		if _, online, err := CallCopyMutation(ctx, p, "", CopyMutationRequest{Action: action, ID: id}); !online || err == nil || !strings.Contains(err.Error(), "409") {
			t.Fatalf("%s accepted submitted job: %v %v", action, online, err)
		}
	}
	q := CopyMutationRequest{Action: "forget", ID: id, Confirm: true}
	if _, online, err := CallCopyMutation(ctx, p, "", q); !online || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("forgot submitted local version: online=%v err=%v", online, err)
	}
	n, err := f.meta.Resolve(ctx, "/dest")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.coll.FS.Remove(ctx, n.ParentIno, n.Name, false); err != nil {
		t.Fatal(err)
	}
	out, online, err = CallCopyMutation(ctx, p, "", q)
	if err != nil || !online || out.Forgotten != id || len(out.Copies) != 0 {
		t.Fatalf("forget: %+v %v %v", out, online, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "journal", "copies", id+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forgotten payload remains: %v", err)
	}
	if _, online, err := CallCopyMutation(ctx, p, "", q); !online || err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("repeated forget: %v %v", online, err)
	}
	if _, online, err := CallCopies(ctx, p, "", CopiesRequest{ID: id}); !online || err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("forgotten job still inspectable: %v %v", online, err)
	}
}

func TestCopyManagementRejectsUnsafeRequestsAndDoesNotReplay(t *testing.T) {
	f, _ := cacheControl(t)
	s := NewServer(f.coll)
	id := journal.NewID()
	body := fmt.Sprintf(`{"id":%q}`, id)
	confirmed := fmt.Sprintf(`{"id":%q,"confirm":true}`, id)
	for _, tc := range []struct {
		route, method, body, origin, header, content string
		status                                       int
	}{
		{"/copies/cancel", "POST", body, "https://bad.invalid", "1", "application/json", 403},
		{"/copies/retry", "POST", body, "", "", "application/json", 403},
		{"/copies/retry", "GET", body, "", "1", "application/json", 405},
		{"/copies/cancel", "POST", body, "", "1", "text/plain", 415},
		{"/copies/cancel", "POST", `{}`, "", "1", "application/json", 400},
		{"/copies/cancel", "POST", `null`, "", "1", "application/json", 400},
		{"/copies/cancel", "POST", body + `{}`, "", "1", "application/json", 400},
		{"/copies/cancel", "POST", `{"id":"../bad"}`, "", "1", "application/json", 400},
		{"/copies/cancel", "POST", `{"id":"x","action":"retry"}`, "", "1", "application/json", 400},
		{"/copies/cancel?all=true", "POST", body, "", "1", "application/json", 400},
		{"/copies/drop", "POST", body, "", "1", "application/json", 404},
		{"/copies/forget", "POST", body, "", "1", "application/json", 400},
		{"/copies/forget", "POST", confirmed, "https://bad.invalid", "1", "application/json", 403},
		{"/copies/forget", "POST", confirmed, "", "", "application/json", 403},
		{"/copies/forget", "GET", confirmed, "", "1", "application/json", 405},
		{"/copies/forget", "POST", confirmed + `{}`, "", "1", "application/json", 400},
		{"/copies/forget?all=true", "POST", confirmed, "", "1", "application/json", 400},
		{"/copies/retry", "POST", confirmed, "", "1", "application/json", 400},
		{"/copies/cancel", "POST", confirmed, "", "1", "application/json", 400},
	} {
		r := httptest.NewRequest(tc.method, "http://cloudfs"+tc.route, strings.NewReader(tc.body))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CloudFS-Control", tc.header)
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	for _, action := range []string{"cancel", "forget"} {
		calls.Store(0)
		if _, online, err := CallCopyMutation(context.Background(), "", strings.TrimPrefix(server.URL, "http://"), CopyMutationRequest{Action: action, ID: id, Confirm: action == "forget"}); !online || err == nil || calls.Load() != 1 {
			t.Fatalf("%s request replayed: %v %v calls=%d", action, online, err, calls.Load())
		}
	}
}

func TestCopyCleanupPendingRemainsQueryableAndCanFinishThroughControl(t *testing.T) {
	f, provider := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	ctx := context.Background()
	identity, err := f.meta.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.j.BeginCopy(ctx, journal.CopySpec{MetaIdentity: identity, SourcePath: "/source", SourceRemote: "ali", SourceID: "source", TargetPath: "/dest", TargetRemote: "ali", TargetParentID: "root", Mode: "writeback"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	c.Close()
	if err := f.coll.FS.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(f.dir, "journal", "copies", id+".part")
	saved := filepath.Join(t.TempDir(), "saved")
	if err := os.Rename(payload, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(payload, 0700); err != nil {
		t.Fatal(err)
	}
	p := socketPath(t)
	srv, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	q := CopyMutationRequest{Action: "forget", ID: id, Confirm: true}
	if _, online, err := CallCopyMutation(ctx, p, "", q); !online || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("ignored unexpected payload directory: %v %v", online, err)
	}
	out, online, err := CallCopies(ctx, p, "", CopiesRequest{ID: id})
	if err != nil || !online || len(out.Copies) != 1 || out.Copies[0].State != journal.CopyPurging || out.Copies[0].Warning == "" {
		t.Fatalf("pending cleanup hidden: %+v %v %v", out, online, err)
	}
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, payload); err != nil {
		t.Fatal(err)
	}
	out, online, err = CallCopyMutation(ctx, p, "", q)
	if err != nil || !online || out.Forgotten != id || provider.TotalCalls() != 0 {
		t.Fatalf("cleanup retry: %+v %v %v calls=%d", out, online, err, provider.TotalCalls())
	}
}
