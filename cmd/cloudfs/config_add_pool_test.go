package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

func poolConfigPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	b := fmt.Sprintf("cache:\n  dir: %q\nsecrets: {backend: file}\nremotes:\n  first: {type: webdav, url: 'https://a.local/dav'}\n  home: {type: pool, pool: home}\npools:\n  home:\n    members:\n      - {remote: first}\n    replicas: 2\n",
		filepath.Join(dir, "cache"))
	if err := os.WriteFile(p, []byte(b), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The web API could add an account and join it to a pool in one call; the CLI
// could not, so building a four-drive pool from a terminal meant eight commands
// where four would do. Worse, the two halves could disagree — a remote created
// by the first command and never joined by the second.
func TestConfigAddJoinsAPoolInTheSameEditThatCreatesTheRemote(t *testing.T) {
	p := poolConfigPath(t)
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag}
	args := []string{"add", "second", "--type", "webdav", "--url", "https://b.local/dav", "--pool", "home", "--config", p}
	if err := runConfig(context.Background(), args, c); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Remotes["second"]; !ok {
		t.Fatal("the account was not created")
	}
	var joined bool
	for _, m := range cfg.Pools["home"].Members {
		if m.Remote == "second" {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("the account did not join the pool: %+v", cfg.Pools["home"].Members)
	}
	if !strings.Contains(out.String(), "home") {
		t.Errorf("the confirmation does not name the pool it joined: %q", out.String())
	}
}

// A typo in the pool name must not leave an account behind that belongs to
// nothing and was never asked for.
func TestConfigAddRefusesAnUnknownPoolWithoutCreatingTheRemote(t *testing.T) {
	p := poolConfigPath(t)
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag}
	args := []string{"add", "second", "--type", "webdav", "--url", "https://b.local/dav", "--pool", "hom", "--config", p}
	if err := runConfig(context.Background(), args, c); err == nil {
		t.Fatal("an unknown pool was accepted")
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Remotes["second"]; ok {
		t.Fatal("the account was created even though the pool does not exist")
	}
}
