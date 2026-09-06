package pool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

func changesOf(t *testing.T, p *Pool, cursor string) ([]provider.Change, string) {
	t.Helper()
	evs, next, err := p.Changes(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	return evs, next
}

func TestChangesFoldMemberEventsIntoPoolEntries(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/docs/f.txt", []byte("F"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	root, _, _ := p.List(ctx, rootID, "")
	docs, _ := find(root, "docs")
	inner, _, _ := p.List(ctx, docs.ID, "")
	f, _ := find(inner, "f.txt")
	if !p.Capabilities().Delta {
		t.Fatal("a pool of members with feeds has a feed")
	}
	// Baseline: the history so far maps to what we already show.
	evs, cursor := changesOf(t, p, "")
	for _, e := range evs {
		if e.Op != provider.ChangeUpsert || !strings.HasPrefix(e.ID, idPrefix) {
			t.Fatalf("baseline event %+v", e)
		}
	}
	// Our own upload through the pool echoes back with the same id and
	// version the pool reported: a no-op for anyone holding it.
	e := upload(t, ctx, p, docs.ID, "f.txt", []byte("F2"))
	evs, cursor = changesOf(t, p, cursor)
	if len(evs) != 1 || evs[0].ID != e.ID || evs[0].Entry == nil || evs[0].Entry.Version != e.Version || evs[0].ParentID != docs.ID {
		t.Fatalf("echo of our write = %+v (want id %s version %s)", evs, e.ID, e.Version)
	}
	// An edit in the vendor's app on a member surfaces with a new version.
	a.Seed("/docs/f.txt", []byte("edited in the app"))
	evs, cursor = changesOf(t, p, cursor)
	if len(evs) != 1 || evs[0].ID != f.ID || evs[0].Entry.Version == e.Version {
		t.Fatalf("vendor edit = %+v", evs)
	}
	if got := readAll(t, p, f.ID); got != "edited in the app" {
		t.Fatalf("read after edit = %q", got)
	}
	// A file created in the app in a known directory appears; a delete
	// of the only copy is a delete.
	b.Seed("/docs/new-on-b.txt", []byte("new")) // creates /docs on b too
	evs, cursor = changesOf(t, p, cursor)
	var newFile *provider.Change
	for i := range evs {
		if evs[i].Entry != nil && evs[i].Entry.Name == "new-on-b.txt" {
			newFile = &evs[i]
		}
	}
	if newFile == nil || newFile.Op != provider.ChangeUpsert || newFile.ParentID != docs.ID {
		t.Fatalf("new file inside a directory the member just created = %+v", evs)
	}
	id, _ := a.IDOf("/docs/f.txt")
	if err := a.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	evs, _ = changesOf(t, p, cursor)
	if len(evs) != 1 || evs[0].Op != provider.ChangeDelete || evs[0].ID != f.ID {
		t.Fatalf("delete = %+v", evs)
	}
}

func TestMemberCursorResetOnlyRescansThatMember(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1, ScrubSample: 1}, a, b)
	ctx := context.Background()
	p.List(ctx, rootID, "")
	_, cursor := changesOf(t, p, "")
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.CursorReset = true })
	evs, next, err := p.Changes(ctx, cursor)
	if err != nil {
		t.Fatalf("a member's reset must not be the pool's: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("events on reset = %+v", evs)
	}
	if !p.byName["a"].needsScrub || p.byName["b"].needsScrub {
		t.Fatal("only the reset member is flagged for a scrub")
	}
	if parseCursor(next)["a"] == "" {
		t.Fatalf("the replacement cursor was not adopted: %s", next)
	}
	b.Seed("/g.txt", []byte("G"))
	evs, _ = changesOf(t, p, next)
	if len(evs) != 1 || evs[0].Entry == nil || evs[0].Entry.Name != "g.txt" {
		t.Fatalf("after the reset the feed goes on: %+v", evs)
	}
}

