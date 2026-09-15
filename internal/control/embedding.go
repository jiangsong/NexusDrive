package control

import (
	"context"
	"net/http"
	"time"

	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
)

// The embedding routes (docs/agent-roadmap.md §3.10, TODO.md T-39) serve
// the console's endpoint panel. GET /index/embedding is the status the
// indexer already keeps plus what only the configuration knows — whether
// a key is set — and a cost estimate; it never spends a request. POST
// /index/embedding/check spends exactly one, so the panel can prove the
// endpoint answers before the worker is left to it. Neither route carries
// the key, the reference that resolves to it, the base_url (which may
// hold credentials) or the header the key travels in: the panel gets the
// host and a boolean, and the key is set with `cloudfs index auth`.

// EmbeddingResponse is GET /index/embedding. Enabled is false without an
// index or with provider none; the other fields then describe what there
// is (a provider of "none", the month's counters) so the panel can say
// "not configured" rather than "unknown".
type EmbeddingResponse struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Dim      int    `json:"dim"`
	// BaseHost is the endpoint's host name, without scheme, port, path or
	// userinfo: what the "content is sent to" banner names.
	BaseHost string `json:"base_host,omitempty"`
	// Remote says chunk text leaves this machine.
	Remote           bool      `json:"remote"`
	Healthy          bool      `json:"healthy"`
	LastError        string    `json:"last_error,omitempty"`
	BreakerOpenUntil time.Time `json:"breaker_open_until,omitzero"`
	Embedded         int       `json:"embedded"`
	Pending          int       `json:"pending"`
	Vectors          int       `json:"vectors"`
	MaxChunks        int       `json:"max_chunks"`
	Capped           bool      `json:"capped"`
	// APIKeyConfigured says index.embedding.api_key names a stored secret;
	// the secret itself is never served.
	APIKeyConfigured bool              `json:"api_key_configured"`
	CharsThisMonth   int64             `json:"chars_this_month"`
	Estimate         EmbeddingEstimate `json:"estimate"`
}

// EmbeddingEstimate is the cost side of the panel. It is an estimate and
// says so: the daemon knows how many characters it sent and how many
// chunks it holds, not the endpoint's tokenizer or price list.
type EmbeddingEstimate struct {
	// Chars is what was sent this calendar month; TokensApprox is that at
	// the usual four characters per token.
	Chars        int64 `json:"chars"`
	TokensApprox int64 `json:"tokens_approx"`
	// CorpusTokensApprox is what embedding every chunk once would cost in
	// tokens, at ~300 per chunk, for the formula below.
	CorpusTokensApprox int64  `json:"corpus_tokens_approx"`
	Formula            string `json:"formula"`
	Note               string `json:"note"`
}

// EmbeddingCheckResponse is POST /index/embedding/check: the outcome of
// one request. A failure is reported here, not as an HTTP error, so the
// panel can show it next to the button.
type EmbeddingCheckResponse struct {
	OK        bool   `json:"ok"`
	Dim       int    `json:"dim"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

const (
	// embeddingCheckText is the one text the check embeds.
	embeddingCheckText = "cloudfs"
	// embeddingCheckTimeout bounds the check independently of the
	// endpoint's own timeout, so a hung endpoint does not hold the panel.
	embeddingCheckTimeout = 30 * time.Second
	// tokensPerChunkApprox and charsPerTokenApprox are the rules of thumb
	// the estimate is built on.
	tokensPerChunkApprox = 300
	charsPerTokenApprox  = 4
	embeddingFormula     = "chunks × ~300 tokens × unit price"
)

// embeddingResponseOf builds the GET body from the index status and the
// configuration's knowledge of the key.
func embeddingResponseOf(st index.Status, keyConfigured bool, lang i18n.Lang) EmbeddingResponse {
	e := st.Embedding
	provider := e.Provider
	if provider == "" {
		provider = "none"
	}
	return EmbeddingResponse{
		Enabled: provider != "none", Provider: provider, Model: e.Model, Dim: e.Dim,
		BaseHost: e.Host, Remote: e.Remote,
		Healthy: e.Healthy, LastError: e.LastError, BreakerOpenUntil: e.BreakerOpenUntil,
		Embedded: e.Embedded, Pending: e.Pending, Vectors: st.Vectors, MaxChunks: st.MaxChunks, Capped: e.Capped,
		APIKeyConfigured: keyConfigured, CharsThisMonth: e.CharsThisMonth,
		Estimate: EmbeddingEstimate{
			Chars: e.CharsThisMonth, TokensApprox: e.CharsThisMonth / charsPerTokenApprox,
			CorpusTokensApprox: int64(st.ChunksTotal) * tokensPerChunkApprox,
			Formula:            embeddingFormula,
			Note:               i18n.T(lang, "embedding.estimate.note"),
		},
	}
}

// apiKeyConfigured reads the one thing the status does not know: whether
// the configuration names a key. The reference itself stays here.
func (s *Server) apiKeyConfigured() bool {
	cfg := s.collector.ConfigView()
	return cfg != nil && cfg.Index.Embedding.APIKey != ""
}

// GET /index/embedding
func (s *Server) indexEmbedding(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.collector.Index == nil {
		writeJSON(w, IndexStatusBrief{Enabled: false})
		return
	}
	st, err := s.collector.Index.Status(r.Context(), "")
	if err != nil {
		indexError(w, r, err)
		return
	}
	writeJSON(w, embeddingResponseOf(st, s.apiKeyConfigured(), LangFrom(r)))
}

// POST /index/embedding/check
//
// No confirm gate: the console explains the one request before the
// button, and a check that is refused for lack of a flag would only
// teach the page to send the flag.
func (s *Server) indexEmbeddingCheck(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	e := x.Embedder()
	if e == nil {
		httpErrorT(w, r, http.StatusConflict, "err.embedding_none")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), embeddingCheckTimeout)
	defer cancel()
	start := time.Now()
	vecs, err := e.Embed(ctx, []string{embeddingCheckText})
	res := EmbeddingCheckResponse{LatencyMS: time.Since(start).Milliseconds()}
	switch {
	case err != nil:
		res.Error = err.Error()
	case len(vecs) != 1 || len(vecs[0]) == 0:
		res.Error = i18n.T(LangFrom(r), "err.embedding_shape", len(vecs))
	default:
		res.OK, res.Dim = true, len(vecs[0])
	}
	writeJSON(w, res)
}
