package vfs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudfs/internal/journal"
)

// TestBindingRejectionNamesTheFenceThatFailed: validateUploadBinding fences a
// queued upload for a dozen different reasons and used to return one bare
// sentinel for all of them. A dead letter then told the operator the account
// binding had changed even when the real reason was that the file had been
// removed from the metadata store, which sends them looking at the wrong
// thing. Each rejection must say which fence it failed.
func TestBindingRejectionNamesTheFenceThatFailed(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T, e *env, u journal.Upload) journal.Upload
		want  string
	}{
		{
			name: "node removed from the metadata store",
			setup: func(t *testing.T, e *env, u journal.Upload) journal.Upload {
				if err := e.store.Remove(ctx, u.Ino); err != nil {
					t.Fatal(err)
				}
				return u
			},
			want: "no longer in the metadata store",
		},
		{
			name: "account binding really did change",
			setup: func(t *testing.T, e *env, u journal.Upload) journal.Upload {
				e.fs.mounts[0].AccountBinding = "another-account"
				return u
			},
			want: "account binding",
		},
		{
			name: "upload predates the binding fields",
			setup: func(t *testing.T, e *env, u journal.Upload) journal.Upload {
				return journal.Upload{Remote: "ali"}
			},
			want: "carries no binding",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			if _, err := e.fs.WriteFile(ctx, "/ali/fenced.txt", []byte("retained"), false); err != nil {
				t.Fatal(err)
			}
			rows, err := e.j.Pending(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("queue: %+v %v", rows, err)
			}
			u := tc.setup(t, e, rows[0])

			err = e.fs.validateUploadBinding(ctx, u)
			if !errors.Is(err, ErrUploadBindingChanged) {
				t.Fatalf("err = %v, want it to match ErrUploadBindingChanged", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDeadLetterReportsTheRealFence: the text an operator reads in the
// transfer queue is the uploader's wrapper plus the fence's reason. The
// wrapper used to assert a cause of its own ("local account binding rejected
// this task"), so every rejection blamed the account no matter what happened.
func TestDeadLetterReportsTheRealFence(t *testing.T) {
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
	if err := e.store.Remove(ctx, u.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := e.j.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != journal.StateDead {
		t.Fatalf("state = %q, want dead", got.State)
	}
	if !strings.Contains(got.LastError, "no longer in the metadata store") {
		t.Fatalf("dead letter does not name the real fence: %q", got.LastError)
	}
	if strings.Contains(got.LastError, "account binding") {
		t.Fatalf("dead letter still blames the account binding: %q", got.LastError)
	}
}
