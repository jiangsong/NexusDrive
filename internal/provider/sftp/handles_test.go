package sftp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	psftp "github.com/pkg/sftp"
)

// SFTP packet types on the wire (draft-ietf-secsh-filexfer-02).
const (
	fxpOpen   = 3
	fxpClose  = 4
	fxpRead   = 5
	fxpStat   = 17
	fxpRemove = 13
)

// packetCounter wraps the client side of the pipe and counts the request
// packets the client sends, by type. That is the ground truth a handle cache
// is judged by: how many OPENs and CLOSEs a series of reads really cost.
type packetCounter struct {
	net.Conn
	mu     sync.Mutex
	buf    []byte
	counts map[byte]int
}

func (c *packetCounter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf = append(c.buf, p...)
	for len(c.buf) >= 5 {
		n := binary.BigEndian.Uint32(c.buf[:4])
		if len(c.buf) < int(4+n) {
			break
		}
		c.counts[c.buf[4]]++
		c.buf = c.buf[4+n:]
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *packetCounter) count(t byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[t]
}

// newCountingProvider is newTestProvider with the request packets counted
// and a configurable client packet size.
func newCountingProvider(t *testing.T, clientOpts ...psftp.ClientOption) (*Provider, string, *packetCounter) {
	t.Helper()
	dir := t.TempDir()
	clientSide, serverSide := net.Pipe()
	srv, err := psftp.NewServer(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	pc := &packetCounter{Conn: clientSide, counts: map[byte]int{}}
	cli, err := psftp.NewClientPipe(pc, pc, clientOpts...)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{Name: "test", Root: dir, Client: cli, PartSize: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		cli.Close()
		serverSide.Close()
		srv.Close()
	})
	return p, dir, pc
}

func readAll(t *testing.T, p *Provider, id, version string, off, n int64) []byte {
	t.Helper()
	rc, err := p.ReadRange(context.Background(), id, version, off, n)
	if err != nil {
		t.Fatalf("ReadRange(%s, %d, %d): %v", id, off, n, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRandomReadsShareOneHandle: a hundred random 4 KiB reads of one file
// must cost one OPEN and a hundred READs — not an OPEN, a READ and a CLOSE
// each, which is four round trips per 4 KiB and what held a cold random
// read to 200 IOPS on a LAN.
func TestRandomReadsShareOneHandle(t *testing.T) {
	p, dir, pc := newCountingProvider(t)
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(data)
	write(t, dir, "big.bin", data)
	opens0, reads0 := pc.count(fxpOpen), pc.count(fxpRead)

	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 100; i++ {
		off := rng.Int63n(int64(len(data) - 4096))
		if got := readAll(t, p, "/big.bin", "v1", off, 4096); !bytes.Equal(got, data[off:off+4096]) {
			t.Fatalf("read %d: wrong bytes", i)
		}
	}
	if opens := pc.count(fxpOpen) - opens0; opens != 1 {
		t.Fatalf("100 reads of one file sent %d OPENs, want 1", opens)
	}
	if closes := pc.count(fxpClose); closes != 0 {
		t.Fatalf("the handle was closed %d times between reads", closes)
	}
	if reads := pc.count(fxpRead) - reads0; reads != 100 {
		t.Fatalf("100 reads sent %d READ packets, want 100", reads)
	}
}

// TestVersionChangeReopensTheHandle: the version is part of the key, so a
// file that changed behind our back is not served from a handle pinned to
// its old inode.
func TestVersionChangeReopensTheHandle(t *testing.T) {
	p, dir, pc := newCountingProvider(t)
	write(t, dir, "f.txt", []byte("version one"))
	readAll(t, p, "/f.txt", "v1", 0, 5)
	readAll(t, p, "/f.txt", "v1", 5, 5)
	if opens := pc.count(fxpOpen); opens != 1 {
		t.Fatalf("same version: %d OPENs, want 1", opens)
	}
	readAll(t, p, "/f.txt", "v2", 0, 5)
	if opens := pc.count(fxpOpen); opens != 2 {
		t.Fatalf("new version: %d OPENs, want 2", opens)
	}
}

// TestMutationsEvictCachedHandles: our own upload renames over the path, a
// delete removes it, a rename moves it. A cached handle would keep reading
// the old inode after any of them — and some servers refuse the operation
// while the file is open — so each closes the handles first.
func TestMutationsEvictCachedHandles(t *testing.T) {
	p, dir, pc := newCountingProvider(t)
	ctx := context.Background()
	write(t, dir, "f.txt", []byte("old content"))
	if got := readAll(t, p, "/f.txt", "v1", 0, 3); string(got) != "old" {
		t.Fatalf("got %q", got)
	}
	closes := pc.count(fxpClose)

	if _, err := p.PutFile(ctx, "/", "f.txt", bytes.NewReader([]byte("new content")), 11, nil); err != nil {
		t.Fatal(err)
	}
	if pc.count(fxpClose) <= closes {
		t.Fatal("PutFile over a path with a cached handle did not close it first")
	}
	// The read after the replacement must see the new bytes even with the
	// same version string, because the eviction, not the key, protects it.
	if got := readAll(t, p, "/f.txt", "v1", 0, 3); string(got) != "new" {
		t.Fatalf("after PutFile got %q, want the new content", got)
	}

	if _, err := p.Rename(ctx, "/f.txt", "g.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, p, "/g.txt", "v1", 0, 3); string(got) != "new" {
		t.Fatalf("after rename got %q", got)
	}
	opens := pc.count(fxpOpen)
	if err := p.Delete(ctx, "/g.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "g.txt")); !os.IsNotExist(err) {
		t.Fatal("delete did not remove the file")
	}
	if _, err := p.ReadRange(ctx, "/g.txt", "v1", 0, 3); err == nil {
		t.Fatal("reading a deleted file through a cached handle succeeded")
	}
	if pc.count(fxpOpen) == opens {
		t.Fatal("the deleted file was served from its cached handle")
	}
}

// TestDeleteOfAFileIsOneRoundTrip: unlink throughput is bounded by round
// trips, so a file is removed without a stat first.
func TestDeleteOfAFileIsOneRoundTrip(t *testing.T) {
	p, dir, pc := newCountingProvider(t)
	write(t, dir, "f.txt", []byte("x"))
	stats, removes := pc.count(fxpStat), pc.count(fxpRemove)
	if err := p.Delete(context.Background(), "/f.txt"); err != nil {
		t.Fatal(err)
	}
	if got := pc.count(fxpStat) - stats; got != 0 {
		t.Fatalf("deleting a file sent %d STATs, want 0", got)
	}
	if got := pc.count(fxpRemove) - removes; got != 1 {
		t.Fatalf("deleting a file sent %d REMOVEs, want 1", got)
	}
	// A directory still works: the failed remove is followed by the walk.
	write(t, dir, "d/inner.txt", []byte("y"))
	if err := p.Delete(context.Background(), "/d"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); !os.IsNotExist(err) {
		t.Fatal("directory delete did not remove the tree")
	}
}

// TestReadRangeFollowsUpAShortAnswer: with a 255 KiB client packet against a
// server that caps requests at 32 KiB (pkg/sftp's own does), pkg/sftp
// reports the short chunk as EOF. The provider must ask again for the rest
// rather than hand back a truncated range that the cache would keep.
func TestReadRangeFollowsUpAShortAnswer(t *testing.T) {
	p, dir, pc := newCountingProvider(t, psftp.MaxPacketUnchecked(255<<10))
	data := make([]byte, 600<<10)
	rand.New(rand.NewSource(3)).Read(data)
	write(t, dir, "big.bin", data)
	got := readAll(t, p, "/big.bin", "v1", 100<<10, 200<<10)
	t.Logf("200 KiB range against a 32 KiB server took %d READ packets", pc.count(fxpRead))
	if !bytes.Equal(got, data[100<<10:300<<10]) {
		t.Fatalf("read returned %d bytes, want %d, or wrong content", len(got), 200<<10)
	}
	// And a range that really ends at the file still ends there.
	tail := readAll(t, p, "/big.bin", "v1", 590<<10, 64<<10)
	if !bytes.Equal(tail, data[590<<10:]) {
		t.Fatalf("tail read returned %d bytes, want %d", len(tail), 10<<10)
	}
}
