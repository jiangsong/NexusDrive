package embed

import (
	"context"
	"strings"
)

// ollamaWire speaks POST {base_url}/api/embed, ollama's batch endpoint. The
// older single-text /api/embeddings is deliberately not used: one request
// per chunk would multiply the call count by Batch.
//
// UNVERIFIED: checked against ollama's API documentation only. Verify on a
// running ollama (0.3+) that /api/embed accepts a string array `input` and
// answers `embeddings` as an array of arrays in input order, and that a
// model which is not pulled yields a 404 rather than a 500 (the latter would
// count toward the breaker).
type ollamaWire struct{}

type ollamaRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

func (ollamaWire) embed(ctx context.Context, c *Client, texts []string) ([][]float32, error) {
	var resp ollamaResponse
	if err := c.post(ctx, strings.TrimRight(c.cfg.BaseURL, "/")+"/api/embed", ollamaRequest{Model: c.cfg.Model, Input: texts}, &resp); err != nil {
		return nil, err
	}
	return resp.Embeddings, nil
}
