package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/embed"
)

// indexAuthConfig writes a configuration whose secrets go to files, so the
// test can read the reference back without a system keyring.
func indexAuthConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(indexAuthConfigBody(t, dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// indexAuthConfigBody is the YAML indexAuthConfig writes, with a provider
// of none the tests flip to openai.
func indexAuthConfigBody(t *testing.T, dir string) string {
	t.Helper()
	sockdir, err := os.MkdirTemp("", "cfs-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockdir) })
	return "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nsecrets: {backend: file}\n" +
		"control: {socket: " + filepath.Join(sockdir, "c.sock") + "}\n" +
		"remotes:\n  ali: {type: unregistered-test-provider}\n" +
		"mounts:\n  - path: " + filepath.Join(dir, "mount") + "\n    layout: {\"/\": {remote: ali}}\n" +
		"index:\n  enabled: true\n  embedding:\n    provider: none\n    model: text-embedding-3-small\n"
}

// TestIndexAuthWritesAReference: the key goes into the secret store and
// the configuration gets a reference to it; the file never holds the key,
// and the key reads back through the store from that reference.
func TestIndexAuthWritesAReference(t *testing.T) {
	p := indexAuthConfig(t)
	var out bytes.Buffer
	in := strings.NewReader("sk-test-from-stdin\n")
	if err := runIndexWith(context.Background(), []string{"auth", "--config", p}, in, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-test-from-stdin") {
		t.Fatalf("the key landed in the config file:\n%s", b)
	}
	if !strings.Contains(string(b), "api_key: secretfile:index.embedding") {
		t.Fatalf("no reference written:\n%s", b)
	}
	if !strings.Contains(string(b), "# keep") && !strings.Contains(string(b), "model: text-embedding-3-small") {
		t.Fatalf("the rest of the block was not preserved:\n%s", b)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.NewSecretStore(cfg).Get(cfg.Index.Embedding.APIKey)
	if err != nil || got != "sk-test-from-stdin" {
		t.Fatalf("read back %q %v", got, err)
	}
	if strings.Contains(out.String(), "sk-test-from-stdin") {
		t.Fatalf("the key was echoed: %s", out.String())
	}

	// --key-file reads the file instead of stdin, trimming the newline an
	// editor leaves, and replaces the earlier key under the same reference.
	kf := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(kf, []byte("sk-test-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runIndexWith(context.Background(), []string{"auth", "--key-file", kf, "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := config.NewSecretStore(cfg).Get(cfg.Index.Embedding.APIKey); err != nil || got != "sk-test-from-file" {
		t.Fatalf("read back after --key-file %q %v", got, err)
	}

	// An empty key is refused before anything is written.
	if err := runIndexWith(context.Background(), []string{"auth", "--config", p}, strings.NewReader("\n"), &out); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty key: %v", err)
	}

	// A block that already says openai but has no key yet is exactly what
	// the command exists to fix; it must not fail on loading that file.
	openai := strings.Replace(indexAuthConfigBody(t, filepath.Dir(p)), "provider: none", "provider: openai\n    allow_remote: true", 1)
	if err := os.WriteFile(p, []byte(openai), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(p); err == nil {
		t.Fatal("the openai block without a key was accepted; the test premise is gone")
	}
	if err := runIndexWith(context.Background(), []string{"auth", "--config", p}, strings.NewReader("sk-after-provider\n"), &out); err != nil {
		t.Fatalf("auth on an openai block without a key: %v", err)
	}
	cfg, err = config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := config.NewSecretStore(cfg).Get(cfg.Index.Embedding.APIKey); err != nil || got != "sk-after-provider" || cfg.Index.Embedding.Provider != "openai" {
		t.Fatalf("read back %q %v provider %s", got, err, cfg.Index.Embedding.Provider)
	}
}

// TestIndexAuthRefusesAKeyOnTheCommandLine: a key typed as an argument
// lands in the shell history; the command refuses it and says where the
// key may come from instead. Nothing is written.
func TestIndexAuthRefusesAKeyOnTheCommandLine(t *testing.T) {
	p := indexAuthConfig(t)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, args := range [][]string{{"auth", "sk-on-the-command-line"}, {"auth", "--key", "sk-on-the-command-line"}, {"auth", "--key=sk-on-the-command-line"}} {
		err := runIndexWith(context.Background(), append(args, "--config", p), strings.NewReader("sk-from-stdin\n"), &out)
		if err == nil || !strings.Contains(err.Error(), "stdin") || !strings.Contains(err.Error(), "--key-file") {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(err.Error(), "sk-on-the-command-line") {
			t.Fatalf("%v: the error repeats the key: %v", args, err)
		}
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the config changed:\n%s", after)
	}
}

// TestIndexEmbeddingPrintsTheStatusTable: `cloudfs index embedding` shows
// the endpoint the way the console panel does and needs the daemon for
// it; --check spends exactly one request on the daemon's endpoint.
func TestIndexEmbeddingPrintsTheStatusTable(t *testing.T) {
	p := indexAuthConfig(t)
	ctx := context.Background()
	var out bytes.Buffer
	err := runIndexWith(ctx, []string{"embedding", "--config", p}, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "requires the running daemon") {
		t.Fatalf("offline: %v", err)
	}
	if err := runIndexWith(ctx, []string{"embedding", "extra", "--config", p}, strings.NewReader(""), &out); err == nil {
		t.Fatal("an argument was accepted")
	}

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	fake := &cliIndex{embedder: embed.NewFake(8)}
	srv, err := control.NewServer(&control.Collector{Index: fake}).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	out.Reset()
	if err := runIndexWith(ctx, []string{"embedding", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "ollama") || !strings.Contains(s, "127.0.0.1") || !strings.Contains(s, "healthy") || !strings.Contains(s, "estimate") {
		t.Fatalf("status table: %s", s)
	}
	if fake.embedder.Calls() != 0 {
		t.Fatalf("printing the status called the endpoint %d times", fake.embedder.Calls())
	}
	out.Reset()
	if err := runIndexWith(ctx, []string{"embedding", "--check", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if fake.embedder.Calls() != 1 || !strings.Contains(out.String(), "check: ok") || !strings.Contains(out.String(), "8 dimensions") {
		t.Fatalf("check: calls %d, %s", fake.embedder.Calls(), out.String())
	}
	out.Reset()
	if err := runIndexWith(ctx, []string{"embedding", "--json", "--config", p}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var res control.EmbeddingResponse
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || !res.Enabled || res.Provider != "ollama" || res.Dim != 8 {
		t.Fatalf("json: %s %v", out.String(), err)
	}
}
