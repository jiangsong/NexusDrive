package fakeprovider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"cloudfs/internal/provider"
)

func TestSeedListStat(t *testing.T) {
	f := New("t")
	f.Seed("docs/a.txt", []byte("hello"))
	f.Seed("docs/b.txt", []byte("world"))
	f.Seed("docs/c.txt", []byte("!"))

	ctx := context.Background()
	root, _, err := f.List(ctx, RootID, "")
	if err != nil || len(root) != 1 || root[0].Name != "docs" {
		t.Fatalf("root list = %v, %v", root, err)
	}
	var all []provider.Entry
	cursor := ""
	for {
		page, next, err := f.List(ctx, root[0].ID, cursor)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(all) != 3 || all[0].Name != "a.txt" || all[2].Name != "c.txt" {
		t.Fatalf("paged list = %v", all)
	}
	if f.Calls("List") != 3 {
		t.Fatalf("expected 3 List calls (1 root + 2 pages), got %d", f.Calls("List"))
	}
	e, err := f.Stat(ctx, all[0].ID)
	if err != nil || e.Size != 5 || e.Hashes[provider.HashSHA1] == "" {
		t.Fatalf("stat = %+v, %v", e, err)
	}
}

func TestReadRange(t *testing.T) {
	f := New("t")
	e := f.Seed("x.bin", []byte("0123456789"))
	rc, err := f.ReadRange(context.Background(), e.ID, e.Version, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "3456" {
		t.Fatalf("range = %q", b)
	}
	if _, err := f.ReadRange(context.Background(), e.ID, "stale", 0, 1); !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("stale version should conflict, got %v", err)
	}
}

func TestUploadPartsAndRapid(t *testing.T) {
	f := New("t")
	ctx := context.Background()
	data := bytes.Repeat([]byte("ab"), 5<<20) // 10 MiB, 3 parts of 4 MiB
	s, err := f.BeginUpload(ctx, RootID, "big.bin", int64(len(data)), nil)
	if err != nil || s.RapidDone {
		t.Fatalf("begin = %+v, %v", s, err)
	}
	var parts []provider.PartToken
	for i := 0; int64(i)*s.PartSize < int64(len(data)); i++ {
		off := int64(i) * s.PartSize
		end := off + s.PartSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		p, err := f.UploadPart(ctx, s, i, bytes.NewReader(data[off:end]), end-off)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
	}
	e, err := f.CompleteUpload(ctx, s, parts)
	if err != nil || e.Size != int64(len(data)) {
		t.Fatalf("complete = %+v, %v", e, err)
	}
	// Complete again with the same session: idempotent.
	e2, err := f.CompleteUpload(ctx, s, parts)
	if err != nil || e2.ID != e.ID {
		t.Fatalf("idempotent complete = %+v, %v", e2, err)
	}
	// Same content elsewhere: rapid upload by hash.
	s2, err := f.BeginUpload(ctx, RootID, "copy.bin", int64(len(data)), e.Hashes)
	if err != nil || !s2.RapidDone || s2.Entry == nil || s2.Entry.Name != "copy.bin" {
		t.Fatalf("rapid = %+v, %v", s2, err)
	}
	if f.Calls("UploadPart") != 3 {
		t.Fatalf("rapid upload should not add parts, got %d", f.Calls("UploadPart"))
	}
}

func TestChangesAndDelete(t *testing.T) {
	f := New("t")
	ctx := context.Background()
	cur := f.Cursor()
	e := f.Seed("d/f.txt", []byte("x"))
	if err := f.Delete(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	evs, next, err := f.Changes(ctx, cur)
	if err != nil || next == cur {
		t.Fatalf("changes err=%v next=%q", err, next)
	}
	// mkdir d, create f.txt, delete f.txt
	if len(evs) != 3 || evs[2].Op != provider.ChangeDelete || evs[2].ID != e.ID {
		t.Fatalf("events = %+v", evs)
	}
	if _, err := f.Stat(ctx, e.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("deleted file should be gone, got %v", err)
	}
}

func TestFaultInjection(t *testing.T) {
	f := New("t")
	ctx := context.Background()
	f.SetFaults(func(ft *Faults) { ft.RateLimitEvery = 3 })
	var limited int
	for i := 0; i < 9; i++ {
		if _, err := f.Stat(ctx, RootID); errors.Is(err, provider.ErrRateLimited) {
			var ra *provider.RetryAfterError
			if !errors.As(err, &ra) || ra.RetryAfter <= 0 {
				t.Fatalf("rate limit should carry Retry-After, got %v", err)
			}
			limited++
		}
	}
	if limited != 3 {
		t.Fatalf("expected 3 rate limited calls, got %d", limited)
	}
	f.Faults = Faults{FailNext: 2}
	for i := 0; i < 2; i++ {
		if _, err := f.Stat(ctx, RootID); !errors.Is(err, provider.ErrTransient) {
			t.Fatalf("call %d should be transient failure, got %v", i, err)
		}
	}
	if _, err := f.Stat(ctx, RootID); err != nil {
		t.Fatalf("third call should succeed, got %v", err)
	}
}

func TestRegistry(t *testing.T) {
	p, err := provider.New("fake", "r1", nil)
	if err != nil || p.Name() != "r1" {
		t.Fatalf("registry New = %v, %v", p, err)
	}
	if _, err := provider.New("nope", "x", nil); err == nil {
		t.Fatal("unknown type should error")
	}
}
