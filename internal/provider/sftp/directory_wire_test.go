package sftp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"path"
	"sync/atomic"
	"testing"
	"time"

	psftp "github.com/pkg/sftp"

	"cloudfs/internal/provider"
)

func directoryReply(typ byte, id uint32, body []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{typ}, id), body...)
}

func directoryTestStatus(id, code uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, code)
	b = directoryString(b, "status")
	b = directoryString(b, "en")
	return directoryReply(dirStatus, id, b)
}

func directoryTestAttrs(size uint64, mode uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, dirRequiredAttrs)
	b = binary.BigEndian.AppendUint64(b, size)
	b = binary.BigEndian.AppendUint32(b, mode)
	b = binary.BigEndian.AppendUint32(b, 1700000000)
	return binary.BigEndian.AppendUint32(b, 1700000001)
}

func directoryTestMember(name string, attrs []byte) []byte {
	b := directoryString(nil, name)
	b = directoryString(b, "not authoritative")
	return append(b, attrs...)
}

func directoryTestNames(id uint32, members ...[]byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(members)))
	for _, m := range members {
		b = append(b, m...)
	}
	return directoryReply(dirName, id, b)
}

func TestDirectoryWireRejectsMalformedWorkingDirectory(t *testing.T) {
	for _, mode := range []string{"zero", "two", "relative", "nul", "utf8", "tail", "status-ok", "missing", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer server.Close()
				if _, err := readDirectoryPacket(server); err != nil {
					return
				}
				if writeDirectoryPacket(server, []byte{dirVersion, 0, 0, 0, 3}) != nil {
					return
				}
				req, err := readDirectoryPacket(server)
				if err != nil || req[0] != dirRealPath {
					return
				}
				id := binary.BigEndian.Uint32(req[1:5])
				name := "/working"
				switch mode {
				case "relative":
					name = "relative"
				case "nul":
					name = "/bad\x00"
				case "utf8":
					name = "/\xff"
				}
				reply := directoryTestNames(id, directoryTestMember(name, []byte{0, 0, 0, 0}))
				switch mode {
				case "zero":
					binary.BigEndian.PutUint32(reply[5:9], 0)
				case "two":
					binary.BigEndian.PutUint32(reply[5:9], 2)
				case "tail":
					reply = append(reply, 1)
				case "status-ok":
					reply = directoryTestStatus(id, 0)
				case "missing":
					reply = directoryTestStatus(id, 2)
				case "truncated":
					reply = reply[:len(reply)-1]
				}
				writeDirectoryPacket(server, reply)
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := scanDirectoryAtRoot(ctx, client, "~/child", "/", func(provider.Entry) error { t.Error("bad working directory delivered entries"); return nil })
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("malformed REALPATH: %v", err)
			}
			if mode == "missing" && !errors.Is(err, provider.ErrNotFound) {
				t.Fatal(err)
			}
			awaitDirectorySignal(t, done)
		})
	}
}

type directoryFixture struct {
	mode   string
	reads  atomic.Int64
	stats  atomic.Int64
	closes atomic.Int64
}

