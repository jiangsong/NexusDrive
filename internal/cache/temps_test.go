package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestartCountsAndCollectsOnlyRecognizedTemps(t *testing.T) {
	c, _ := newTest(t, Options{})
	if err := c.LinkFile(key, wholeSource(t, "kept"), 4); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var paths []string
	for _, dir := range []string{"blocks", "hydrated"} {
		for _, name := range []string{".hydrate-old", ".install-old", ".flush-old", key.hash() + "-0.tmp", key.hash() + "-0.part.tmp", key.hash() + ".key.tmp"} {
			p := filepath.Join(c.opt.Dir, dir, name)
			if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, p)
		}
	}
	unknown := filepath.Join(c.opt.Dir, "hydrated", "user-notes.tmp")
	if err := os.WriteFile(unknown, []byte("leave alone"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := New(c.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if s := r.Stats(); s.OrphanBytes != 36 || s.Bytes != 40 || s.HydratedFiles != 1 || s.Blocks != 0 {
		t.Fatalf("reload: %+v", s)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("startup mutated temp: %v", err)
		}
	}
	if err := r.GC(); err != nil {
		t.Fatal(err)
	}
	if s := r.Stats(); s.OrphanBytes != 0 || s.Bytes != 4 {
		t.Fatalf("gc: %+v", s)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("temp retained: %s %v", p, err)
		}
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown removed: %v", err)
	}
	if b, ok := r.Get(key, 0); !ok || string(b) != "kept" {
		t.Fatalf("payload changed: %q %v", b, ok)
	}
}

func TestLiveTempsAreNotCollectedByGC(t *testing.T) {
	c, _ := newTest(t, Options{})
	p := filepath.Join(c.opt.Dir, "hydrated", ".hydrate-live")
	if err := os.WriteFile(p, []byte("active"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("GC removed active temp: %v", err)
	}
}
