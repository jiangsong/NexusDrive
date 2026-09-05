package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestForgetCheckedRetainsChargeOnUnlinkFailureAndRetries(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(map[bool]string{false: "block", true: "whole"}[whole], func(t *testing.T) {
			c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 64})
			body := "retained content"
			p := c.blockPath(key.hash(), 0)
			if whole {
				if err := c.LinkFile(key, wholeSource(t, body), int64(len(body))); err != nil {
					t.Fatal(err)
				}
				p = c.hydratedPath(key.hash())
			} else if err := c.Put(key, 0, []byte(body), int64(len(body))); err != nil {
				t.Fatal(err)
			}
			before := c.Stats()
			saved := filepath.Join(t.TempDir(), "saved")
			if err := os.Rename(p, saved); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
			if err := c.ForgetChecked(key); err == nil {
				t.Fatal("cleanup silently removed a directory")
			}
			if after := c.Stats(); after.Bytes != before.Bytes || after.WholeBytes != before.WholeBytes || after.Blocks != before.Blocks {
				t.Fatalf("failed unlink released accounting: before=%+v after=%+v", before, after)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(saved, p); err != nil {
				t.Fatal(err)
			}
			if err := c.ForgetChecked(key); err != nil {
				t.Fatal(err)
			}
			if after := c.Stats(); after.Bytes != 0 || after.Blocks != 0 || after.HydratedFiles != 0 {
				t.Fatalf("retry did not release content: %+v", after)
			}
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private content still present: %v", err)
			}
			if err := c.ForgetChecked(key); err != nil {
				t.Fatalf("cleanup not idempotent: %v", err)
			}
		})
	}
}

func TestForgetCheckedRemovesOnlySymlinkName(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 64})
	source := wholeSource(t, "source survives")
	if err := c.LinkFile(key, source, 15); err != nil {
		t.Fatal(err)
	}
	p := c.hydratedPath(key.hash())
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, p); err != nil {
		t.Fatal(err)
	}
	if err := c.ForgetChecked(key); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(source); err != nil || string(b) != "source survives" {
		t.Fatalf("cleanup followed link: %q %v", b, err)
	}
}
