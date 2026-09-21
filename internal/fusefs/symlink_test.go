//go:build !windows

package fusefs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A local npm package exercises both node_modules/package directory links and
// .bin command links without a registry or any external account.
func TestMountSymlinkNpmCI(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not installed")
	}
	e := newMount(t, "")
	fixtures := map[string]string{
		"package.json":      `{"name":"cloudfs-link-test","version":"1.0.0","scripts":{"test":"fixture-tool","build":"fixture-tool"},"dependencies":{"fixture-tool":"file:tool"}}`,
		"package-lock.json": `{"name":"cloudfs-link-test","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"cloudfs-link-test","version":"1.0.0","dependencies":{"fixture-tool":"file:tool"}},"node_modules/fixture-tool":{"resolved":"tool","link":true},"tool":{"name":"fixture-tool","version":"1.0.0","bin":{"fixture-tool":"cli.js"}}}}`,
		"tool/package.json": `{"name":"fixture-tool","version":"1.0.0","bin":{"fixture-tool":"cli.js"}}`,
		"tool/cli.js":       "#!/usr/bin/env node\nconsole.log('cloudfs-npm-link-ok')\n",
	}
	for name, body := range fixtures {
		p := filepath.Join(e.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, args := range [][]string{{"ci", "--offline", "--no-audit", "--no-fund"}, {"test"}, {"run", "build"}, {"ci", "--offline", "--no-audit", "--no-fund"}, {"test"}} {
		// Do not chdir into the filesystem served by this process between
		// fork and exec: with -race, vfork can suspend its FUSE server.
		shellArgs := append([]string{"-c", `cd "$1" && shift && exec "$@"`, "sh", e.dir, npm}, args...)
		cmd := exec.CommandContext(ctx, "sh", shellArgs...)
		cmd.Env = append(os.Environ(), "npm_config_cache="+t.TempDir(), "npm_config_update_notifier=false")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("npm %v: %v\n%s", args, err, out)
		}
		t.Logf("npm %v: %s", args, out)
	}
}

func TestMountSymlink(t *testing.T) {
	e := newMount(t, "")
	dir := e.dir
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "bin", "tool"), []byte("#!/bin/sh\nprintf link-ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".bin", "tool")
	if err := os.Symlink("../pkg/bin/tool", link); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(link); err != nil || target != "../pkg/bin/tool" {
		t.Fatalf("%q %v", target, err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lstat %v %v", st, err)
	}
	if out, err := exec.Command("sh", "-c", `exec "$1"`, "sh", link).CombinedOutput(); err != nil || string(out) != "link-ok" {
		t.Fatalf("exec %q %v", out, err)
	}
	if err := os.Symlink("pkg", filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(dir, "linked-dir", "bin", "tool")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "absolute")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "absolute")); err != nil || string(got) != "external" {
		t.Fatalf("absolute: %q %v", got, err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink("missing", dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dangling); !os.IsNotExist(err) {
		t.Fatalf("dangling: %v", err)
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "loop")); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("loop: %v", err)
	}
	// POSIX unlink removes the link, not its target.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "bin", "tool")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMountSymlinkRenameReplacement(t *testing.T) {
	for _, uploaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "uploaded"}[uploaded], func(t *testing.T) {
			e := newMount(t, "")
			if !uploaded {
				e.up.Stop()
			}
			p := func(n string) string { return filepath.Join(e.dir, n) }
			if err := os.WriteFile(p("file"), []byte("payload"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("target", p("link")); err != nil {
				t.Fatal(err)
			}
			if uploaded {
				if _, err := e.up.Flush(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(p("link"), p("file")); err != nil {
				t.Fatal(err)
			}
			if got, err := os.Readlink(p("file")); err != nil || got != "target" {
				t.Fatalf("replace file: %q %v", got, err)
			}
			if err := os.WriteFile(p("new"), []byte("replacement"), 0o644); err != nil {
				t.Fatal(err)
			}
			if uploaded {
				if _, err := e.up.Flush(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(p("new"), p("file")); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(p("file")); err != nil || string(got) != "replacement" {
				t.Fatalf("replace link: %q %v", got, err)
			}
		})
	}
}
