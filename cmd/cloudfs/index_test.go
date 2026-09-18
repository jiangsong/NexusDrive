package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloudfs/internal/control"
	"cloudfs/internal/embed"
	"cloudfs/internal/index"
	"cloudfs/internal/textract"
)

func TestIndexSearchFlags(t *testing.T) {
	q, err := indexSearchFromArgs([]string{"网盘", "--path", "/work", "--mode", "keyword", "--limit", "7"})
	if err != nil || q.Query != "网盘" || q.Roots[0] != "/work" || q.Mode != "keyword" || q.TopK != 7 {
		t.Fatalf("%+v %v", q, err)
	}
	q, err = indexSearchFromArgs([]string{"quarterly", "report", "--json"})
	if err != nil || q.Query != "quarterly report" || q.Roots != nil || q.TopK != 0 {
		t.Fatalf("phrase: %+v %v", q, err)
	}
	for _, bad := range [][]string{{}, {"x", "--mode", "magic"}, {"x", "--limit", "0"}, {"x", "--limit", "101"}, {"x", "--path", "relative"}, {"x", "--bogus", "y"}} {
		if _, err := indexSearchFromArgs(bad); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}

func TestDropFirstPositionalKeepsFlagValues(t *testing.T) {
	got := dropFirstPositional([]string{"--path", "search", "--json", "search", "hello", "--confirm"}, "json", "confirm")
	if strings.Join(got, " ") != "--path search --json hello --confirm" {
		t.Fatalf("%q", got)
	}
	if got := dropFirstPositional([]string{"--mode=hybrid", "search", "x"}, "json"); strings.Join(got, " ") != "--mode=hybrid x" {
		t.Fatalf("%q", got)
	}
}

func TestIndexRebuildAndRemoveNeedConfirm(t *testing.T) {
	_, p := uploadCLIConfig(t)
	var out bytes.Buffer
	err := runIndex(context.Background(), []string{"rebuild", "--config", p}, &out)
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("rebuild without confirm: %v", err)
	}
	err = runIndex(context.Background(), []string{"rm", "/notes", "--config", p}, &out)
	if err == nil || !strings.Contains(err.Error(), "--confirm") || !strings.Contains(err.Error(), "/notes") {
		t.Fatalf("rm without confirm: %v", err)
	}
	// Confirmed, but no daemon: the CLI says so rather than pretending.
	for _, args := range [][]string{{"rebuild", "--confirm"}, {"rm", "/notes", "--confirm"}, {"add", "/notes"}, {"retry"}} {
		err := runIndex(context.Background(), append(args, "--config", p), &out)
		if err == nil || !strings.Contains(err.Error(), "requires the running daemon") {
			t.Fatalf("%v offline: %v", args, err)
		}
	}
	for _, bad := range [][]string{{}, {"dance"}, {"add", "relative"}, {"rm"}, {"status", "extra"}, {"retry", "a", "b"}} {
		if err := runIndex(context.Background(), append(bad, "--config", p), &out); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}

func TestIndexStatusWithoutAnIndexSaysItIsOff(t *testing.T) {
	_, p := uploadCLIConfig(t)
	var out bytes.Buffer
	if err := runIndex(context.Background(), []string{"status", "--config", p}, &out); err != nil || !strings.Contains(out.String(), "index.enabled: false") {
		t.Fatalf("%s %v", out.String(), err)
	}
	out.Reset()
	if err := runIndex(context.Background(), []string{"status", "--config", p, "--json"}, &out); err != nil || strings.TrimSpace(out.String()) != `{"enabled":false}` {
		t.Fatalf("%s %v", out.String(), err)
	}
	if err := runIndex(context.Background(), []string{"search", "x", "--config", p}, &out); err == nil || !strings.Contains(err.Error(), "index.enabled") {
		t.Fatalf("search without an index: %v", err)
	}
}

// indexCLIStore builds an index.db under the CLI config's cache directory
// with one document, the way a daemon would have left it.
func indexCLIStore(t *testing.T, cacheDir string) {
	t.Helper()
	st, err := index.OpenStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	text := "the quarterly report is due on friday"
	chunks := []textract.Chunk{{Seq: 0, StartOff: 0, EndOff: int64(len(text)), Text: text}}
	if _, err := st.UpsertDocument(ctx, index.Document{Remote: "ali", RemoteID: "r1", Version: "v1", Path: "/work/report.md", Kind: "markdown"}, text, chunks); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRule(ctx, index.Rule{Path: "/work", Source: "tool"}); err != nil {
		t.Fatal(err)
	}
}

// cliIndex is IndexControl for the online half of the CLI tests: the
// daemon's answers are canned, and what the CLI sent is recorded.
type cliIndex struct {
	added    []index.Rule
	removed  []string
	rebuilt  int
	retried  []string
	queries  []index.SearchQuery
	embedder *embed.Fake
}

func (c *cliIndex) Status(context.Context, string) (index.Status, error) {
	st := index.Status{Enabled: true, Docs: index.DocCounts{OK: 1}, ChunksTotal: 1, Progress: index.Progress{Running: true}, Embedding: index.EmbeddingStatus{Provider: "none"}}
	if c.embedder != nil {
		st.Vectors, st.MaxChunks = 1, 1000
		st.Embedding = index.EmbeddingStatus{Provider: "ollama", Model: "fake", Dim: c.embedder.Dim(), Host: "127.0.0.1", Healthy: true, Embedded: 1, CharsThisMonth: 40}
	}
	return st, nil
}
func (c *cliIndex) Rules(context.Context) ([]index.RuleView, error) {
	out := []index.RuleView{}
	for _, r := range c.added {
		out = append(out, index.RuleView{Rule: r, Documents: 1})
	}
	return out, nil
}
func (c *cliIndex) AddRule(_ context.Context, r index.Rule) error {
	c.added = append(c.added, r)
	return nil
}
func (c *cliIndex) RemoveRule(_ context.Context, p string) error {
	c.removed = append(c.removed, p)
	return nil
}
func (c *cliIndex) Rebuild(context.Context) error { c.rebuilt++; return nil }
func (c *cliIndex) Retry(_ context.Context, p string) (int64, error) {
	c.retried = append(c.retried, p)
	return 3, nil
}
func (c *cliIndex) Failed(context.Context, string, int) ([]index.FailedDoc, string, error) {
	return nil, "", nil
}
func (c *cliIndex) Search(_ context.Context, q index.SearchQuery) (index.SearchResult, error) {
	c.queries = append(c.queries, q)
	return index.SearchResult{Hits: []index.Hit{{Path: "/work/report.md", Snippet: "quarterly report", Stale: true}}, ModeUsed: "keyword", Docs: 1}, nil
}
func (c *cliIndex) Text(_ context.Context, p string, off int64, _ int) (index.TextPage, error) {
	return index.TextPage{Path: p, Offset: off, EOF: true}, nil
}
func (c *cliIndex) Watch() (<-chan index.Progress, func()) {
	ch := make(chan index.Progress)
	return ch, func() {}
}
func (c *cliIndex) Identity(context.Context) (string, error) { return "", nil }
func (c *cliIndex) Embedder() embed.Embedder {
	if c.embedder == nil {
		return nil
	}
	return c.embedder
}
func (c *cliIndex) RecordedEmbedding(context.Context) (string, int, error) { return "", 0, nil }

func TestIndexCLIReadsTheStoreOfflineAndTheDaemonOnline(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	indexCLIStore(t, cfg.StateDir())
	ctx := context.Background()

	var out bytes.Buffer
	if err := runIndex(ctx, []string{"status", "--config", p, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var st index.Status
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || !st.Enabled || st.Docs.OK != 1 || st.ChunksTotal != 1 {
		t.Fatalf("offline status: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runIndex(ctx, []string{"status", "--config", p}, &out); err != nil || !strings.Contains(out.String(), "offline") || !strings.Contains(out.String(), "1 ok") {
		t.Fatalf("offline status table: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runIndex(ctx, []string{"rules", "--config", p}, &out); err != nil || !strings.Contains(out.String(), "/work") || !strings.Contains(out.String(), "tool") {
		t.Fatalf("offline rules: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runIndex(ctx, []string{"search", "quarterly", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "offline: stale marks unavailable") || !strings.Contains(s, "/work/report.md") || strings.Contains(s, "[stale]") {
		t.Fatalf("offline search: %s", s)
	}
	out.Reset()
	if err := runIndex(ctx, []string{"search", "quarterly", "--config", p, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var res index.SearchResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res.Hits) != 1 || res.Hits[0].Path != "/work/report.md" {
		t.Fatalf("offline search json: %s %v", out.String(), err)
	}

	fake := &cliIndex{}
	srv, err := control.NewServer(&control.Collector{Index: fake}).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	out.Reset()
	if err := runIndex(ctx, []string{"status", "--config", p}, &out); err != nil || strings.Contains(out.String(), "offline") || !strings.Contains(out.String(), "worker: idle") {
		t.Fatalf("online status: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runIndex(ctx, []string{"add", "/notes", "--include", "*.md,*.txt", "--max-file-size", "2MiB", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if len(fake.added) != 1 || fake.added[0].Path != "/notes" || fake.added[0].Source != "ui" || len(fake.added[0].Include) != 2 || fake.added[0].MaxFileSize != 2<<20 {
		t.Fatalf("add: %+v", fake.added)
	}
	if !strings.Contains(out.String(), "/notes") || !strings.Contains(out.String(), "2.0 MiB") {
		t.Fatalf("add did not print the rule table: %s", out.String())
	}
	out.Reset()
	if err := runIndex(ctx, []string{"rm", "/notes", "--confirm", "--config", p}, &out); err != nil || len(fake.removed) != 1 || fake.removed[0] != "/notes" {
		t.Fatalf("rm: %v %v", err, fake.removed)
	}
	if err := runIndex(ctx, []string{"rebuild", "--confirm", "--config", p}, &out); err != nil || fake.rebuilt != 1 {
		t.Fatalf("rebuild: %v %d", err, fake.rebuilt)
	}
	out.Reset()
	if err := runIndex(ctx, []string{"retry", "/work", "--config", p}, &out); err != nil || len(fake.retried) != 1 || fake.retried[0] != "/work" || !strings.Contains(out.String(), "requeued 3") {
		t.Fatalf("retry: %v %v %s", err, fake.retried, out.String())
	}
	out.Reset()
	if err := runIndex(ctx, []string{"search", "quarterly", "report", "--path", "/work", "--limit", "5", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if len(fake.queries) != 1 || fake.queries[0].Query != "quarterly report" || fake.queries[0].Roots[0] != "/work" || fake.queries[0].TopK != 5 {
		t.Fatalf("online search query: %+v", fake.queries)
	}
	if s := out.String(); strings.Contains(s, "offline") || !strings.Contains(s, "[stale]") {
		t.Fatalf("online search: %s", s)
	}
}
