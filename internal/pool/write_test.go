package pool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// upload pushes content through the pool's session protocol the way the
// uploader does, and returns the entry the pool reports.
func upload(t *testing.T, ctx context.Context, p *Pool, parentID, name string, content []byte) provider.Entry {
	t.Helper()
	h := provider.Hashes{provider.HashSHA1: sha1hex(content)}
	sess, err := p.BeginUpload(ctx, parentID, name, int64(len(content)), h)
	if err != nil {
		t.Fatalf("begin %s: %v", name, err)
	}
	if sess.RapidDone {
		return *sess.Entry
	}
	var parts []provider.PartToken
	for i, off := 0, int64(0); off < int64(len(content)) || i == 0; i++ {
		n := sess.PartSize
		if n <= 0 || off+n > int64(len(content)) {
			n = int64(len(content)) - off
		}
		pt, err := p.UploadPart(ctx, sess, i, bytes.NewReader(content[off:off+n]), n)
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		parts = append(parts, pt)
		off += n
		if n == 0 {
			break
		}
	}
	e, err := p.CompleteUpload(ctx, sess, parts)
	if err != nil {
		t.Fatalf("complete %s: %v", name, err)
	}
	return e
}

func sha1hex(b []byte) string {
	f := fakeprovider.New("h")
	return f.Seed("/x", b).Hashes[provider.HashSHA1]
}

func TestCompleteUploadReturnsAdoptableEntry(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	docs, err := p.Mkdir(ctx, rootID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	e := upload(t, ctx, p, docs.ID, "new.txt", []byte("first"))
	if !strings.HasPrefix(e.ID, idPrefix) || e.Name != "new.txt" || e.Size != 5 || e.Version == "" || e.ModTime.IsZero() || e.Hashes[provider.HashSHA1] == "" || e.ParentID != docs.ID {
		t.Fatalf("entry the VFS must adopt is incomplete: %+v", e)
	}
	// The file is at its real path on the member that took it.
	if got := strings.Join(a.Tree(), ","); got != "/docs/,/docs/new.txt" {
		t.Fatalf("member a tree = %s", got)
	}
	if got := readAll(t, p, e.ID); got != "first" {
		t.Fatalf("read back = %q", got)
	}
	// Overwriting keeps the id and changes the version.
	e2 := upload(t, ctx, p, docs.ID, "new.txt", []byte("second!"))
	if e2.ID != e.ID || e2.Version == e.Version || e2.Size != 7 {
		t.Fatalf("overwrite: %+v vs %+v", e, e2)
	}
	if got := readAll(t, p, e.ID); got != "second!" {
		t.Fatalf("read after overwrite = %q", got)
	}
	// Reading with the old version is a conflict, as with any drive.
	if _, err := p.ReadRange(ctx, e.ID, e.Version, 0, 1); !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("stale version read = %v", err)
	}
}

func TestBeginUploadDoesNotTouchTheTree(t *testing.T) {
	a := fakeprovider.New("a")
	a.Seed("/keep.txt", []byte("old"))
	p := newTestPool(t, t.TempDir(), a)
	ctx := context.Background()
	entries, _, _ := p.List(ctx, rootID, "")
	keep, _ := find(entries, "keep.txt")
	sess, err := p.BeginUpload(ctx, rootID, "keep.txt", 3, nil)
	if err != nil || sess.RapidDone {
		t.Fatalf("begin: %+v, %v", sess, err)
	}
	// Abandoned session: the old content is untouched and still listed.
	got, _ := p.Stat(ctx, keep.ID)
	if got.Version != keep.Version || readAll(t, p, keep.ID) != "old" {
		t.Fatalf("an unfinished upload changed the tree: %+v", got)
	}
}

func TestRapidUploadFinalizesInBegin(t *testing.T) {
	a := fakeprovider.New("a")
	a.Seed("/elsewhere.bin", []byte("dedupe me"))
	p := newTestPool(t, t.TempDir(), a)
	ctx := context.Background()
	content := []byte("dedupe me")
	sess, err := p.BeginUpload(ctx, rootID, "copy.bin", int64(len(content)), provider.Hashes{provider.HashSHA1: sha1hex(content)})
	if err != nil || !sess.RapidDone || sess.Entry == nil || sess.Entry.Name != "copy.bin" {
		t.Fatalf("rapid: %+v, %v", sess, err)
	}
	if a.Calls("UploadPart") != 0 {
		t.Fatal("a rapid upload sent parts")
	}
	if got := readAll(t, p, sess.Entry.ID); got != "dedupe me" {
		t.Fatalf("rapid content = %q", got)
	}
}

