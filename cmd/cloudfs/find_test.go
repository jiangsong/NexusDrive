package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/control"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

func TestFindCLIFiltersAndSorts(t *testing.T) {
	cfg, configPath := uploadCLIConfig(t)
	dir := t.TempDir()
	s, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := fakeprovider.New("ali")
	f, err := vfs.New(vfs.Options{Meta: s, Cache: c, Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: p}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	for i, name := range []string{"old.go", "new.go", "notes.md"} {
		n := meta.Node{ParentIno: meta.RootIno, Name: name, Kind: provider.KindFile, Size: int64(100 * (i + 1)), MTime: base.Add(time.Duration(i) * time.Hour), Remote: "ali", RemoteID: name, Version: "1", TTL: time.Hour}
		if _, err := s.Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := control.NewServer(&control.Collector{FS: f, Cache: c}).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var out bytes.Buffer
	if err := runFind(ctx, []string{"--ext", "go", "--sort", "mtime", "--config", configPath}, &out); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(strings.Fields(out.String())); got != "[/new.go /old.go]" {
		t.Fatalf("find --ext go --sort mtime: %q", out.String())
	}
	out.Reset()
	if err := runFind(ctx, []string{"--size", ">150", "--after", base.Add(30 * time.Minute).Format(time.RFC3339), "--config", configPath}, &out); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(strings.Fields(out.String())); got != "[/new.go /notes.md]" {
		t.Fatalf("find --size --after: %q", out.String())
	}
	out.Reset()
	if err := runFind(ctx, []string{"*.md", "--type", "file", "--json", "--config", configPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"path":"/notes.md"`) || !strings.Contains(out.String(), `"coverage"`) || !strings.Contains(out.String(), `"size":300`) {
		t.Fatalf("find --json: %s", out.String())
	}
	for _, bad := range [][]string{{"--config", configPath}, {"--size", "lots", "--config", configPath}, {"a", "--type", "link", "--config", configPath}, {"a", "--after", "yesterday", "--config", configPath}, {"a", "--bogus", "1", "--config", configPath}, {"a", "b", "--config", configPath}} {
		if err := runFind(ctx, bad, &out); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	// --all lists the tree first: a file in a directory nobody opened appears.
	for _, name := range []string{"old.go", "new.go", "notes.md"} {
		p.Seed(name, []byte("x"))
	}
	p.Seed("deep/never/x.go", []byte("x"))
	out.Reset()
	if err := runFind(ctx, []string{"--all", "--ext", "go", "--config", configPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/deep/never/x.go") {
		t.Fatalf("find --all did not crawl first: %q", out.String())
	}
}
