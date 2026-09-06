package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

const poolConfigTemplate = `
cache:
  dir: %s
  block_size: 64KiB
remotes:
  a: { type: fake }
  b: { type: fake }
  c: { type: fake }
  home: { type: pool, pool: home }
pools:
  home:
    members: [{remote: a}, {remote: b}, {remote: c}]
    replicas: 2
    probe_interval: 1h
    out_after: 1s
mounts:
  - path: %s
    layout:
      /: { remote: home, dir_ttl: 1s }
`

// TestPoolEndToEndThroughFUSE: the whole product through the kernel. A file
// written with a shell into the mount lands on one member at its real
// path, is replicated to a second, survives the first member dying (still
// readable, then rebuilt on the third), and the surviving copies are what
// the vendor's app would show.
func TestPoolEndToEndThroughFUSE(t *testing.T) {
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(poolConfigTemplate, cacheDir, mountDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: "e2e", NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	members := map[string]*fakeprovider.Fake{}
	for _, name := range []string{"a", "b", "c"} {
		f, _ := provider.Unwrap(d.Providers[name]).(*fakeprovider.Fake)
		if f == nil {
			t.Fatalf("%s is not the fake", name)
		}
		members[name] = f
	}
	p := d.Pools["home"]
	m, err := fusefs.MountFS(fusefs.MountOptions{Options: fusefs.Options{FS: d.FS, AttrTimeout: time.Second, EntryTimeout: time.Second}, Path: mountDir})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer m.Unmount()

	// Write through the kernel.
	if err := os.MkdirAll(filepath.Join(mountDir, "photos", "2026"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountDir, "photos", "2026", "cat.jpg"), []byte("not really a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A write settles for two seconds before it is eligible to go up (the
	// daemon's WriteSettle); drain until the queue is empty.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := d.Uploader.DrainAll(ctx); err != nil {
			t.Fatal(err)
		}
		st, _ := d.Journal.Stats(ctx)
		if st.Pending+st.Uploading == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload never drained: %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	holders := func() []string {
		var out []string
		for _, name := range []string{"a", "b", "c"} {
			if _, ok := members[name].Content("/photos/2026/cat.jpg"); ok {
				out = append(out, name)
			}
		}
		return out
	}
	if h := holders(); len(h) != 1 {
		st, _ := d.Journal.Stats(ctx)
		t.Fatalf("after upload the file is on %v, want exactly one member (journal %+v, a=%v b=%v c=%v)", h, st, members["a"].Tree(), members["b"].Tree(), members["c"].Tree())
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if h := holders(); len(h) != 2 {
		t.Fatalf("after repair the file is on %v, want two", h)
	}
	// The mirrored tree is the real one on every holder.
	for _, name := range holders() {
		if got := strings.Join(members[name].Tree(), ","); !strings.Contains(got, "/photos/2026/cat.jpg") {
			t.Fatalf("%s tree = %s", name, got)
		}
	}
	// The primary dies. Reads keep working through the kernel.
	primary := holders()[0]
	members[primary].SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		// The pool notices from its own listings (the kernel keeps its
		// own page and entry caches, which this test does not drop).
		if err := p.ScrubPath(ctx, "/photos/2026"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.FS.DropCaches(ctx); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(mountDir, "photos", "2026"))
		if err != nil || len(entries) != 1 || entries[0].Name() != "cat.jpg" {
			t.Fatalf("readdir while %s is down = %v, %v", primary, entries, err)
		}
		data, err := os.ReadFile(filepath.Join(mountDir, "photos", "2026", "cat.jpg"))
		if err != nil || string(data) != "not really a jpeg" {
			t.Fatalf("read while %s is down = %q, %v", primary, data, err)
		}
	}
	for _, st := range p.Status() {
		if st.Name == primary && st.Health.State != provider.HealthDown {
			t.Fatalf("%s after three failed listings = %s", primary, st.Health.State)
		}
	}
	// It stays down past out_after: the pool rebuilds the copy elsewhere.
	time.Sleep(1200 * time.Millisecond)
	if n, err := p.ScanOnce(ctx); err != nil || n != 1 {
		t.Fatalf("scan after %s went out = %d, %v (state %s)", primary, n, err, p.Status()[0].Health.State)
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, name := range []string{"a", "b", "c"} {
		if name == primary {
			continue
		}
		if _, ok := members[name].Content("/photos/2026/cat.jpg"); ok {
			live++
		}
	}
	if live != 2 {
		t.Fatalf("after re-replication %d live members hold the file, want 2", live)
	}
	// A rename through the kernel fans out to the live holders and leaves
	// the dead one an op to replay.
	if err := os.Rename(filepath.Join(mountDir, "photos", "2026", "cat.jpg"), filepath.Join(mountDir, "photos", "2026", "kitten.jpg")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if name == primary {
			continue
		}
		if _, ok := members[name].Content("/photos/2026/kitten.jpg"); !ok {
			t.Fatalf("%s did not rename: %v", name, members[name].Tree())
		}
	}
	data, err := os.ReadFile(filepath.Join(mountDir, "photos", "2026", "kitten.jpg"))
	if err != nil || string(data) != "not really a jpeg" {
		t.Fatalf("read after rename = %q, %v", data, err)
	}
}
