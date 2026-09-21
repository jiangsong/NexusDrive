package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// TestMemoryV1LayoutUnchanged: with no marker and no configuration the
// store is v1 — keys are plain agent names, an owner-qualified key is
// refused with a pointer at the migration, and directories are where
// they always were.
func TestMemoryV1LayoutUnchanged(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	if s.Layout(ctx) != LayoutV1 {
		t.Fatalf("layout %s", s.Layout(ctx))
	}
	if k, err := ParseKey(LayoutV1, "codex", "alice"); err != nil || k.String() != "codex" {
		t.Fatalf("v1 key: %+v %v", k, err)
	}
	if _, err := ParseKey(LayoutV1, "alice/codex", "alice"); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("v1 qualified key: %v", err)
	}
	if _, err := s.Put(ctx, "codex", "style", "x\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if s.FactPath("codex", "style") != "/work/.agent/memory/codex/facts/style.md" {
		t.Fatalf("v1 path: %s", s.FactPath("codex", "style"))
	}
}

// TestMemoryLayoutV2SeparatesOwners: in v2 the key is owner/agent, an
// unqualified agent is the caller's own, personal is owner-scoped, shared
// stays drive-wide, two
// owners' agents of the same name are different directories, and Agents
// reports each with its owner.
func TestMemoryLayoutV2SeparatesOwners(t *testing.T) {
	s, _, _, _ := newStack(t, config.Memory{Layout: LayoutV2})
	ctx := context.Background()
	if s.Layout(ctx) != LayoutV2 {
		t.Fatalf("layout %s", s.Layout(ctx))
	}
	mine, err := ParseKey(LayoutV2, "codex", "alice")
	if err != nil || mine.String() != "alice/codex" {
		t.Fatalf("own key: %+v %v", mine, err)
	}
	theirs, err := ParseKey(LayoutV2, "bob/codex", "alice")
	if err != nil || theirs.String() != "bob/codex" {
		t.Fatalf("their key: %+v %v", theirs, err)
	}
	if k, err := ParseKey(LayoutV2, "shared", "alice"); err != nil || k.String() != "shared" || k.Owner != "" {
		t.Fatalf("shared: %+v %v", k, err)
	}
	if k, err := ParseKey(LayoutV2, PersonalAgent, "alice"); err != nil || k.String() != "alice/shared" {
		t.Fatalf("personal: %+v %v", k, err)
	}
	if k, err := ParseIdentity(LayoutV2, PersonalAgent, "alice"); err != nil || k.String() != "alice/personal" {
		t.Fatalf("personal client identity: %+v %v", k, err)
	}
	if k, err := ParseIdentity(LayoutV1, PersonalAgent, "alice"); err != nil || k.String() != "personal" {
		t.Fatalf("v1 personal client identity: %+v %v", k, err)
	}
	if k, err := ParseKey(LayoutV2, "alice/shared", "bob"); err != nil || k.String() != "alice/shared" {
		t.Fatalf("qualified personal: %+v %v", k, err)
	}
	if _, err := ParseKey(LayoutV2, "codex", "Not Valid"); err == nil {
		t.Fatal("bad default owner accepted")
	}
	if _, err := s.Put(ctx, mine.String(), "style", "alice's\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, theirs.String(), "style", "bob's\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, SharedAgent, "team", "everyone's\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get(ctx, "alice/codex", "style")
	b, _ := s.Get(ctx, "bob/codex", "style")
	if a.Content == b.Content || a.Path != "/work/.agent/memory/alice/codex/facts/style.md" || b.Path != "/work/.agent/memory/bob/codex/facts/style.md" {
		t.Fatalf("owners share a directory: %+v %+v", a, b)
	}
	agents, err := s.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ag := range agents {
		got[ag.Name] = ag.Owner
	}
	if len(got) != 3 || got["alice/codex"] != "alice" || got["bob/codex"] != "bob" || got["shared"] != "" {
		t.Fatalf("agents: %+v", agents)
	}
	if ag, name := s.locate("/work/.agent/memory/bob/codex/facts/style.md"); ag != "bob/codex" || name != "style" {
		t.Fatalf("locate: %s %s", ag, name)
	}
}

// TestMemoryMigrateIsIdempotent: a v1 tree moves under the owner one
// directory at a time; a run interrupted after some moves (simulated by
// a second store over the same drive) is finished by the next, which
// skips what moved, moves the rest and writes the marker only then; a
// third run changes nothing; and after it a store with no configured
// layout reads the marker and is v2.
func TestMemoryMigrateIsIdempotent(t *testing.T) {
	s, fsys, _, _ := newStack(t, config.Memory{})
	ctx := context.Background()
	for _, ag := range []string{"codex", "claude-code"} {
		if _, err := s.Put(ctx, ag, "style", ag+"\n", PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put(ctx, SharedAgent, "team", "shared\n", PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// The interruption: move one directory by hand, the way a run that
	// died after its first rename leaves the tree.
	memDir := "/work/.agent/memory"
	parent, _ := fsys.StatPath(ctx, memDir)
	if _, err := fsys.Mkdir(ctx, parent.Ino, "alice"); err != nil {
		t.Fatal(err)
	}
	owner, _ := fsys.StatPath(ctx, memDir+"/alice")
	if err := fsys.Rename(ctx, parent.Ino, "claude-code", owner.Ino, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if s.Layout(ctx) != LayoutV1 {
		t.Fatal("a half-moved tree is already v2")
	}
	moved, err := s.Migrate(ctx, "alice")
	if err != nil || len(moved) != 1 || moved[0] != "codex" {
		t.Fatalf("migrate: %v %v", moved, err)
	}
	if s.Layout(ctx) != LayoutV2 {
		t.Fatal("the marker was not written")
	}
	if moved, err := s.Migrate(ctx, "alice"); err != nil || len(moved) != 0 {
		t.Fatalf("second run: %v %v", moved, err)
	}
	agents, _ := s.Agents(ctx)
	names := map[string]bool{}
	for _, ag := range agents {
		names[ag.Name] = true
	}
	if !names["alice/codex"] || !names["alice/claude-code"] || !names["shared"] || len(names) != 3 {
		t.Fatalf("after migration: %+v", agents)
	}
	if f, err := s.Get(ctx, "alice/codex", "style"); err != nil || f.Content != "codex\n" {
		t.Fatalf("moved fact: %+v %v", f, err)
	}
	if _, err := s.Get(ctx, "codex", "style"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the v1 path still answers: %v", err)
	}
	// A store that follows the marker.
	fresh := New(Options{FS: fsys, Config: s.cfg})
	if fresh.Layout(ctx) != LayoutV2 {
		t.Fatal("a fresh store does not read the marker")
	}
	if _, err := s.Migrate(ctx, "shared"); err == nil {
		t.Fatal("shared accepted as an owner")
	}
}

// TestMemoryPutChecksRemoteVersion: a put that names the remote version
// it read is refused when the drive's version moved since (another
// device wrote), and not when only the local version moved (this
// device's own write, still uploading); both checks together fall back
// to the content when the remote is unchanged.
func TestMemoryPutChecksRemoteVersion(t *testing.T) {
	s, _, fake, up := newStack(t, config.Memory{})
	ctx := context.Background()
	f, err := s.Put(ctx, "codex", "style", "one\n", PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f.RemoteVersion != "" {
		t.Fatalf("a local-only fact has a remote version: %q", f.RemoteVersion)
	}
	if _, err := up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ = s.Get(ctx, "codex", "style")
	if f.RemoteVersion == "" {
		t.Fatal("an uploaded fact has no remote version")
	}
	// This device writes again: Version moves, RemoteVersion does not
	// until the upload lands. A put expecting the old remote version is
	// still accepted.
	up.Stop()
	if _, err := s.Put(ctx, "codex", "style", "two\n", PutOptions{ExpectedRemoteVersion: f.RemoteVersion}); err != nil {
		t.Fatalf("own pending write refused: %v", err)
	}
	// Another device's write lands on the drive: RemoteVersion moves.
	fake.Seed(strings.TrimPrefix(f.Path, "/"), []byte("theirs\n"))
	if err := refreshDir(ctx, s, "/work/.agent/memory/codex/facts"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, "codex", "style", "three\n", PutOptions{ExpectedRemoteVersion: f.RemoteVersion})
	if !errors.Is(err, ErrVersionChanged) || !strings.Contains(err.Error(), "on the drive") {
		t.Fatalf("moved remote version accepted: %v", err)
	}
	cur, _ := s.Get(ctx, "codex", "style")
	if _, err := s.Put(ctx, "codex", "style", "four\n", PutOptions{ExpectedRemoteVersion: cur.RemoteVersion, ExpectedVersion: cur.Version}); err != nil {
		t.Fatalf("current versions refused: %v", err)
	}
}

// refreshDir relists a directory so the drive's version of what is under
// it is what the store sees.
func refreshDir(ctx context.Context, s *Store, dir string) error {
	a, err := s.fs.StatPath(ctx, dir)
	if err != nil {
		return err
	}
	return s.fs.(interface {
		Refresh(context.Context, uint64) error
	}).Refresh(ctx, a.Ino)
}