func (f *directoryFixture) reply(req []byte) []byte {
	if len(req) < 5 {
		return nil
	}
	id := binary.BigEndian.Uint32(req[1:5])
	switch req[0] {
	case dirInit:
		if f.mode == "version" {
			return []byte{dirVersion, 0, 0, 0, 4}
		}
		return []byte{dirVersion, 0, 0, 0, 3}
	case dirOpen:
		if f.mode == "missing" {
			return directoryTestStatus(id, 2)
		}
		if f.mode == "open-ok" {
			return directoryTestStatus(id, 0)
		}
		return directoryReply(dirHandle, id, directoryString(nil, "handle"))
	case dirRead:
		if f.reads.Add(1) > 1 {
			if f.mode == "transport-eof" {
				return nil
			}
			return directoryTestStatus(id, 1)
		}
		attrs := directoryTestAttrs(7, 0o100644)
		name := "file"
		switch f.mode {
		case "wrong-id":
			id++
		case "empty":
			return directoryTestNames(id)
		case "read-ok":
			return directoryTestStatus(id, 0)
		case "read-denied":
			return directoryTestStatus(id, 3)
		case "count":
			return directoryReply(dirName, id, binary.BigEndian.AppendUint32(nil, math.MaxUint32))
		case "attrs":
			attrs = binary.BigEndian.AppendUint32(nil, 0x40000000)
		case "extensions":
			attrs = binary.BigEndian.AppendUint32(nil, 0x80000000)
			attrs = binary.BigEndian.AppendUint32(attrs, math.MaxUint32)
		case "size":
			attrs = directoryTestAttrs(math.MaxUint64, 0o100644)
		case "parent":
			name = "../outside"
		case "invalid-utf8":
			name = string([]byte{0xff})
		case "partial-attrs":
			attrs = []byte{0, 0, 0, 0}
		}
		result := directoryTestNames(id, directoryTestMember(name, attrs))
		if f.mode == "tail" {
			result = append(result, 99)
		}
		return result
	case dirStat:
		f.stats.Add(1)
		return directoryReply(dirAttrs, id, directoryTestAttrs(9, 0o100644))
	case dirClose:
		f.closes.Add(1)
		if f.mode == "close-failed" {
			return directoryTestStatus(id, 4)
		}
		return directoryTestStatus(id, 0)
	}
	return nil
}

func startDirectoryFixture(t *testing.T, f *directoryFixture) (net.Conn, <-chan struct{}) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		for {
			req, err := readDirectoryPacket(server)
			if err != nil {
				return
			}
			if err := writeDirectoryPacket(server, f.reply(req)); err != nil || req[0] == dirClose {
				return
			}
		}
	}()
	t.Cleanup(func() {
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("directory fixture leaked its stream")
		}
	})
	return client, done
}

func TestDirectoryWireReadsRealSFTPServerWithoutWholeDirectory(t *testing.T) {
	dir := t.TempDir()
	for i := range 513 {
		write(t, dir, fmt.Sprintf("file-%04d", i), []byte("payload"))
	}
	write(t, dir, "sub/nested", []byte("nested"))
	write(t, dir, tempPrefix+"hidden", []byte("unfinished"))
	client, server := net.Pipe()
	srv, err := psftp.NewServer(server)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); defer server.Close(); srv.Serve() }()
	defer func() { client.Close(); server.Close(); srv.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, dirs := 0, 0
	err = scanDirectoryStream(ctx, client, dir, "/root", func(e provider.Entry) error {
		count++
		if e.ParentID != "/root" || e.ID != path.Join("/root", e.Name) || e.Version == "" {
			t.Fatalf("directory identity/attributes: %+v", e)
		}
		if e.Kind == provider.KindDir {
			dirs++
		} else if e.Size != 7 {
			t.Fatalf("file size: %+v", e)
		}
		return nil
	})
	if err != nil || count != 514 || dirs != 1 {
		t.Fatalf("real protocol enumeration: count=%d dirs=%d err=%v", count, dirs, err)
	}
}

