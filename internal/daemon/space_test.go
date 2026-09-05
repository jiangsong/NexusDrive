package daemon

import (
	"context"
	"errors"
	"math"
	"syscall"
	"testing"
)

func TestDaemonAppliesMinFreeToJournalWrites(t *testing.T) {
	for _, requireOwner := range []bool{false, true} {
		cfg, _ := writeConfig(t, baseConfig)
		cfg.Cache.MinFree = math.MaxInt64
		d, err := Open(context.Background(), Options{Config: cfg, NoBackground: true, RequireOwner: requireOwner})
		if err != nil {
			t.Fatal(err)
		}
		s, err := d.Journal.NewStaging(nil)
		if err != nil {
			d.Close()
			t.Fatal(err)
		}
		n, err := s.WriteAt([]byte("must not be accepted"), 0)
		if n != 0 || !errors.Is(err, syscall.ENOSPC) {
			t.Errorf("requireOwner=%v: %d %v", requireOwner, n, err)
		}
		if s.Size() != 0 || d.Cache.Stats().WriteReservedBytes != 0 {
			t.Error("denied write changed data or leaked reservation")
		}
		s.Discard()
		root, err := d.FS.Lookup(context.Background(), 1, "demo")
		if err != nil {
			d.Close()
			t.Fatal(err)
		}
		h, err := d.FS.Create(context.Background(), root.Ino, "denied-write")
		if err != nil {
			d.Close()
			t.Fatal(err)
		}
		if n, err := d.FS.Write(context.Background(), h, []byte("payload"), 0); n != 0 || !errors.Is(err, syscall.ENOSPC) {
			t.Errorf("VFS write: %d %v", n, err)
		}
		d.FS.Release(context.Background(), h)
		d.Close()
	}
}
