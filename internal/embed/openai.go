package embed

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// openaiWire speaks POST {base_url}/embeddings, the OpenAI shape that
// vLLM, LM Studio, llama.cpp's server, text-embeddings-inference and most
// hosted providers also accept.
//
// UNVERIFIED: checked against the published OpenAI API reference only, not a
// live account. Verify with a real key that text-embedding-3-small honours
// `dimensions` and that `encoding_format: "float"` is accepted, and against
// at least one compatible server (vLLM / TEI) that ignores or rejects
// `dimensions` — the dimension check in Client.check names that case.
type openaiWire struct{}

type openaiRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	// Dimensions is omitted when zero; only the text-embedding-3 family
	// accepts it.
	Dimensions int `json:"dimensions,omitempty"`
}

type openaiResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

func (openaiWire) embed(ctx context.Context, c *Client, texts []string) ([][]float32, error) {
	req := openaiRequest{Model: c.cfg.Model, Input: texts, EncodingFormat: "float", Dimensions: c.cfg.Dimensions}
	var resp openaiResponse
	if err := c.post(ctx, strings.TrimRight(c.cfg.BaseURL, "/")+"/embeddings", req, &resp); err != nil {
		return nil, err
	}
	// The reference says data is ordered by index; a compatible server may
	// not bother, and a misordered vector is silent corruption, so sort.
	sort.SliceStable(resp.Data, func(i, j int) bool { return resp.Data[i].Index < resp.Data[j].Index })
	out := make([][]float32, 0, len(resp.Data))
	for i, d := range resp.Data {
		if d.Index != i {
			return nil, fmt.Errorf("embed: openai response index %d at position %d", d.Index, i)
		}
		out = append(out, d.Embedding)
	}
	return out, nil
}
