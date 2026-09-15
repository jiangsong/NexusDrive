package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// IndexEmbedding configures the embedding endpoint the indexer sends chunk
// text to for semantic search (docs/agent-roadmap.md §3.5). The default
// provider is none: nothing leaves the machine unless a block names an
// endpoint, and an endpoint outside loopback, the private ranges, link-local
// or a .local name additionally needs allow_remote: true, because every
// indexed chunk is posted to it.
type IndexEmbedding struct {
	// Provider is none (default), openai (any /embeddings-compatible server)
	// or ollama (/api/embed).
	Provider string `yaml:"provider"`
	// BaseURL is the API root the paths are appended to. Empty means the
	// provider's own default: https://api.openai.com/v1 or
	// http://127.0.0.1:11434.
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	// APIKey is a keyring:/secretfile: reference written by
	// `cloudfs index auth`; a literal key in YAML is rejected.
	APIKey string `yaml:"api_key"`
	// Dimensions is the openai `dimensions` request parameter (default 512).
	// ollama has no such parameter, so it must stay zero there.
	Dimensions int `yaml:"dimensions"`
	// Batch is how many texts one request carries.
	Batch int `yaml:"batch"`
	// Concurrency bounds requests in flight.
	Concurrency int `yaml:"concurrency"`
	// QPS bounds requests per second; 429 responses halve it adaptively.
	QPS float64 `yaml:"qps"`
	// Timeout bounds one request.
	Timeout time.Duration `yaml:"timeout"`
	// Proxy names an outbound or group from proxy:, like Remote.Proxy.
	Proxy string `yaml:"proxy"`
	// AllowRemote acknowledges that chunk text is sent to a host outside
	// this machine and its private networks.
	AllowRemote bool `yaml:"allow_remote"`
	// Quantize is int8 (default) or none, the storage form of the vectors.
	Quantize string `yaml:"quantize"`
}

// Built-in embedding limits, applied where the YAML is silent.
const (
	defaultEmbeddingBatch       = 64
	defaultEmbeddingConcurrency = 2
	defaultEmbeddingQPS         = 4.0
	defaultEmbeddingTimeout     = 30 * time.Second
	defaultEmbeddingQuantize    = "int8"
	defaultOpenAIDimensions     = 512
	defaultOpenAIBaseURL        = "https://api.openai.com/v1"
	defaultOllamaBaseURL        = "http://127.0.0.1:11434"
	defaultIndexMaxChunks       = 200000
)

// Enabled reports whether a provider is configured at all.
func (e IndexEmbedding) Enabled() bool { return e.Provider != "" && e.Provider != "none" }

// EndpointHost is the host (without port) of BaseURL, or "" when the URL
// does not parse. It is what the console names in its "content is sent to
// <host>" banner.
func (e IndexEmbedding) EndpointHost() string {
	u, err := url.Parse(e.BaseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// RemoteEndpoint reports whether BaseURL points outside this machine and its
// private networks: anything that is not loopback, RFC 1918 / ULA, link-local,
// "localhost" or a .local / .localhost name. A hostname that is not an IP
// literal cannot be checked without a DNS lookup, so it counts as remote —
// the gate errs on the side of asking.
func (e IndexEmbedding) RemoteEndpoint() bool {
	host := strings.ToLower(e.EndpointHost())
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

// validate fills the defaults and rejects a block the client could not act
// on. Proxy names are checked by Config.Validate once the outbound table is
// known.
func (e *IndexEmbedding) validate() error {
	if e.Provider == "" {
		e.Provider = "none"
	}
	switch e.Provider {
	case "none":
		if e.APIKey != "" && !secretReference(e.APIKey) {
			return fmt.Errorf("config: index.embedding.api_key must be a keyring: or secretfile: reference")
		}
		return nil
	case "openai":
		if e.BaseURL == "" {
			e.BaseURL = defaultOpenAIBaseURL
		}
		if e.Dimensions == 0 {
			e.Dimensions = defaultOpenAIDimensions
		}
		if e.APIKey == "" {
			return fmt.Errorf("config: index.embedding.api_key is required for provider openai; run cloudfs index auth")
		}
	case "ollama":
		if e.BaseURL == "" {
			e.BaseURL = defaultOllamaBaseURL
		}
		if e.Dimensions != 0 {
			return fmt.Errorf("config: index.embedding.dimensions applies to provider openai only; ollama models have a fixed size")
		}
	default:
		return fmt.Errorf("config: index.embedding.provider must be none, openai, or ollama, got %q", e.Provider)
	}
	if e.Model == "" {
		return fmt.Errorf("config: index.embedding.model is required for provider %s", e.Provider)
	}
	if e.APIKey != "" && !secretReference(e.APIKey) {
		return fmt.Errorf("config: index.embedding.api_key must be a keyring: or secretfile: reference, not the key itself; run cloudfs index auth")
	}
	u, err := url.Parse(e.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("config: index.embedding.base_url must be an http(s) URL with a host, got %q", e.BaseURL)
	}
	if e.Dimensions < 0 {
		return fmt.Errorf("config: index.embedding.dimensions must not be negative, got %d", e.Dimensions)
	}
	if e.Batch == 0 {
		e.Batch = defaultEmbeddingBatch
	}
	if e.Batch < 0 {
		return fmt.Errorf("config: index.embedding.batch must be positive, got %d", e.Batch)
	}
	if e.Concurrency == 0 {
		e.Concurrency = defaultEmbeddingConcurrency
	}
	if e.Concurrency < 0 {
		return fmt.Errorf("config: index.embedding.concurrency must be positive, got %d", e.Concurrency)
	}
	if e.QPS == 0 {
		e.QPS = defaultEmbeddingQPS
	}
	if e.QPS < 0 {
		return fmt.Errorf("config: index.embedding.qps must be positive, got %g", e.QPS)
	}
	if e.Timeout == 0 {
		e.Timeout = defaultEmbeddingTimeout
	}
	if e.Timeout < 0 {
		return fmt.Errorf("config: index.embedding.timeout must be positive, got %s", e.Timeout)
	}
	if e.Quantize == "" {
		e.Quantize = defaultEmbeddingQuantize
	}
	if e.Quantize != "int8" && e.Quantize != "none" {
		return fmt.Errorf("config: index.embedding.quantize must be int8 or none, got %q", e.Quantize)
	}
	if e.RemoteEndpoint() && !e.AllowRemote {
		return fmt.Errorf("config: index.embedding.base_url %q is outside this machine; indexed file content would be sent to %s — set allow_remote: true to accept that", e.BaseURL, e.EndpointHost())
	}
	return nil
}
