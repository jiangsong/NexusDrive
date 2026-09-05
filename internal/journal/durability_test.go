package journal

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestCommitDoesNotAcknowledgeFailedDirectorySync(t *testing.T) {
	j, _, _ := openTest(t)
	u := stage(t, j, "durable", []byte("must be recoverable"))
	saved := j.ObjectsDir() + ".saved"
	if err := os.Rename(j.ObjectsDir(), saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Rename(saved, j.ObjectsDir()) })
	ctx := context.Background()
	if err := j.Commit(ctx, u); err == nil {
		t.Fatal("commit acknowledged without syncing objects directory")
	}
	if _, err := j.Get(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed commit created row: %v", err)
	}
	if err := os.Rename(saved, j.ObjectsDir()); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(ctx, u); err != nil {
		t.Fatalf("retry after restoring storage: %v", err)
	}
}
