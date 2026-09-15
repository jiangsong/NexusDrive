package control

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/embed"
	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
)

func embeddingDoctor(fi *fakeIndex, cfg config.IndexEmbedding) *Doctor {
	c := &config.Config{}
	c.Index.Embedding = cfg
	return &Doctor{Index: fi, Config: func() *config.Config { return c }}
}

// TestDoctorFlagsDimensionMismatch: vectors of one size in index_meta and
// an endpoint answering another cannot be compared; the doctor says so as
// a failure with the two ways out, and stays quiet when they agree or
// when nothing is configured.
func TestDoctorFlagsDimensionMismatch(t *testing.T) {
	fi := newFakeIndex()
	fi.embedder = embed.NewFake(16)
	fi.recordedModel, fi.recordedDim = "fake", 8
	fi.status.Embedding = index.EmbeddingStatus{Provider: "ollama", Model: "fake", Dim: 16, Healthy: true, Host: "127.0.0.1"}
	cfg := config.IndexEmbedding{Provider: "ollama", Model: "fake", BaseURL: "http://127.0.0.1:11434"}
	checks := embeddingDoctor(fi, cfg).Run(context.Background())
	c, ok := checkByName(checks, "index_embedding")
	if !ok || c.Level != LevelFail || !strings.Contains(c.Detail, "16") || !strings.Contains(c.Detail, "8") {
		t.Fatalf("mismatch: %+v", c)
	}
	if !strings.Contains(c.Fix, "cloudfs index rebuild") {
		t.Fatalf("fix names neither way out: %+v", c)
	}
	if zh := c.Localize(i18n.ZH); zh.Detail == c.Detail || !strings.Contains(zh.Detail, "16") {
		t.Fatalf("did not localize: %+v", zh)
	}

	fi.recordedDim = 16
	checks = embeddingDoctor(fi, cfg).Run(context.Background())
	if c, ok := checkByName(checks, "index_embedding"); !ok || c.Level != LevelOK || !strings.Contains(c.Detail, "fake") {
		t.Fatalf("matching: %+v", c)
	}

	// An unhealthy endpoint is a warning carrying its own last error.
	fi.status.Embedding.Healthy, fi.status.Embedding.LastError = false, "connection refused by 127.0.0.1:11434"
	checks = embeddingDoctor(fi, cfg).Run(context.Background())
	if c, ok := checkByName(checks, "index_embedding"); !ok || c.Level != LevelWarn || !strings.Contains(c.Detail, "connection refused") {
		t.Fatalf("unhealthy: %+v", c)
	}

	// No provider: ok, "not configured", and no probe of anything.
	none := newFakeIndex()
	none.status.Embedding = index.EmbeddingStatus{Provider: "none"}
	checks = embeddingDoctor(none, config.IndexEmbedding{Provider: "none"}).Run(context.Background())
	if c, ok := checkByName(checks, "index_embedding"); !ok || c.Level != LevelOK || !strings.Contains(c.Detail, "not configured") {
		t.Fatalf("none: %+v", c)
	}
	if _, ok := checkByName((&Doctor{}).Run(context.Background()), "index_embedding"); ok {
		t.Fatal("index_embedding reported without an index")
	}
}

// TestDoctorNotesARemoteEndpoint: content leaving the machine is worth a
// line of its own, at warning level, naming the host; a local endpoint
// gets no such line.
func TestDoctorNotesARemoteEndpoint(t *testing.T) {
	fi := newFakeIndex()
	fi.embedder = embed.NewFake(8)
	fi.recordedModel, fi.recordedDim = "fake", 8
	fi.status.Embedding = index.EmbeddingStatus{Provider: "openai", Model: "fake", Dim: 8, Healthy: true, Remote: true, Host: "api.openai.com"}
	cfg := config.IndexEmbedding{Provider: "openai", Model: "fake", BaseURL: "https://api.openai.com/v1", APIKey: "keyring:index.embedding", AllowRemote: true}
	checks := embeddingDoctor(fi, cfg).Run(context.Background())
	c, ok := checkByName(checks, "index_embedding_remote")
	if !ok || c.Level != LevelWarn || !strings.Contains(c.Detail, "api.openai.com") {
		t.Fatalf("remote: %+v", c)
	}
	if strings.Contains(c.Detail, "keyring:") || strings.Contains(c.Fix, "keyring:") {
		t.Fatalf("doctor leaks the key reference: %+v", c)
	}
	if zh := c.Localize(i18n.ZH); !strings.Contains(zh.Detail, "api.openai.com") || zh.Detail == c.Detail {
		t.Fatalf("did not localize: %+v", zh)
	}

	fi.status.Embedding.Remote, fi.status.Embedding.Host = false, "127.0.0.1"
	checks = embeddingDoctor(fi, cfg).Run(context.Background())
	if _, ok := checkByName(checks, "index_embedding_remote"); ok {
		t.Fatal("a local endpoint was reported as remote")
	}
}
