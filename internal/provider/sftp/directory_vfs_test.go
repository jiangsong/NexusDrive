package sftp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

type directoryStreamingOnly struct{ *Provider }

func (*directoryStreamingOnly) List(context.Context, string, string) ([]provider.Entry, string, error) {
	return nil, "", errors.New("VFS must use SFTP ListStream, not collect a full compatibility slice")
}

func TestDirectorySSHThroughInstrumentationAndAtomicVFSRefresh(t *testing.T) {
	var complete atomic.Bool
	complete.Store(true)
	var members atomic.Int64
	s := newDirectorySSHServer(t, "", func(ch ssh.Channel) {
		f := &directoryFixture{}
		page := 0
		for {
			req, err := readDirectoryPacket(ch)
			if err != nil {
				return
			}
			reply := f.reply(req)
			if req[0] == dirRead {
				id := binary.BigEndian.Uint32(req[1:5])
				if !complete.Load() && page == 3 {
					return
				} // After more than one VFS TEMP batch.
				if page < 5 {
					var entries [][]byte
					for i := range 100 {
						name := fmt.Sprintf("file-%04d", page*100+i)
						if !complete.Load() {
							name = "new-" + name
						}
						entries = append(entries, directoryTestMember(name, directoryTestAttrs(7, 0o100644)))
					}
					reply = directoryTestNames(id, entries...)
					members.Add(100)
				} else {
					reply = directoryTestStatus(id, 1)
				}
				page++
			}
			if writeDirectoryPacket(ch, reply) != nil {
				return
			}
		}
	})
	p := s.provider(t)
	ctx := context.Background()
	var seconds atomic.Int64
	seconds.Store(1700000000)
	now := func() time.Time { return time.Unix(seconds.Load(), 0) }
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cached, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	stats := provider.NewStats()
	fs, err := vfs.New(vfs.Options{Meta: store, Cache: cached, Now: now, DefaultDirTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/nas", Remote: "nas", RootID: "/", Provider: provider.Instrument(&directoryStreamingOnly{p}, stats), Mode: config.ModeReadonly}}})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	page, err := fs.ReadDirPagePath(ctx, "/nas", vfs.DirectoryPageOptions{Limit: 10, Count: true})
	if err != nil || len(page.Entries) != 10 || page.Total != 500 || stats.Snapshot()["list"] != 1 {
		t.Fatalf("cold SSH page: %+v calls=%v err=%v", page, stats.Snapshot(), err)
	}
	root, err := store.Resolve(ctx, "/nas")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.DirState(ctx, root.Ino)
	if err != nil {
		t.Fatal(err)
	}
	complete.Store(false)
	seconds.Add(120)
	page, err = fs.ReadDirPagePath(ctx, "/nas", vfs.DirectoryPageOptions{Limit: 10, Count: true})
	if err != nil || page.Total != 500 || stats.Snapshot()["list"] != 2 || members.Load() != 800 || s.subsystems.Load() != 2 {
		t.Fatalf("partial SSH page published/replayed: %+v calls=%v members=%d streams=%d err=%v", page, stats.Snapshot(), members.Load(), s.subsystems.Load(), err)
	}
	if state, err := store.DirState(ctx, root.Ino); err != nil || state != before {
		t.Fatalf("incomplete SSH listing advanced TTL: %+v %v", state, err)
	}
	entries, err := store.Children(ctx, root.Ino)
	if err != nil || len(entries) != 500 {
		t.Fatalf("old tree changed: n=%d err=%v", len(entries), err)
	}
	for i, e := range entries {
		if e.Name != fmt.Sprintf("file-%04d", i) {
			t.Fatalf("partial replacement escaped TEMP: %+v", e)
		}
	}
}
