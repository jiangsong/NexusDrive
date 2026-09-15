package vfs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// expectChange reads the next change and checks the operation it announces
// and where it came from. p, when set, must be among the change's paths.
func expectChange(t *testing.T, ch <-chan Change, kind ChangeKind, origin Origin, p string) Change {
	t.Helper()
	c := nextChange(t, ch)
	if c.Kind != kind || c.Origin != origin {
		t.Fatalf("change %+v: kind/origin = %s/%s, want %s/%s", c, c.Kind, c.Origin, kind, origin)
	}
	if p != "" {
		found := false
		for _, got := range c.Paths {
			found = found || got == p
		}
		if !found {
			t.Fatalf("change %+v does not name %s", c, p)
		}
	}
	return c
}

func TestKindAndOriginNamesAreLowerCase(t *testing.T) {
	kinds := map[ChangeKind]string{
		KindWrite: "write", KindCreate: "create", KindMkdir: "mkdir", KindRemove: "remove",
		KindRename: "rename", KindRemote: "remote", KindRescan: "rescan",
	}
	for k, want := range kinds {
		if k.String() != want {
			t.Errorf("ChangeKind(%d).String() = %q, want %q", k, k.String(), want)
		}
	}
	origins := map[Origin]string{OriginKernel: "kernel", OriginAPI: "api", OriginRemote: "remote"}
	for o, want := range origins {
		if o.String() != want {
			t.Errorf("Origin(%d).String() = %q, want %q", o, o.String(), want)
		}
	}
	if ChangeKind(0).String() != "unknown" || Origin(0).String() != "unknown" {
		t.Fatal("the zero values must not impersonate a real kind or origin")
	}
}

func TestWithOriginRoundTrips(t *testing.T) {
	ctx := context.Background()
	if got := OriginName(ctx); got != "" {
		t.Fatalf("bare context carries origin %q", got)
	}
	if got := originOf(ctx); got != OriginRemote {
		t.Fatalf("bare context classified as %s, want remote", got)
	}
	mcp := WithOrigin(ctx, "mcp")
	if got := OriginName(mcp); got != "mcp" {
		t.Fatalf("OriginName = %q, want mcp", got)
	}
	if got := originOf(mcp); got != OriginAPI {
		t.Fatalf("WithOrigin context classified as %s, want api", got)
	}
	if got := originOf(FromKernel(ctx)); got != OriginKernel {
		t.Fatalf("kernel context classified as %s, want kernel", got)
	}
	// An empty name marks nothing: the adapter must say who it is.
	if got := originOf(WithOrigin(ctx, "")); got != OriginRemote {
		t.Fatalf("empty origin name classified as %s, want remote", got)
	}
}

func TestAffectsIgnoresKindAndOrigin(t *testing.T) {
	base := Change{Paths: []string{"/ali/dir"}, Subtree: true}
	for _, kind := range []ChangeKind{KindWrite, KindCreate, KindMkdir, KindRemove, KindRename, KindRemote, KindRescan} {
		for _, origin := range []Origin{OriginKernel, OriginAPI, OriginRemote} {
			c := base
			c.Kind, c.Origin = kind, origin
			for _, p := range []string{"/ali/dir", "/ali", "/ali/dir/child"} {
				if c.Affects(p) != base.Affects(p) {
					t.Fatalf("%s/%s changed Affects(%s)", kind, origin, p)
				}
			}
			if c.Affects("/ali/other") {
				t.Fatalf("%s/%s made an unrelated path affected", kind, origin)
			}
		}
	}
	rescan := Change{Rescan: true, Kind: KindRescan, Origin: OriginRemote}
	if !rescan.Affects("/anything") {
		t.Fatal("a rescan stopped affecting every path")
	}
}

// A write handle the kernel opened, wrote and flushed: the create is
// announced when the name appears, the write when FLUSH commits it.
func TestKernelCreateAndFlushAreTaggedKernel(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := FromKernel(context.Background())
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	h, err := e.fs.Create(ctx, root.Ino, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindCreate, OriginKernel, "/ali/new.txt")
	if _, err := e.fs.Write(ctx, h, []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	noChange(t, ch)
	if err := e.fs.Sync(ctx, h); err != nil { // FUSE FLUSH
		t.Fatal(err)
	}
	expectChange(t, ch, KindWrite, OriginKernel, "/ali/new.txt")
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	noChange(t, ch) // nothing was written after the flush
}

// WriteFile through an API adapter: a new path is a create followed by the
// write that fills it; an existing path is only a write.
func TestWriteFileViaAPIIsCreateThenWrite(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := WithOrigin(context.Background(), "mcp")
	if _, err := e.fs.StatPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if _, err := e.fs.WriteFile(ctx, "/ali/notes.md", []byte("v1"), false); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindCreate, OriginAPI, "/ali/notes.md")
	expectChange(t, ch, KindWrite, OriginAPI, "/ali/notes.md")
	noChange(t, ch)
	if _, err := e.fs.WriteFile(ctx, "/ali/notes.md", []byte("v2"), false); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindWrite, OriginAPI, "/ali/notes.md")
	noChange(t, ch)
}

