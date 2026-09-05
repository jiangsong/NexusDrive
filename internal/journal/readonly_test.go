package journal

import (
	"context"
	"os"
	"testing"
)

func TestReadOnlyInspectionDoesNotOwnOrRecoverQueue(t *testing.T) {
	j, _, dir := openTest(t)
	u := stage(t, j, "waiting", []byte("keep"))
	ctx := context.Background()
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Owner() || ro.committer != nil {
		t.Fatal("inspection started queue ownership")
	}
	if got, err := ro.Get(ctx, u.ID); err != nil || got.State != StatePending {
		t.Fatalf("read: %+v %v", got, err)
	}
	if err := ro.Fail(ctx, u.ID, os.ErrPermission); err == nil {
		t.Fatal("inspection mutated queue")
	}
	if got, _ := j.Get(ctx, u.ID); got.State != StatePending {
		t.Fatal("inspection changed upload state")
	}
	if _, err := OpenReadOnly(t.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("missing journal: %v", err)
	}
}
