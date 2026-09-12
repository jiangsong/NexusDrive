package sftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"

	psftp "github.com/pkg/sftp"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// newTestProvider serves a temporary directory over a real SFTP session
// running on a pipe. The protocol is exercised for real; only the SSH
// transport is skipped.
func newTestProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()

	// net.Pipe is full duplex, so closing one end unblocks the reader on the
	// other; two io.Pipes would leave the client's receive loop parked.
	clientSide, serverSide := net.Pipe()
	srv, err := psftp.NewServer(serverSide)
	if err != nil {
		t.Fatalf("start sftp server: %v", err)
	}
	go srv.Serve()
	cli, err := psftp.NewClientPipe(clientSide, clientSide)
	if err != nil {
		t.Fatalf("start sftp client: %v", err)
	}
	p, err := New(Options{Name: "test", Root: dir, Client: cli, PartSize: 8})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	t.Cleanup(func() {
		cli.Close()
		serverSide.Close()
		srv.Close()
	})
	return p, dir
}

func write(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListAndStat(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	write(t, dir, "a.txt", []byte("hello"))
	write(t, dir, "sub/b.txt", []byte("world"))

	entries, next, err := p.List(ctx, RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("sftp has no pagination, got cursor %q", next)
	}
	byName := map[string]provider.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if len(byName) != 2 {
		t.Fatalf("listed %d entries, want 2: %v", len(byName), entries)
	}
	if got := byName["a.txt"]; got.Kind != provider.KindFile || got.Size != 5 || got.ID != "/a.txt" {
		t.Fatalf("a.txt = %+v", got)
	}
	if got := byName["sub"]; got.Kind != provider.KindDir {
		t.Fatalf("sub = %+v", got)
	}
	// Every entry must carry a change token, or two different contents would
	// share a block-cache key.
	for _, e := range entries {
		if e.Version == "" {
			t.Fatalf("entry %s has no version", e.Name)
		}
	}
	st, err := p.Stat(ctx, "/sub/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 5 || st.ParentID != "/sub" {
		t.Fatalf("stat = %+v", st)
	}
}

func TestVersionTracksContent(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	write(t, dir, "v.txt", []byte("one"))
	first, err := p.Stat(ctx, "/v.txt")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "v.txt", []byte("a longer second version"))
	second, err := p.Stat(ctx, "/v.txt")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version == second.Version {
		t.Fatalf("version did not change with content: %q", first.Version)
	}
}

