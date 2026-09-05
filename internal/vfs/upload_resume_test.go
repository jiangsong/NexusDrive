package vfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
)

func stoppedUpload(t *testing.T) (*env, journal.Upload) {
	t.Helper()
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/resume.txt", []byte("retained"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	if _, err := e.up.Cancel(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	return e, rows[0]
}

func TestUploadResumeCompletesCurrentLocalVersion(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := context.Background()
	if err := e.fs.ResumeUpload(ctx, u.ID, false); err == nil {
		t.Fatal("missing confirmation accepted")
	}
	if err := e.fs.ResumeUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(time.Second)
	if _, err := e.up.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/resume.txt", 0, 8)
	if err != nil || string(got) != "retained" {
		t.Fatalf("read: %q %v", got, err)
	}
	row, err := e.j.Get(ctx, u.ID)
	if err != nil || row.State != journal.StateDone {
		t.Fatalf("not completed: %+v %v", row, err)
	}
}

func TestUploadResumeRejectsReplacementMountAndNewCancellation(t *testing.T) {
	for _, kind := range []string{"replacement", "replacement-during-check", "mount", "readonly", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			e, u := stoppedUpload(t)
			ctx := context.Background()
			var want error = ErrUploadResumeTarget
			switch kind {
			case "replacement-during-check":
				e.fs.uploadResumeFault = func() {
					if _, err := e.fs.WriteFile(ctx, "/ali/resume.txt", []byte("newer"), false); err != nil {
						t.Fatal(err)
					}
				}
			case "replacement":
				if _, err := e.fs.WriteFile(ctx, "/ali/resume.txt", []byte("newer"), false); err != nil {
					t.Fatal(err)
				}
			case "mount":
				e.fs.mounts[0].RootID = "another-root"
			case "readonly":
				e.fs.mounts[0].Mode = config.ModeReadonly
				want = ErrReadOnly
			case "cancel":
				e.fs.uploadResumeFault = func() {
					if _, err := e.up.Cancel(ctx, u.ID); err != nil {
						t.Fatal(err)
					}
				}
				want = journal.ErrResumeChanged
			}
			if err := e.fs.ResumeUpload(ctx, u.ID, true); !errors.Is(err, want) {
				t.Fatalf("resume=%v want %v", err, want)
			}
			row, err := e.j.Get(ctx, u.ID)
			if err != nil || row.State != journal.StateCancelled {
				t.Fatalf("stop lost: %+v %v", row, err)
			}
		})
	}
}

func TestUploadResumeSubmittedCopyHonorsOriginalMetadataBinding(t *testing.T) {
	for _, changed := range []bool{false, true} {
		e := newEnv(t, envOpt{})
		ctx := context.Background()
		e.fake.Seed("source", []byte("copy contents"))
		if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/copied"); err != nil {
			t.Fatal(err)
		}
		rows, err := e.j.Pending(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("copy upload: %+v %v", rows, err)
		}
		if _, err := e.up.Cancel(ctx, rows[0].ID); err != nil {
			t.Fatal(err)
		}
		if changed {
			if _, err := e.store.DB().Exec(`UPDATE store_identity SET id='another-database'`); err != nil {
				t.Fatal(err)
			}
		}
		err = e.fs.ResumeUpload(ctx, rows[0].ID, true)
		if changed {
			if !errors.Is(err, ErrCopyBindingChanged) {
				t.Fatalf("foreign metadata accepted: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}
