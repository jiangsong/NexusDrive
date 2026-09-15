package config

import (
	"strings"
	"testing"
	"time"
)

// A configuration that names an endpoint outside this machine must say so
// with allow_remote: true, because every indexed chunk is sent to that host.
// The OpenAI default base URL is the obvious case; a private-network or
// loopback endpoint needs no opt-in.
func TestRemoteEndpointNeedsAllowRemote(t *testing.T) {
	remote := example + `
index:
  embedding:
    provider: openai
    base_url: https://api.openai.com/v1
    model: text-embedding-3-small
    api_key: keyring:index.embedding
`
	_, err := Parse([]byte(remote))
	if err == nil || !strings.Contains(err.Error(), "allow_remote") {
		t.Fatalf("remote endpoint without allow_remote accepted: err = %v", err)
	}
	cfg, err := Parse([]byte(remote + "    allow_remote: true\n"))
	if err != nil {
		t.Fatalf("allow_remote: true still rejected: %v", err)
	}
	if !cfg.Index.Embedding.RemoteEndpoint() || cfg.Index.Embedding.EndpointHost() != "api.openai.com" {
		t.Fatalf("remote = %v host = %q", cfg.Index.Embedding.RemoteEndpoint(), cfg.Index.Embedding.EndpointHost())
	}
	// The default OpenAI base URL is remote too, so leaving base_url out is
	// not a way around the gate.
	if _, err := Parse([]byte(example + "\nindex:\n  embedding:\n    provider: openai\n    model: m\n    api_key: keyring:k\n")); err == nil || !strings.Contains(err.Error(), "allow_remote") {
		t.Fatalf("default openai base_url without allow_remote accepted: err = %v", err)
	}
	for name, base := range map[string]string{
		"loopback":   "http://127.0.0.1:11434",
		"localhost":  "http://localhost:11434",
		"ipv6 lo":    "http://[::1]:11434",
		"rfc1918":    "http://192.168.1.20:11434",
		"rfc1918 10": "http://10.0.0.5:8080/v1",
		"link-local": "http://169.254.10.10:11434",
		"mdns":       "http://gpu-box.local:11434",
	} {
		cfg, err := Parse([]byte(example + "\nindex:\n  embedding:\n    provider: ollama\n    model: nomic-embed-text\n    base_url: " + base + "\n"))
		if err != nil {
			t.Errorf("%s (%s) rejected without allow_remote: %v", name, base, err)
			continue
		}
		if cfg.Index.Embedding.RemoteEndpoint() {
			t.Errorf("%s (%s) reported as remote", name, base)
		}
	}
	for name, base := range map[string]string{
		"public ip": "http://8.8.8.8:11434",
		"hostname":  "http://embeddings.example.com",
		"cgnat":     "http://100.64.0.1:11434",
	} {
		if _, err := Parse([]byte(example + "\nindex:\n  embedding:\n    provider: ollama\n    model: m\n    base_url: " + base + "\n")); err == nil {
			t.Errorf("%s (%s) accepted without allow_remote", name, base)
		}
	}
	// A URL the client could not use is rejected outright.
	for name, base := range map[string]string{
		"no scheme": "api.openai.com/v1",
		"ftp":       "ftp://127.0.0.1/v1",
		"no host":   "http:///v1",
	} {
		if _, err := Parse([]byte(example + "\nindex:\n  embedding:\n    provider: ollama\n    model: m\n    allow_remote: true\n    base_url: " + base + "\n")); err == nil {
			t.Errorf("%s (%q) accepted", name, base)
		}
	}
}