func TestStatMissing(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.Stat(context.Background(), "/nope.txt")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReadRange(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	body := []byte("0123456789abcdefghij")
	write(t, dir, "r.bin", body)

	cases := []struct{ off, n int64 }{{0, 5}, {5, 5}, {10, 10}, {3, 0}, {0, 0}}
	for _, c := range cases {
		rc, err := p.ReadRange(ctx, "/r.bin", "", c.off, c.n)
		if err != nil {
			t.Fatalf("range %d+%d: %v", c.off, c.n, err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := body[c.off:]
		if c.n > 0 {
			want = body[c.off : c.off+c.n]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("range %d+%d = %q, want %q", c.off, c.n, got, want)
		}
	}
}

// TestUploadOutOfOrderParts is the property a resumed upload depends on: parts
// carry their own offset, so the order they arrive in cannot matter.
func TestUploadOutOfOrderParts(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	body := []byte("AAAAAAAABBBBBBBBCCCC") // 8 + 8 + 4 with PartSize 8
	s, err := p.BeginUpload(ctx, RootID, "up.bin", int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{2, 0, 1} {
		off := idx * 8
		end := off + 8
		if end > len(body) {
			end = len(body)
		}
		chunk := body[off:end]
		if _, err := p.UploadPart(ctx, s, idx, bytes.NewReader(chunk), int64(len(chunk))); err != nil {
			t.Fatalf("part %d: %v", idx, err)
		}
	}
	// While the upload is in flight the name must not be visible.
	entries, _, err := p.List(ctx, RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, tempPrefix) {
			t.Fatalf("staging file %s leaked into a listing", e.Name)
		}
	}
	e, err := p.CompleteUpload(ctx, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "up.bin" || e.Size != int64(len(body)) {
		t.Fatalf("entry = %+v", e)
	}
	got, err := os.ReadFile(filepath.Join(dir, "up.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("uploaded content = %q", got)
	}
}

// TestShortPartIsRejected guards the failure that is worst for a filesystem: a
// truncated write that reports success and is then completed into place.
func TestShortPartIsRejected(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, RootID, "short.bin", 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The reader ends early while the caller claims 8 bytes.
	if _, err := p.UploadPart(ctx, s, 0, bytes.NewReader([]byte("abc")), 8); err == nil {
		t.Fatal("a part that wrote fewer bytes than promised must fail")
	}
}

func TestCompleteRejectsWrongSize(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, RootID, "trunc.bin", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UploadPart(ctx, s, 0, bytes.NewReader([]byte("only a few")), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CompleteUpload(ctx, s, nil); err == nil {
		t.Fatal("completing a staged file smaller than the declared size must fail")
	}
}

func TestUploadOverwritesExisting(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	write(t, dir, "over.txt", []byte("old content"))
	body := []byte("new")
	s, err := p.BeginUpload(ctx, RootID, "over.txt", int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UploadPart(ctx, s, 0, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CompleteUpload(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "over.txt"))
	if !bytes.Equal(got, body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
}

func TestAbortUploadRemovesStaging(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, RootID, "gone.bin", 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AbortUpload(ctx, s); err != nil {
		t.Fatal(err)
	}
	names, _ := os.ReadDir(dir)
	for _, n := range names {
		if strings.HasPrefix(n.Name(), tempPrefix) {
			t.Fatalf("staging file %s survived the abort", n.Name())
		}
	}
	// Aborting twice is not an error, because recovery replays it.
	if err := p.AbortUpload(ctx, s); err != nil {
		t.Fatalf("second abort: %v", err)
	}
}

func TestMkdirRenameMoveDelete(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()

	if _, err := p.Mkdir(ctx, RootID, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Mkdir(ctx, RootID, "d1"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("creating an existing directory should report ErrExists, got %v", err)
	}
	if _, err := p.Mkdir(ctx, "/d1", "d2"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "d1/f.txt", []byte("x"))

	e, err := p.Rename(ctx, "/d1/f.txt", "g.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "/d1/g.txt" {
		t.Fatalf("renamed id = %s", e.ID)
	}
	e, err = p.Move(ctx, "/d1/g.txt", "/d1/d2")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "/d1/d2/g.txt" {
		t.Fatalf("moved id = %s", e.ID)
	}
	// Deleting a directory takes its contents with it; SFTP itself only
	// removes empty directories.
	if err := p.Delete(ctx, "/d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d1")); !os.IsNotExist(err) {
		t.Fatalf("directory survived delete: %v", err)
	}
}

func TestRenameReplacesDestination(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	write(t, dir, "src.txt", []byte("source"))
	write(t, dir, "dst.txt", []byte("destination"))
	if _, err := p.Rename(ctx, "/src.txt", "dst.txt"); err != nil {
		t.Fatalf("rename over an existing name must succeed: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "dst.txt"))
	if string(got) != "source" {
		t.Fatalf("dst.txt = %q", got)
	}
}

func TestDownloadURLUnsupported(t *testing.T) {
	p, _ := newTestProvider(t)
	if _, err := p.DownloadURL(context.Background(), "/x"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestCapabilitiesAreHonest(t *testing.T) {
	p, _ := newTestProvider(t)
	c := p.Capabilities()
	if len(c.HashTypes) != 0 || len(c.RapidUpload) != 0 {
		t.Fatal("sftp exposes no content hash; claiming one would break rapid upload")
	}
	if c.Delta {
		t.Fatal("sftp has no change feed; claiming delta would stop TTL refresh")
	}
	if c.ServerCopy {
		t.Fatal("pkg/sftp exposes no server-side copy")
	}
	if !c.PathIDs {
		t.Fatal("an sftp id is a path, so renaming a directory changes every id beneath it; the VFS needs to be told")
	}
	if !c.RangeRead {
		t.Fatal("sftp supports ranged reads")
	}
	if c.LinkShareable {
		t.Fatal("an sftp path needs this process's credentials")
	}
}

func TestRootResolution(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	abs, err := p.abs(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if abs != dir {
		t.Fatalf("root resolved to %q, want %q", abs, dir)
	}
	abs, err = p.abs(ctx, "/a/../b/c")
	if err != nil {
		t.Fatal(err)
	}
	if abs != dir+"/b/c" {
		t.Fatalf("path resolved to %q", abs)
	}
	// A path may not escape the configured root.
	abs, err = p.abs(ctx, "/../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(abs, dir) {
		t.Fatalf("path escaped the root: %q", abs)
	}
}

// TestHostKeyAliasVerifiesUnderTheRealName: through a local proxy the dialled
// address is 127.0.0.1:port, which known_hosts has no entry for. The alias
// makes verification use the real host's entry instead of forcing the user
// to disable verification.
func TestHostKeyAliasVerifiesUnderTheRealName(t *testing.T) {
	var seen string
	cb := aliased(func(hostname string, _ net.Addr, _ ssh.PublicKey) error {
		seen = hostname
		return nil
	}, "nas.real:22")
	if err := cb("127.0.0.1:2222", &net.TCPAddr{}, nil); err != nil {
		t.Fatal(err)
	}
	if seen != "nas.real:22" {
		t.Fatalf("verified under %q, want the alias", seen)
	}
}

func TestPutFileStoresAtomicallyAndReplaces(t *testing.T) {
	p, dir := newTestProvider(t)
	ctx := context.Background()
	write(t, dir, "one.txt", []byte("old"))
	body := []byte("a small file in one request")
	e, err := p.PutFile(ctx, RootID, "one.txt", bytes.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "one.txt" || e.Size != int64(len(body)) {
		t.Fatalf("entry = %+v", e)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "one.txt"))
	if !bytes.Equal(got, body) {
		t.Fatalf("content = %q", got)
	}
	// No staging file is left behind.
	names, _ := os.ReadDir(dir)
	for _, n := range names {
		if strings.HasPrefix(n.Name(), tempPrefix) {
			t.Fatalf("staging file %s leaked", n.Name())
		}
	}
	// A short body is a failure, not a truncated file.
	if _, err := p.PutFile(ctx, RootID, "two.txt", bytes.NewReader([]byte("abc")), 10, nil); err == nil {
		t.Fatal("a body shorter than the declared size must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "two.txt")); !os.IsNotExist(err) {
		t.Fatal("a failed put must not leave a file at the target")
	}
	if p.Capabilities().SinglePutMax <= 0 {
		t.Fatal("sftp should advertise single-request uploads")
	}
}

func TestAFullFilesystemIsClassifiedAsQuota(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"errno", &os.PathError{Op: "write", Path: "/srv/one.txt", Err: syscall.ENOSPC}},
		{"status code", &psftp.StatusError{Code: 14}}, // SSH_FX_NO_SPACE_ON_FILESYSTEM
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.err)
			if !errors.Is(got, provider.ErrQuotaExceeded) {
				t.Fatalf("%v mapped to %v, want ErrQuotaExceeded", tc.err, got)
			}
			if c := retry.Classify(got); c != retry.ClassQuota {
				t.Fatalf("Classify = %v, want quota: a full server must not be retried", c)
			}
			// Re-mapping an already mapped error must not downgrade it.
			if c := retry.Classify(mapErr(got)); c != retry.ClassQuota {
				t.Fatalf("re-mapped Classify = %v, want quota", c)
			}
		})
	}
	// A generic failure stays a conflict, not a full disk.
	if errors.Is(mapErr(&psftp.StatusError{Code: 4}), provider.ErrQuotaExceeded) {
		t.Fatal("SSH_FX_FAILURE must not be read as a full filesystem")
	}
}
