package vfs

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/journal"
)

func TestUploadAdministrationBindsMutationsToCurrentAuthorizedPath(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	a, err := e.fs.WriteFile(ctx, "/ali/managed", []byte("retained"), false)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.ByIno(ctx, a.Ino)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	id := rows[0].ID
	info, err := e.fs.InspectUpload(ctx, id)
	if err != nil || info.Path != "/ali/managed" || info.ID != id || info.Remote != "ali" {
		t.Fatalf("inspection: %+v %v", info, err)
	}
	if err := e.j.Fail(ctx, id, errors.New("private backend failure")); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RetryUploadAt(ctx, id, "/ali/outside"); !errors.Is(err, ErrUploadManagementTarget) {
		t.Fatalf("wrong-path retry: %v", err)
	}
	if row, err := e.j.Get(ctx, id); err != nil || row.State != journal.StateDead {
		t.Fatalf("wrong-path retry changed state: %+v %v", row, err)
	}
	if err := e.fs.RetryUploadAt(ctx, id, "/ali/managed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.CancelUploadAt(ctx, id, "/ali/outside"); !errors.Is(err, ErrUploadManagementTarget) {
		t.Fatalf("wrong-path cancel: %v", err)
	}
	if row, err := e.j.Get(ctx, id); err != nil || row.State != journal.StatePending {
		t.Fatalf("wrong-path cancel changed state: %+v %v", row, err)
	}
	if state, err := e.fs.CancelUploadAt(ctx, id, "/ali/managed"); err != nil || state != journal.StateCancelled {
		t.Fatalf("cancel: %s %v", state, err)
	}
	if err := e.fs.ResumeUploadAt(ctx, id, "/ali/outside", true); !errors.Is(err, ErrUploadManagementTarget) {
		t.Fatalf("wrong-path resume: %v", err)
	}
	if row, err := e.j.Get(ctx, id); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("wrong-path resume changed state: %+v %v", row, err)
	}
	if err := e.fs.ResumeUploadAt(ctx, id, "/ali/managed", true); err != nil {
		t.Fatal(err)
	}
}

func TestInspectUploadDoesNotGuessAPathAfterNamespaceReplacement(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/resume.txt", []byte("replacement"), false); err != nil {
		t.Fatal(err)
	}
	info, err := e.fs.InspectUpload(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != "" || info.ID != u.ID || info.State != journal.StateCancelled {
		t.Fatalf("stale upload path was guessed: %+v", info)
	}
}

func TestScopedUploadResumeRechecksPathAfterUnlockedContentVerification(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := context.Background()
	e.fs.uploadResumeFault = func() {
		if _, err := e.fs.WriteFile(ctx, "/ali/resume.txt", []byte("replacement during verification"), false); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.fs.ResumeUploadAt(ctx, u.ID, "/ali/resume.txt", true); !errors.Is(err, ErrUploadResumeTarget) && !errors.Is(err, ErrUploadManagementTarget) {
		t.Fatalf("scoped resume accepted a replaced path: %v", err)
	}
	row, err := e.j.Get(ctx, u.ID)
	if err != nil || row.State != journal.StateCancelled {
		t.Fatalf("old stopped upload changed: %+v %v", row, err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/resume.txt", 0, 64)
	if err != nil || string(got) != "replacement during verification" {
		t.Fatalf("replacement was damaged: %q %v", got, err)
	}
}