func TestTwoIndexesOverSameMembersConverge(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	settings := config.Pool{Replicas: 2, MinReplicas: 1, TrimGrace: time.Hour}
	p1 := newTestPoolWith(t, t.TempDir(), settings, a, b, c)
	p2 := newTestPoolWith(t, t.TempDir(), settings, a, b, c)
	ctx := context.Background()
	// Each machine writes its own files, and repairs; they never talk.
	for i := 0; i < 4; i++ {
		upload(t, ctx, p1, rootID, "m1-"+string(rune('0'+i))+".txt", []byte("from machine 1"))
		upload(t, ctx, p2, rootID, "m2-"+string(rune('0'+i))+".txt", []byte("from machine 2"))
		p1.RepairOnce(ctx)
		p2.RepairOnce(ctx)
	}
	// Both see the same namespace after listing.
	e1, _, _ := p1.List(ctx, rootID, "")
	e2, _, _ := p2.List(ctx, rootID, "")
	if strings.Join(names(e1), ",") != strings.Join(names(e2), ",") || len(e1) != 8 {
		t.Fatalf("namespaces differ: %v vs %v", names(e1), names(e2))
	}
	for i := range e1 {
		if e1[i].Version != e2[i].Version {
			t.Fatalf("%s: versions differ %s vs %s", e1[i].Name, e1[i].Version, e2[i].Version)
		}
	}
	// Both run scan+repair on everything; nothing gets a third copy from
	// the other machine's ignorance, and nothing is a conflict.
	for i := 0; i < 3; i++ {
		p1.ScanOnce(ctx)
		p1.RepairOnce(ctx)
		p2.ScanOnce(ctx)
		p2.RepairOnce(ctx)
	}
	for _, e := range e1 {
		holders := 0
		for _, f := range []*fakeprovider.Fake{a, b, c} {
			if _, ok := f.Content("/" + e.Name); ok {
				holders++
			}
		}
		if holders != 2 {
			t.Fatalf("%s is on %d members after both machines repaired", e.Name, holders)
		}
		if strings.Contains(e.Name, "conflict") {
			t.Fatalf("a conflict copy appeared: %s", e.Name)
		}
	}
	// Machine 1 renames, machine 2 sees it through its next listing.
	f, _ := find(e1, "m2-0.txt")
	if _, err := p1.Rename(ctx, f.ID, "renamed-by-1.txt"); err != nil {
		t.Fatal(err)
	}
	e2, _, _ = p2.List(ctx, rootID, "")
	if _, ok := find(e2, "renamed-by-1.txt"); !ok {
		t.Fatalf("machine 2 does not see the rename: %v", names(e2))
	}
	if _, ok := find(e2, "m2-0.txt"); ok {
		t.Fatal("machine 2 still shows the old name")
	}
	divs1, _ := p1.Divergences(ctx, 10)
	divs2, _ := p2.Divergences(ctx, 10)
	if len(divs1)+len(divs2) > 0 {
		t.Fatalf("divergences: %+v %+v", divs1, divs2)
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func TestJoinDiscoversSettingsFromMarker(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	settings := config.Pool{Replicas: 3, MinReplicas: 2, GCGrace: time.Hour, TrimGrace: time.Hour}
	p1 := newTestPoolWith(t, t.TempDir(), settings, a, b)
	ctx := context.Background()
	if err := p1.WriteMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	// The marker is invisible in the namespace.
	entries, _, _ := p1.List(ctx, rootID, "")
	if len(entries) != 0 {
		t.Fatalf("marker listed: %v", names(entries))
	}
	m, err := ReadMarker(ctx, b, "/")
	if err != nil {
		t.Fatal(err)
	}
	if m.PoolID != p1.ID(ctx) || m.Settings.Replicas != 3 || m.Settings.MinReplicas != 2 || strings.Join(m.Members, ",") != "a,b" {
		t.Fatalf("marker = %+v", m)
	}
	// A second machine joins from b alone: it adopts the id.
	p2 := newTestPoolWith(t, t.TempDir(), settings, a, b)
	if p2.ID(ctx) == p1.ID(ctx) {
		t.Fatal("two indexes minted the same id")
	}
	if err := p2.AdoptMarker(ctx, m); err != nil {
		t.Fatal(err)
	}
	if p2.ID(ctx) != p1.ID(ctx) {
		t.Fatal("adopt did not take")
	}
	// Writing markers from p2 changes nothing (same id, same epoch); with
	// different settings it leaves the newer one and says so.
	if err := p2.WriteMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	p3 := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	p3.AdoptMarker(ctx, m)
	// p3's settings epoch is newer (minted now) — it would overwrite. Make
	// the member's marker newer instead to exercise the notice.
	newer := m
	newer.SettingsEpoch = time.Now().Add(time.Hour).UnixNano()
	newer.Settings.Replicas = 5
	body, _ := jsonMarshal(newer)
	rootA, _ := resolveRoot(ctx, a, "/")
	sess, _ := a.BeginUpload(ctx, rootA, markerName, int64(len(body)), nil)
	pt, _ := a.UploadPart(ctx, sess, 0, strings.NewReader(string(body)), int64(len(body)))
	a.CompleteUpload(ctx, sess, []provider.PartToken{pt})
	if err := p3.WriteMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	notices := p3.Notices()
	if len(notices) == 0 || !strings.Contains(notices[0], "newer pool settings") {
		t.Fatalf("notices = %v", notices)
	}
	got, _ := ReadMarker(ctx, a, "/")
	if got.Settings.Replicas != 5 {
		t.Fatal("the newer marker was overwritten")
	}
}