func TestMkdirRemoveAndRenameCarryTheirKind(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := WithOrigin(context.Background(), "control")
	e.fake.Seed("dir/child", []byte("x"))
	if _, err := e.fs.StatPath(ctx, "/ali/dir/child"); err != nil {
		t.Fatal(err)
	}
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if _, err := e.fs.Mkdir(ctx, root.Ino, "made"); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindMkdir, OriginAPI, "/ali/made")
	if err := e.fs.Rename(ctx, root.Ino, "dir", root.Ino, "moved"); err != nil {
		t.Fatal(err)
	}
	c := expectChange(t, ch, KindRename, OriginAPI, "/ali/dir")
	if len(c.Paths) != 2 || c.Paths[1] != "/ali/moved" || !c.Subtree {
		t.Fatalf("rename lost its destination: %+v", c)
	}
	if err := e.fs.Remove(ctx, root.Ino, "moved", true); err != nil {
		t.Fatal(err)
	}
	c = expectChange(t, ch, KindRemove, OriginAPI, "/ali/moved")
	if !c.Subtree {
		t.Fatalf("directory removal lost its subtree flag: %+v", c)
	}
	if err := e.fs.Remove(FromKernel(ctx), root.Ino, "made", false); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindRemove, OriginKernel, "/ali/made")
	noChange(t, ch)
}

// Changes the delta feed discovers are remote whatever context the poller
// happens to run under.
func TestDeltaRefreshIsRemote(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/ali/file"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	r := NewRefresher(e.fs, time.Hour)
	e.fake.Seed("file", []byte("changed remotely"))
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindRemote, OriginRemote, "/ali/file")
	n, _ := e.store.Resolve(ctx, "/ali/file")
	if err := e.fake.Delete(ctx, n.RemoteID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindRemote, OriginRemote, "/ali/file")
	noChange(t, ch)
}

// A listing reads the remote on behalf of whoever asked for it, but what it
// finds changed there is not that caller's doing: a kernel readdir that
// notices a new file must not be reported as a kernel write.
func TestListingChangesAreRemoteEvenForAKernelReaddir(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("old/child", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/ali/old/child"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	old, _ := e.store.Resolve(ctx, "/ali/old")
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if err := e.fake.Delete(ctx, old.RemoteID); err != nil {
		t.Fatal(err)
	}
	e.fake.Seed("new", []byte("new"))
	e.clk.advance(time.Second)
	if err := e.fs.Refresh(FromKernel(ctx), root.Ino); err != nil {
		t.Fatal(err)
	}
	c := expectChange(t, ch, KindRemote, OriginRemote, "/ali/new")
	if !c.Affects("/ali/old/child") {
		t.Fatalf("listing lost the removed subtree: %+v", c)
	}
	noChange(t, ch)
}

// An upload landing rewrites the node's identity to the remote's: that is a
// remote change even though the bytes were written locally.
func TestUploadLandingIsRemote(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := WithOrigin(context.Background(), "mcp")
	if _, err := e.fs.StatPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if _, err := e.fs.WriteFile(ctx, "/ali/up.txt", []byte("payload"), false); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindCreate, OriginAPI, "/ali/up.txt")
	expectChange(t, ch, KindWrite, OriginAPI, "/ali/up.txt")
	if _, err := e.up.DrainAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindRemote, OriginRemote, "/ali/up.txt")
	noChange(t, ch)
}

func TestCopyAnnouncesACreate(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := WithOrigin(context.Background(), "webdav")
	e.fake.Seed("source", []byte("complete local content"))
	if _, err := e.fs.StatPath(ctx, "/ali/source"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err != nil {
		t.Fatal(err)
	}
	expectChange(t, ch, KindCreate, OriginAPI, "/ali/dest")
}

func TestQueueOverflowIsARescanFromRemote(t *testing.T) {
	e := newEnv(t, envOpt{})
	slow, cancel := e.fs.WatchChanges()
	defer cancel()
	for i := range 200 {
		e.fs.emitChange(Change{Paths: []string{fmt.Sprintf("/ali/%d", i)}, Kind: KindWrite, Origin: OriginKernel})
	}
	var rescan *Change
	for len(slow) > 0 {
		c := <-slow
		if c.Rescan {
			rescan = &c
		}
	}
	if rescan == nil {
		t.Fatal("overflow produced no rescan")
	}
	if rescan.Kind != KindRescan || rescan.Origin != OriginRemote {
		t.Fatalf("overflow rescan tagged %s/%s, want rescan/remote", rescan.Kind, rescan.Origin)
	}
	// A change naming more paths than a consumer should walk collapses to a
	// rescan the same way, whatever the caller said it was.
	wide := Change{Kind: KindRename, Origin: OriginAPI}
	for i := range 129 {
		wide.Paths = append(wide.Paths, fmt.Sprintf("/ali/wide/%d", i))
	}
	e.fs.emitChange(wide)
	c := nextChange(t, slow)
	if !c.Rescan || c.Kind != KindRescan || c.Origin != OriginRemote {
		t.Fatalf("oversized change collapsed to %+v, want a remote rescan", c)
	}
}