func TestWriteWhenPrimaryDownFallsToNextMember(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	e := upload(t, ctx, p, rootID, "f.txt", []byte("landed"))
	if got, ok := b.Content("/f.txt"); !ok || string(got) != "landed" {
		t.Fatalf("b has %q, %v", got, ok)
	}
	if got := readAll(t, p, e.ID); got != "landed" {
		t.Fatalf("read = %q", got)
	}
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := p.BeginUpload(ctx, rootID, "g.txt", 1, nil); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("with every member down = %v, want ErrUnavailable", err)
	}
}

func TestMkdirMirrorsOnEveryMember(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	d, err := p.Mkdir(ctx, rootID, "photos")
	if err != nil || d.Kind != provider.KindDir || d.Version == "" {
		t.Fatalf("mkdir: %+v, %v", d, err)
	}
	sub, err := p.Mkdir(ctx, d.ID, "2026")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		if got := strings.Join(f.Tree(), ","); got != "/photos/,/photos/2026/" {
			t.Fatalf("%s tree = %s", f.Name(), got)
		}
	}
	// Listing agrees, with the same ids.
	entries, _, _ := p.List(ctx, rootID, "")
	if got, _ := find(entries, "photos"); got.ID != d.ID {
		t.Fatalf("mkdir id %s, listed %s", d.ID, got.ID)
	}
	inner, _, _ := p.List(ctx, d.ID, "")
	if got, _ := find(inner, "2026"); got.ID != sub.ID {
		t.Fatalf("nested id drifted")
	}
	if _, err := p.Mkdir(ctx, rootID, "photos"); !errors.Is(err, provider.ErrExists) {
		// Both members refuse; the pool reports what they said.
		t.Fatalf("mkdir existing = %v", err)
	}
}

