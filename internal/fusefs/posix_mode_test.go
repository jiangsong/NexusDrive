package fusefs

import (
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
)

// TestChmodReachesTheFile: chmod(2) on the mount has to change what stat(2)
// reports, and keep reporting it. Permissions exist nowhere but here, so a
// mount that swallows the call is a mount where every checked-out script
// loses its exec bit.
func TestChmodReachesTheFile(t *testing.T) {
	requireFUSE(t)
	e := newMount(t, config.ModeWriteback)
	target := filepath.Join(e.dir, "script.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Fatalf("mode after chmod = %#o, want %#o", perm, 0o755)
	}
}

// TestCreateModeReachesTheFile: git checks a script out by creating it with
// the mode from the index rather than by calling chmod(2) afterwards, so the
// mode the kernel passes to CREATE has to be kept too.
func TestCreateModeReachesTheFile(t *testing.T) {
	requireFUSE(t)
	e := newMount(t, config.ModeWriteback)
	target := filepath.Join(e.dir, "checked-out.sh")
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("#!/bin/sh\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Fatalf("mode after create = %#o, want %#o", perm, 0o755)
	}
}

// TestBirthModeStoresOnlyWhatTheCreateDidNotWrite: recording the mode costs a
// metadata read and a write transaction, and under the usual umask of 022 the
// kernel asks for exactly the mode the create already wrote — 0644 for a file,
// 0755 for a directory. Paying for that on every create is a write per file in
// a tree copy, for no change. Mode 0 is not the same thing as "no mode": a
// file nobody may read is a real request, and it is stored.
func TestBirthModeStoresOnlyWhatTheCreateDidNotWrite(t *testing.T) {
	cases := []struct {
		name       string
		want, born uint32
		store      bool
	}{
		{"file under umask 022", 0o644, 0o644, false},
		{"directory under umask 022", 0o755, 0o755, false},
		{"checked-out script", 0o755, 0o644, true},
		{"private key", 0o600, 0o644, true},
		{"private directory", 0o700, 0o755, true},
		{"unreadable file", 0, 0o644, true},
		{"setuid bits are not ours to keep", 0o4644, 0o644, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := birthModeNeedsStore(c.want, c.born); got != c.store {
				t.Fatalf("birthModeNeedsStore(%#o, %#o) = %v, want %v", c.want, c.born, got, c.store)
			}
		})
	}
}

// TestCreateModeZeroReachesTheFile: open(2) with a mode of 0 asks for a file
// nobody may read, and answering 0644 would describe something else.
func TestCreateModeZeroReachesTheFile(t *testing.T) {
	requireFUSE(t)
	e := newMount(t, config.ModeWriteback)
	target := filepath.Join(e.dir, "sealed")
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0 {
		t.Fatalf("mode after a create with mode 0 = %#o, want 0", perm)
	}
}

// TestMkdirModeReachesTheDirectory covers the same for MKDIR.
func TestMkdirModeReachesTheDirectory(t *testing.T) {
	requireFUSE(t)
	e := newMount(t, config.ModeWriteback)
	target := filepath.Join(e.dir, "private")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("mode after mkdir = %#o, want %#o", perm, 0o700)
	}
}