func TestDirectoryWireRejectsIncompleteOrMalformedResponses(t *testing.T) {
	for _, mode := range []string{"version", "missing", "open-ok", "wrong-id", "empty", "read-ok", "read-denied", "count", "attrs", "extensions", "size", "parent", "invalid-utf8", "tail", "close-failed", "transport-eof"} {
		t.Run(mode, func(t *testing.T) {
			f := &directoryFixture{mode: mode}
			rw, done := startDirectoryFixture(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := scanDirectoryStream(ctx, rw, "/server/root", "/", func(provider.Entry) error { return nil })
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("malformed stream accepted or hung: %v", err)
			}
			if mode == "missing" && !errors.Is(err, provider.ErrNotFound) {
				t.Fatalf("missing directory classification: %v", err)
			}
			if mode == "read-denied" && !errors.Is(err, provider.ErrAuth) {
				t.Fatalf("denied directory classification: %v", err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("failed scan retained its stream")
			}
		})
	}
}

func TestDirectoryWireFetchesMissingAttributes(t *testing.T) {
	f := &directoryFixture{mode: "partial-attrs"}
	rw, _ := startDirectoryFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count := 0
	err := scanDirectoryStream(ctx, rw, "/server/root", "/", func(e provider.Entry) error {
		count++
		if e.Size != 9 || e.Kind != provider.KindFile || e.ModTime.Unix() != 1700000001 {
			t.Fatalf("missing attributes guessed instead of queried: %+v", e)
		}
		return nil
	})
	if err != nil || count != 1 || f.stats.Load() != 1 || f.closes.Load() != 1 {
		t.Fatalf("attribute fallback: count=%d stat=%d close=%d err=%v", count, f.stats.Load(), f.closes.Load(), err)
	}
}

func TestDirectoryWireVisitorFailureDoesNotReadOrReplayMore(t *testing.T) {
	f := &directoryFixture{}
	rw, done := startDirectoryFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stop := errors.New("TEMP full")
	count := 0
	err := scanDirectoryStream(ctx, rw, "/server/root", "/", func(provider.Entry) error { count++; return stop })
	if !errors.Is(err, stop) || count != 1 || f.reads.Load() != 1 {
		t.Fatalf("visitor failure replayed or lost: count=%d reads=%d err=%v", count, f.reads.Load(), err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("visitor failure left remote handles alive")
	}
}

func TestDirectoryWireCancellationInterruptsEveryNetworkPhase(t *testing.T) {
	for _, phase := range []byte{dirInit, dirOpen, dirRead, dirStat, dirClose} {
		t.Run(fmt.Sprint(phase), func(t *testing.T) {
			client, server := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer client.Close()
			defer server.Close()
			entered, done := make(chan struct{}), make(chan struct{})
			f := &directoryFixture{mode: "partial-attrs"}
			go func() {
				defer close(done)
				defer server.Close()
				for {
					req, err := readDirectoryPacket(server)
					if err != nil {
						return
					}
					if req[0] == phase {
						close(entered)
						var b [1]byte
						server.Read(b[:]) // Client cancellation must interrupt this too.
						return
					}
					if err := writeDirectoryPacket(server, f.reply(req)); err != nil {
						return
					}
				}
			}()
			result := make(chan error, 1)
			go func() {
				result <- scanDirectoryStream(ctx, client, "/root", "/", func(provider.Entry) error { return nil })
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("fixture did not reach selected phase")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled IO: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled scan did not exit")
			}
			<-done
		})
	}
}

func TestDirectoryWireBoundsFramesAndFields(t *testing.T) {
	for _, input := range [][]byte{
		nil, {0}, {0, 0, 0, 0}, {0, 0, 0, 2, 1},
		binary.BigEndian.AppendUint32(nil, maxDirectoryPacket+1),
		binary.BigEndian.AppendUint32(nil, math.MaxUint32),
	} {
		if b, err := readDirectoryPacket(bytes.NewReader(input)); err == nil || len(b) != 0 {
			t.Fatalf("invalid frame accepted: %x %v", input, err)
		}
	}
	valid := directoryTestAttrs(7, 0o100644)
	for n := range len(valid) {
		f := directoryFields{b: valid[:n]}
		f.attrs()
		if f.done() == nil {
			t.Fatalf("truncated attributes accepted at %d", n)
		}
	}
	w := shortDirectoryWriter{}
	if err := writeDirectoryPacket(w, []byte{dirInit, 0, 0, 0, 3}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short request write ignored: %v", err)
	}
}

type shortDirectoryWriter struct{}

func (shortDirectoryWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func FuzzDirectoryWireFields(f *testing.F) {
	f.Add(directoryTestAttrs(7, 0o100644))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > maxDirectoryPacket {
			t.Skip()
		}
		fields := directoryFields{b: b}
		fields.attrs()
		fields.done()
		fields = directoryFields{b: b}
		fields.str()
		fields.done()
		directoryStatusCode(b)
	})
}
