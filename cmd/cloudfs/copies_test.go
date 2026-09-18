package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/control"
	"cloudfs/internal/journal"
)

func TestCopiesCLIReadsActiveOwnerWithoutRecovering(t *testing.T) {
	cfg, p := uploadCLIConfig(t) // Provider deliberately cannot be constructed.
	j, u := uploadCLIJournal(t, cfg)
	defer j.Close()
	ctx := context.Background()
	if _, err := j.Claim(ctx, u.Remote, 1); err != nil {
		t.Fatal(err)
	}
	c, err := j.BeginCopy(ctx, journal.CopySpec{SourcePath: "/a", SourceRemote: "ali", SourceID: "source", SourceVersion: "v", Size: 8, TargetPath: "/b", TargetRemote: "ali", TargetParentID: "root", Mode: "writeback"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("xx")); err != nil {
		t.Fatal(err)
	}
	for _, online := range []bool{false, true} {
		if online {
			srv, err := control.NewServer(&control.Collector{Journal: j}).Start(ctx, cfg.Control.Socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
		}
		for _, args := range [][]string{{"list", "--limit", "1"}, {"show", c.Job().ID}} {
			var out bytes.Buffer
			if err := runCopies(ctx, append(args, "--config", p, "--json"), &out); err != nil {
				t.Fatal(err)
			}
			var result control.CopiesResponse
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Copies) != 1 || result.Copies[0].Checkpoint != 6 || result.Copies[0].State != journal.CopyPreparing {
				t.Fatalf("inspection: %s %v", out.String(), err)
			}
		}
	}
	row, err := j.Get(ctx, u.ID)
	if err != nil || row.State != journal.StateUploading {
		t.Fatalf("inspection recovered live upload: %+v %v", row, err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.StateDir(), "journal", "copies", c.Job().ID+".part"))
	if err != nil || string(b) != "prefixxx" {
		t.Fatalf("inspection truncated live tail: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir(), "meta.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection constructed metadata stack")
	}
}

func TestCopiesCLIEmptyAndInvalidRequestsCreateNothing(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	var out bytes.Buffer
	if err := runCopies(context.Background(), []string{"list", "--json", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"copies\":[]}\n" {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"show"}, {"show", "bad-id"}, {"show", journal.NewID()}, {"retry"}, {"list", "extra"}, {"list", "--limit", "1001"}, {"list", "--limit"}, {"list", "--cursor", "../bad"}, {"list", "--timeout", "0s"}, {"forget", journal.NewID()}, {"forget", "--confirm"}, {"forget", "../bad", "--confirm"}, {"forget", journal.NewID(), "--confirm", "--limit", "1"}, {"cancel", journal.NewID(), "--confirm"}} {
		if err := runCopies(context.Background(), append(args, "--config", p), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	assertNoDaemonStorage(t, cfg, "read-only command")
}
