//go:build !windows

package fusefs

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGitClonePackOnMount(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	e := newMount(t, "")
	src := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(src, "init", "-q")
	data := make([]byte, 256*1024+633)
	rng := rand.New(rand.NewSource(1))
	rng.Read(data)
	for i := 0; i < 8; i++ {
		data[i*4096]++
		if err := os.WriteFile(filepath.Join(src, "data.bin"), data, 0644); err != nil {
			t.Fatal(err)
		}
		git(src, "add", ".")
		git(src, "commit", "-qm", "fixture")
	}
	dst := filepath.Join(e.dir, "clone")
	git(src, "-c", "pack.threads=2", "clone", "--no-local", src, dst)
	git(dst, "fsck", "--full")
	got, err := os.ReadFile(filepath.Join(dst, "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("clone content mismatch")
	}
}