// The key never lives in YAML: openai requires api_key and it must be a
// keyring:/secretfile: reference written by `cloudfs index auth`. ollama
// has no key by default, but if one is given it obeys the same rule.
func TestAPIKeyMustBeAReference(t *testing.T) {
	openai := example + "\nindex:\n  embedding:\n    provider: openai\n    model: text-embedding-3-small\n    base_url: http://127.0.0.1:8000/v1\n"
	if _, err := Parse([]byte(openai)); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("openai without api_key accepted: err = %v", err)
	}
	if _, err := Parse([]byte(openai + "    api_key: sk-plaintext\n")); err == nil || !strings.Contains(err.Error(), "keyring:") {
		t.Fatalf("plaintext api_key accepted: err = %v", err)
	}
	for _, ref := range []string{"keyring:index.embedding", "secretfile:index.embedding"} {
		cfg, err := Parse([]byte(openai + "    api_key: " + ref + "\n"))
		if err != nil {
			t.Fatalf("%s rejected: %v", ref, err)
		}
		if cfg.Index.Embedding.APIKey != ref {
			t.Fatalf("api_key = %q, want %q", cfg.Index.Embedding.APIKey, ref)
		}
	}
	if _, err := Parse([]byte(example + "\nindex:\n  embedding:\n    provider: ollama\n    model: m\n    api_key: plaintext\n")); err == nil {
		t.Fatal("plaintext api_key accepted for ollama")
	}
	if !IsSecretField("api_key") {
		t.Fatal("api_key is not a secret field")
	}
}

// An empty block means no embedding at all, and a minimal ollama block gets
// working limits: batch 64, two workers, 4 requests per second, 30 s per
// request, int8 vectors, the local ollama address, and 200 000 chunks.
func TestEmbeddingDefaults(t *testing.T) {
	cfg, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Index.Embedding
	if e.Provider != "none" {
		t.Fatalf("provider = %q, want none", e.Provider)
	}
	if cfg.Index.MaxChunks != 200000 {
		t.Fatalf("max_chunks = %d, want 200000", cfg.Index.MaxChunks)
	}
	cfg, err = Parse([]byte(example + "\nindex:\n  embedding:\n    provider: ollama\n    model: nomic-embed-text\n"))
	if err != nil {
		t.Fatal(err)
	}
	e = cfg.Index.Embedding
	if e.BaseURL != "http://127.0.0.1:11434" || e.Batch != 64 || e.Concurrency != 2 || e.QPS != 4 || e.Timeout != 30*time.Second || e.Quantize != "int8" || e.Proxy != "" || e.AllowRemote {
		t.Fatalf("defaults not applied: %+v", e)
	}
	if e.Dimensions != 0 {
		t.Fatalf("ollama got dimensions %d; the parameter is openai-only", e.Dimensions)
	}
	cfg, err = Parse([]byte(example + "\nindex:\n  embedding:\n    provider: openai\n    model: m\n    api_key: keyring:k\n    base_url: http://127.0.0.1:8000/v1\n    batch: 16\n    concurrency: 1\n    qps: 0.5\n    timeout: 5s\n    quantize: none\n    proxy: proxy\n  max_chunks: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	e = cfg.Index.Embedding
	if e.Dimensions != 512 || e.Batch != 16 || e.Concurrency != 1 || e.QPS != 0.5 || e.Timeout != 5*time.Second || e.Quantize != "none" || e.Proxy != "proxy" || cfg.Index.MaxChunks != 10 {
		t.Fatalf("explicit values lost: %+v max_chunks=%d", e, cfg.Index.MaxChunks)
	}
	for name, yaml := range map[string]string{
		"unknown provider":  "index:\n  embedding:\n    provider: cohere\n",
		"no model":          "index:\n  embedding:\n    provider: ollama\n",
		"bad quantize":      "index:\n  embedding:\n    provider: ollama\n    model: m\n    quantize: fp16\n",
		"zero qps":          "index:\n  embedding:\n    provider: ollama\n    model: m\n    qps: -1\n",
		"negative batch":    "index:\n  embedding:\n    provider: ollama\n    model: m\n    batch: -2\n",
		"unknown proxy":     "index:\n  embedding:\n    provider: ollama\n    model: m\n    proxy: nope\n",
		"ollama dimensions": "index:\n  embedding:\n    provider: ollama\n    model: m\n    dimensions: 256\n",
		"negative chunks":   "index:\n  max_chunks: -1\n",
		"unknown key":       "index:\n  embedding:\n    provider: ollama\n    model: m\n    apikey: x\n",
	} {
		if _, err := Parse([]byte(example + "\n" + yaml)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
