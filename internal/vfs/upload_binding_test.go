package vfs

import (
	"context"
	"errors"
	"os"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
)

func TestWritePersistsUploadBinding(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/bound.txt", []byte("bound"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	u := rows[0]
	identity, err := e.store.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u.MetaIdentity != identity || u.MountPrefix != "/ali" || u.MountRootID != e.fs.mounts[0].RootID || u.AccountBinding != e.fs.mounts[0].AccountBinding {
		t.Fatalf("missing upload binding: %+v", u)
	}
}

func TestUploadBindingMismatchDeadLettersBeforeProviderIO(t *testing.T) {
	for _, changed := range []string{"metadata", "prefix", "root", "account", "readonly", "shadow"} {
		t.Run(changed, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			if _, err := e.fs.WriteFile(ctx, "/ali/fenced.txt", []byte("retained"), false); err != nil {
				t.Fatal(err)
			}
			rows, err := e.j.Pending(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("queue: %+v %v", rows, err)
			}
			u := rows[0]
			switch changed {
			case "metadata":
				if _, err := e.store.DB().Exec("UPDATE store_identity SET id='another-database'"); err != nil {
					t.Fatal(err)
				}
			case "prefix":
				e.fs.mounts[0].Prefix = "/other"
			case "root":
				e.fs.mounts[0].RootID = "another-root"
			case "account":
				e.fs.mounts[0].AccountBinding = "another-account"
			case "readonly":
				e.fs.mounts[0].Mode = config.ModeReadonly
			case "shadow":
				shadow := e.fs.mounts[0]
				shadow.Prefix = "/ali/fenced.txt"
				shadow.RootID = "shadow-root"
				e.fs.mounts = append([]Mount{shadow}, e.fs.mounts...)
			}
			before := e.fake.TotalCalls()
			if _, err := e.up.DrainAll(ctx); err != nil {
				t.Fatal(err)
			}
			if e.fake.TotalCalls() != before {
				t.Fatalf("binding failure made %d provider calls", e.fake.TotalCalls()-before)
			}
			got, err := e.j.Get(ctx, u.ID)
			if err != nil || got.State != journal.StateDead || got.LastError == "" {
				t.Fatalf("row: %+v %v", got, err)
			}
			if _, err := os.Stat(u.BlobPath); err != nil {
				t.Fatalf("payload not retained: %v", err)
			}
		})
	}
}

func TestLegacyUnboundUploadIsNotAuthorized(t *testing.T) {
	e := newEnv(t, envOpt{})
	// Matching the sentinel is the contract; the wrapped text names the fence.
	err := e.fs.validateUploadBinding(context.Background(), journal.Upload{Remote: "ali"})
	if !errors.Is(err, ErrUploadBindingChanged) {
		t.Fatalf("legacy upload authorization=%v", err)
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("authorization queried provider")
	}
}

func TestConfirmedResumeAdoptsCurrentAccountBinding(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := context.Background()
	e.fs.mounts[0].AccountBinding = "new-account-generation"
	if err := e.fs.ResumeUpload(ctx, u.ID, false); err == nil {
		t.Fatal("missing confirmation accepted")
	}
	if err := e.fs.ResumeUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	row, err := e.j.Get(ctx, u.ID)
	if err != nil || row.AccountBinding != "new-account-generation" || row.State != journal.StatePending {
		t.Fatalf("resume did not rebind: %+v %v", row, err)
	}
	if _, err := e.up.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
