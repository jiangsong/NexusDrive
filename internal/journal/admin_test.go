package journal

import (
	"context"
	"errors"
	"testing"
)

func TestRequeueRefusesActiveAndCompletedRows(t *testing.T) {
	ctx := context.Background()
	for _, state := range []State{StatePending, StateUploading, StateDone} {
		t.Run(string(state), func(t *testing.T) {
			j, _, _ := openTest(t)
			u := stage(t, j, "a.txt", []byte("keep"))
			if err := j.Commit(ctx, u); err != nil {
				t.Fatal(err)
			}
			if state == StateUploading {
				if _, err := j.Claim(ctx, "ali", 1); err != nil {
					t.Fatal(err)
				}
			}
			if state == StateDone {
				if err := j.Succeed(ctx, u.ID); err != nil {
					t.Fatal(err)
				}
			}
			want := ErrNotDead
			if state == StateUploading {
				want = ErrInFlight
			}
			if err := j.Requeue(ctx, u.ID); !errors.Is(err, want) {
				t.Fatalf("retry %s = %v", state, err)
			}
			got, err := j.Get(ctx, u.ID)
			if err != nil || got.State != state {
				t.Fatalf("retry changed row: %+v, %v", got, err)
			}
		})
	}
}
