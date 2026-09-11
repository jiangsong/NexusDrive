//go:build !windows

package vfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// sourceFile writes data to a temporary file and returns it opened for
// reading, so a test can hand its fd to WriteFromFD the way fusefs would
// hand it the fd of a hydrated cache lease.
func sourceFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestWriteFromFDMatchesWrite is the core equivalence check: copying bytes
// through WriteFromFD must leave staging, hashes and pendingSize exactly as
// calling Write with those same bytes would.
func TestWriteFromFDMatchesWrite(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("cloudfs-splice-copy-range "), 300) // 7800 bytes, not block-aligned

	wh, err := e.fs.Create(ctx, dir.Ino, "written.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, wh, payload, 0); err != nil {
		t.Fatal(err)
	}

	ch, err := e.fs.Create(ctx, dir.Ino, "copied.bin")
	if err != nil {
		t.Fatal(err)
	}
	src := sourceFile(t, payload)
	n, err := e.fs.WriteFromFD(ctx, ch, int(src.Fd()), 0, 0, int64(len(payload)))
	if err != nil {
		t.Fatalf("WriteFromFD: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("wrote %d bytes, want %d", n, len(payload))
	}

	if got, want := ch.writer.staging.Size(), wh.writer.staging.Size(); got != want {
		t.Fatalf("staging size %d, want %d", got, want)
	}

	wantPending, ok := e.fs.pendingSize(wh.Ino)
	if !ok {
		t.Fatal("pendingSize missing for the Write handle")
	}
	gotPending, ok := e.fs.pendingSize(ch.Ino)
	if !ok {
		t.Fatal("pendingSize missing for the WriteFromFD handle")
	}
	if gotPending != wantPending {
		t.Fatalf("pendingSize %d, want %d", gotPending, wantPending)
	}

	wantHashes, err := wh.writer.staging.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	gotHashes, err := ch.writer.staging.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(gotHashes, wantHashes) {
		t.Fatalf("hashes differ: got %+v, want %+v", gotHashes, wantHashes)
	}

	gotBytes := make([]byte, len(payload))
	if _, err := ch.writer.staging.ReadAt(gotBytes, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatal("copied staging content does not match the source")
	}
}

// TestWriteFromFDSpansMultipleChunks exercises the internal buffer-sized
// chunking loop: the payload is bigger than one buffer, so WriteFromFD must
// issue more than one Pread/Write round trip and still land every byte at
// the right offset.
func TestWriteFromFDSpansMultipleChunks(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	size := copyFileRangeBufSize + copyFileRangeBufSize/2 + 12345
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}

	ch, err := e.fs.Create(ctx, dir.Ino, "copied-big.bin")
	if err != nil {
		t.Fatal(err)
	}
	src := sourceFile(t, payload)
	const dstOff = 17 // exercise a non-zero destination offset too
	n, err := e.fs.WriteFromFD(ctx, ch, int(src.Fd()), 0, dstOff, int64(len(payload)))
	if err != nil {
		t.Fatalf("WriteFromFD: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("wrote %d bytes, want %d", n, len(payload))
	}
	if got, want := ch.writer.staging.Size(), int64(len(payload))+dstOff; got != want {
		t.Fatalf("staging size %d, want %d", got, want)
	}
	got := make([]byte, len(payload))
	if _, err := ch.writer.staging.ReadAt(got, dstOff); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("multi-chunk copy content mismatch")
	}
}

// TestWriteFromFDShortSourceReadFails checks the required error behaviour: a
// source shorter than the requested length must fail, and staging must end
// up holding exactly the bytes that were actually read — no more.
func TestWriteFromFDShortSourceReadFails(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	dir, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	available := []byte("only ten!!") // 10 bytes
	ch, err := e.fs.Create(ctx, dir.Ino, "short.bin")
	if err != nil {
		t.Fatal(err)
	}
	src := sourceFile(t, available)
	n, err := e.fs.WriteFromFD(ctx, ch, int(src.Fd()), 0, 0, int64(len(available))+10)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if n != int64(len(available)) {
		t.Fatalf("wrote %d bytes, want %d (nothing past what was read)", n, len(available))
	}
	if got := ch.writer.staging.Size(); got != int64(len(available)) {
		t.Fatalf("staging size %d, want %d", got, len(available))
	}
	got := make([]byte, len(available))
	if _, err := ch.writer.staging.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, available) {
		t.Fatal("staging content past a short read does not match what was actually read")
	}
}