func TestRenameKeepsIDAndFansOut(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/docs/a.txt", []byte("A"))
	b.Seed("/docs/a.txt", []byte("A"))
	b.Seed("/docs/deep/b.txt", []byte("B"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	root, _, _ := p.List(ctx, rootID, "")
	docs, _ := find(root, "docs")
	inner, _, _ := p.List(ctx, docs.ID, "")
	file, _ := find(inner, "a.txt")
	deep, _ := find(inner, "deep")
	deepInner, _, _ := p.List(ctx, deep.ID, "")
	bfile := deepInner[0]

	e, err := p.Rename(ctx, file.ID, "renamed.txt")
	if err != nil || e.ID != file.ID || e.Name != "renamed.txt" || e.Version != file.Version {
		t.Fatalf("rename: %+v, %v", e, err)
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		if _, ok := f.Content("/docs/renamed.txt"); !ok {
			t.Fatalf("%s did not rename: %v", f.Name(), f.Tree())
		}
	}
	if got := readAll(t, p, file.ID); got != "A" {
		t.Fatalf("read after rename = %q", got)
	}

	// A directory rename keeps every descendant's id.
	d, err := p.Rename(ctx, docs.ID, "papers")
	if err != nil || d.ID != docs.ID {
		t.Fatalf("dir rename: %+v, %v", d, err)
	}
	if got := strings.Join(b.Tree(), ","); got != "/papers/,/papers/deep/,/papers/deep/b.txt,/papers/renamed.txt" {
		t.Fatalf("b tree = %s", got)
	}
	got, err := p.Stat(ctx, "/papers/deep/b.txt")
	if err != nil || got.ID != bfile.ID {
		t.Fatalf("descendant id after dir rename: %+v, %v (want %s)", got, err, bfile.ID)
	}
	if readAll(t, p, bfile.ID) != "B" {
		t.Fatal("descendant unreadable after dir rename")
	}
	papers, _, err := p.List(ctx, docs.ID, "")
	if err != nil || strings.Join(names(papers), ",") != "deep,renamed.txt" {
		t.Fatalf("renamed dir listing = %v, %v", names(papers), err)
	}
	// Move into the renamed directory.
	a.Seed("/loose.txt", []byte("L"))
	root, _, _ = p.List(ctx, rootID, "")
	loose, _ := find(root, "loose.txt")
	mv, err := p.Move(ctx, loose.ID, docs.ID)
	if err != nil || mv.ID != loose.ID {
		t.Fatalf("move: %+v, %v", mv, err)
	}
	if _, ok := a.Content("/papers/loose.txt"); !ok {
		t.Fatalf("a after move: %v", a.Tree())
	}
}

func TestOverwriteMarksOtherReplicasStaleNotConflict(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("v1"))
	b.Seed("/f.txt", []byte("v1"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	e := upload(t, ctx, p, rootID, "f.txt", []byte("v2 longer"))
	// b still has v1, older. It is a stale replica of our own overwrite,
	// not someone else's edit: no conflict copy appears.
	entries, _, _ := p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "f.txt" {
		t.Fatalf("listing after overwrite = %s", got)
	}
	if got, _ := find(entries, "f.txt"); got.Version != e.Version {
		t.Fatalf("listing reverted the version: %+v vs %+v", got, e)
	}
	if got := readAll(t, p, e.ID); got != "v2 longer" {
		t.Fatalf("read = %q", got)
	}
	var stale int
	p.db.QueryRow(`SELECT COUNT(*) FROM replicas WHERE path = '/f.txt' AND state = 'stale'`).Scan(&stale)
	if stale != 1 {
		t.Fatalf("stale replicas = %d", stale)
	}
	// Someone edits b's copy in the vendor app: that is new content again,
	// and it competes on mtime like any out-of-band edit.
	b.Seed("/f.txt", []byte("edited on b"))
	b.SetMTime("/f.txt", time.Now().Add(time.Hour))
	entries, _, _ = p.List(ctx, rootID, "")
	if len(entries) != 2 {
		t.Fatalf("an out-of-band edit of a stale replica should surface: %v", names(entries))
	}
}

func TestUnreachableMemberGetsAPendingOp(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	entries, _, _ := p.List(ctx, rootID, "")
	f, _ := find(entries, "f.txt")
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := p.Rename(ctx, f.ID, "g.txt"); err != nil {
		t.Fatalf("rename with a member down: %v", err)
	}
	var ops int
	p.db.QueryRow(`SELECT COUNT(*) FROM pending_ops WHERE member = 'b' AND op = 'rename' AND state = 'pending'`).Scan(&ops)
	if ops != 1 {
		t.Fatalf("pending ops for b = %d", ops)
	}
	entries, _, _ = p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "g.txt" {
		t.Fatalf("listing with b down = %s", got)
	}
	// b returns still showing the old name; that is not a new file.
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	entries, _, _ = p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "g.txt" {
		t.Fatalf("listing after b returned = %s", got)
	}
	// Meanwhile b's copy still serves reads (same bytes, old name).
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if got := readAll(t, p, f.ID); got != "F" {
		t.Fatalf("read from the member with the pending rename = %q", got)
	}
}

func TestDeleteFansOutAndReleasesHold(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	dir := t.TempDir()
	p := newTestPool(t, dir, a, b)
	ctx := context.Background()
	blob := filepath.Join(dir, "blob")
	os.WriteFile(blob, []byte("held"), 0o600)
	link := func(dst string) error { return os.Link(blob, dst) }
	e := upload(t, provider_ctx(ctx, link), p, rootID, "h.txt", []byte("held"))
	holds, _ := filepath.Glob(filepath.Join(p.holdsDir(), "*"))
	if len(holds) != 1 {
		t.Fatalf("holds after upload = %v", holds)
	}
	if got, _ := os.ReadFile(holds[0]); string(got) != "held" {
		t.Fatalf("hold content = %q", got)
	}
	var queued int
	p.db.QueryRow(`SELECT COUNT(*) FROM repair_queue WHERE path = '/h.txt'`).Scan(&queued)
	if queued != 1 {
		t.Fatal("an under-replicated file was not queued for repair")
	}
	// Give b a copy too, then delete: both go, and the hold with them.
	b.Seed("/h.txt", []byte("held"))
	p.List(ctx, rootID, "")
	if err := p.Delete(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		if _, ok := f.Content("/h.txt"); ok {
			t.Fatalf("%s still has the file", f.Name())
		}
	}
	if holds, _ = filepath.Glob(filepath.Join(p.holdsDir(), "*")); len(holds) != 0 {
		t.Fatalf("holds after delete = %v", holds)
	}
	if _, err := p.Stat(ctx, e.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("stat after delete = %v", err)
	}
}

func TestHoldBudgetEvictsOldest(t *testing.T) {
	a := fakeprovider.New("a")
	dir := t.TempDir()
	opt := Options{Name: "home", StateDir: dir, Settings: config.Pool{Replicas: 2, MinReplicas: 1, HoldMaxBytes: 10}, Members: []Member{{Name: "a", Provider: a}}}
	p, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	for i, content := range []string{"aaaaaa", "bbbbbb", "cccccc"} {
		blob := filepath.Join(dir, "blob"+string(rune('0'+i)))
		os.WriteFile(blob, []byte(content), 0o600)
		upload(t, provider_ctx(ctx, func(dst string) error { return os.Link(blob, dst) }), p, rootID, "f"+string(rune('0'+i)), []byte(content))
	}
	holds, _ := filepath.Glob(filepath.Join(p.holdsDir(), "*"))
	if len(holds) != 1 {
		t.Fatalf("holds within a 10-byte budget = %d", len(holds))
	}
	if got, _ := os.ReadFile(holds[0]); string(got) != "cccccc" {
		t.Fatalf("the newest hold should survive, got %q", got)
	}
}

func provider_ctx(ctx context.Context, link provider.UploadBlobLinker) context.Context {
	return provider.WithUploadBlobLink(ctx, link)
}
