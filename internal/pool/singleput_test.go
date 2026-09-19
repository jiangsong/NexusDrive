package pool

import (
	"bytes"
	"context"
	"io"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// singlePut gives the fake a one-request upload, counted apart from the
// session protocol, with a size ceiling the way a real driver declares one.
type singlePut struct {
	*fakeprovider.Fake
	puts int
	max  int64
}

func (s *singlePut) Capabilities() provider.Caps {
	c := s.Fake.Capabilities()
	c.SinglePutMax = s.max
	return c
}

func (s *singlePut) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h provider.Hashes) (provider.Entry, error) {
	s.puts++
	sess, err := s.Fake.BeginUpload(ctx, parentID, name, size, nil)
	if err != nil {
		return provider.Entry{}, err
	}
	pt, err := s.Fake.UploadPart(ctx, sess, 0, r, size)
	if err != nil {
		return provider.Entry{}, err
	}
	return s.Fake.CompleteUpload(ctx, sess, []provider.PartToken{pt})
}

// TestPoolPutsSmallFilesInOneRequest: on two Drive accounts every 600-byte
// object of a repository cost the pool begin + part + complete — three round
// trips, 3.4 s — because the pool did not offer PutFile, while the member
// under it would have taken the file in one request (2.2 s). The pool now
// declares the ceiling every member honours and puts the file in one call on
// the member placement chose; the index and the hold come out the same as
// after a session, so repair still copies from the local bytes.
func TestPoolPutsSmallFilesInOneRequest(t *testing.T) {
	a := &singlePut{Fake: fakeprovider.New("a"), max: 64}
	b := &singlePut{Fake: fakeprovider.New("b"), max: 32}
	p := newPoolOf(t, config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()

	if got := p.Capabilities().SinglePutMax; got != 32 {
		t.Fatalf("pool SinglePutMax = %d, want the smallest member ceiling 32", got)
	}
	sp, ok := provider.Provider(p).(provider.SinglePutter)
	if !ok {
		t.Fatal("the pool does not offer PutFile")
	}
	content := []byte("tiny")
	e, err := sp.PutFile(ctx, rootID, "small.txt", bytes.NewReader(content), int64(len(content)), provider.Hashes{provider.HashSHA1: sha1hex(content)})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if e.Kind != provider.KindFile || e.Size != int64(len(content)) || e.ID == "" {
		t.Fatalf("entry = %+v", e)
	}
	if a.puts+b.puts != 1 {
		t.Fatalf("puts a=%d b=%d, want exactly one member asked once", a.puts, b.puts)
	}
	// The member's own PutFile runs the fake's session once; the pool must
	// not have run a session of its own beside it.
	if a.Calls("BeginUpload")+b.Calls("BeginUpload") != 1 {
		t.Fatalf("sessions begun a=%d b=%d, want the one inside the put", a.Calls("BeginUpload"), b.Calls("BeginUpload"))
	}
	// The index knows the file and its one live replica, and lists it.
	entries, _, err := p.List(ctx, rootID, "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := find(entries, "small.txt")
	if !ok || got.ID != e.ID {
		t.Fatalf("listed %+v (ok=%v), put %+v", got, ok, e)
	}
	var live int
	if err := p.db.QueryRow(`SELECT count(*) FROM replicas WHERE path = '/small.txt' AND state = 'live'`).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live replicas = %d (%v)", live, err)
	}
	// Repair brings the second copy, as after a session upload.
	if _, err := p.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.Tree()) != 1 || len(b.Tree()) != 1 {
		t.Fatalf("after repair a=%v b=%v", a.Tree(), b.Tree())
	}

	// A pool with a member that has no one-request upload offers none: the
	// uploader would otherwise call PutFile for a size that member refuses.
	mixed := newPoolOf(t, config.Pool{Replicas: 1, MinReplicas: 1}, &singlePut{Fake: fakeprovider.New("d"), max: 64}, fakeprovider.New("e"))
	if got := mixed.Capabilities().SinglePutMax; got != 0 {
		t.Fatalf("a pool with a member that has no PutFile declared SinglePutMax %d", got)
	}
}

// TestPoolUploadParallelIsTheMembersTogether: each upload lands on one
// member, so the pool carries as many at once as its members can between
// them — not as few as its most cautious member allows.
func TestPoolUploadParallelIsTheMembersTogether(t *testing.T) {
	one := &singlePut{Fake: fakeprovider.New("one"), max: 64}
	two := &singlePut{Fake: fakeprovider.New("two"), max: 64}
	p := newPoolOf(t, config.Pool{Replicas: 2, MinReplicas: 1}, one, two)
	a, b := one.Fake.Capabilities().UploadParallel, two.Fake.Capabilities().UploadParallel
	if got := p.Capabilities().UploadParallel; got != max(a, 1)+max(b, 1) {
		t.Fatalf("pool UploadParallel = %d, members %d and %d", got, a, b)
	}
}
