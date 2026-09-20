package meta

import (
	"context"
	"testing"
	"time"
)

// TestSetModePersists: chmod(2) is the only source of POSIX permissions in
// this filesystem, so what SetMode writes must be what Get reads back.
func TestSetModePersists(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "script.sh", 10)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	n, err := s.Lookup(ctx, RootIno, "script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if n.Mode != 0o644 {
		t.Fatalf("fresh file mode = %#o, want %#o", n.Mode, 0o644)
	}
	if err := s.SetMode(ctx, n.Ino, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, n.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode = %#o, want %#o", got.Mode, 0o755)
	}
}

// TestSetModeSurvivesPutDir: no provider reports a POSIX mode, so every
// listing carries the default. A refresh that updates the file's size must
// not drag the default back over what chmod(2) set, or an executable script
// on the mount silently loses its exec bit the next time its directory goes
// stale.
func TestSetModeSurvivesPutDir(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "script.sh", 10)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	n, err := s.Lookup(ctx, RootIno, "script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, n.Ino, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "script.sh", 20)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, n.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 20 {
		t.Fatalf("size after refresh = %d, want 20", got.Size)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode after refresh = %#o, want %#o", got.Mode, 0o755)
	}
}

// TestSetModeSurvivesDirListing is TestSetModeSurvivesPutDir over the paged
// listing path, which merges through its own statement.
func TestSetModeSurvivesDirListing(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "script.sh", 10)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	n, err := s.Lookup(ctx, RootIno, "script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, n.Ino, 0o755); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	l := beginListingTest(t, s, RootIno)
	if err := l.Append(ctx, []Node{file(0, "script.sh", 20)}); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, n.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 20 {
		t.Fatalf("size after listing = %d, want 20", got.Size)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode after listing = %#o, want %#o", got.Mode, 0o755)
	}
}

// TestSetModeKeepsPermissionBitsOnly: the mount carries nosuid, so storing a
// setuid bit would make ls(1) report a permission the kernel will not honour.
func TestSetModeKeepsPermissionBitsOnly(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "script.sh", 10)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	n, err := s.Lookup(ctx, RootIno, "script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, n.Ino, 0o4755); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, n.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode = %#o, want %#o with the setuid bit dropped", got.Mode, 0o755)
	}
}

// TestSetModeSurvivesRemoteChange: the change feed folds a remote attribute
// update into the cached node, carrying the default mode like every other
// remote event. A remote that has no permissions to report must not take the
// exec bit off a file by reporting a new size.
func TestSetModeSurvivesRemoteChange(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "script.sh", 10)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	old, err := s.Lookup(ctx, RootIno, "script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, old.Ino, 0o755); err != nil {
		t.Fatal(err)
	}
	// chmod(2) makes the caller's snapshot stale exactly as any other
	// attribute write does, so the feed rereads before it applies.
	old, err = s.Get(ctx, old.Ino)
	if err != nil {
		t.Fatal(err)
	}
	updated := old
	updated.Version, updated.Size = "v2", 42
	// The event itself carries the default: the remote has no permissions.
	updated.Mode = 0o644
	if _, applied, err := s.ApplyRemoteNode(ctx, old, &updated, nil); err != nil || !applied {
		t.Fatalf("attribute update: applied=%v err=%v", applied, err)
	}
	got, err := s.Get(ctx, old.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 42 {
		t.Fatalf("size after remote change = %d, want 42", got.Size)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode after remote change = %#o, want %#o", got.Mode, 0o755)
	}
}
