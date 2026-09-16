package mcpsrv

import (
	"unicode/utf8"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Token budget (docs/agent-first-design.md §5.3, TODO.md T-47). The byte
// limits bound what a tool reads; the token budget bounds what the model
// pays, which for Chinese text is about four times the bytes suggest. The
// middleware only measures and records (the SDK has serialised the output
// by then, so cutting there would leave structured and text content
// disagreeing); each tool cuts its own payload before returning and says
// truncated_by: tokens, keeping its cursor or offset semantics.

// truncatedByTokens is the value of truncated_by when the token budget,
// not a byte or count limit, cut a result.
const truncatedByTokens = "tokens"

// tokenFraming is what the JSON around a payload costs: field names, the
// summary line, the structured copy of small fields.
const tokenFraming = 256

// tokenBudget is what one tool may spend on its payload; 0 means no
// budget.
func (s *Server) tokenBudget() int {
	if s.opt.Limits.MaxTokens <= 0 {
		return 0
	}
	if b := s.opt.Limits.MaxTokens - tokenFraming; b > 0 {
		return b
	}
	return 1
}

// tokenCounter accumulates the estimate rune by rune with the same rule
// as agent.EstimateTokens, so a prefix can be cut at the budget without
// re-estimating from the start each time.
type tokenCounter struct{ cjk, other int }

func (c *tokenCounter) add(r rune, size int) {
	if agent.IsCJK(r) {
		c.cjk++
	} else {
		c.other += size
	}
}

func (c tokenCounter) tokens() int {
	n := c.cjk + (c.other+3)/4
	return n + n/10
}

// cutHead returns the longest prefix of s that fits budget tokens, on a
// rune boundary, and whether anything was cut. budget <= 0 keeps s.
func cutHead(s string, budget int) (string, bool) {
	if budget <= 0 || agent.EstimateTokens(s) <= budget {
		return s, false
	}
	var c tokenCounter
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		c.add(r, size)
		if c.tokens() > budget {
			break
		}
		i += size
	}
	return s[:i], true
}

// cutTail is cutHead from the end: the longest suffix within budget.
func cutTail(s string, budget int) (string, bool) {
	if budget <= 0 || agent.EstimateTokens(s) <= budget {
		return s, false
	}
	var c tokenCounter
	i := len(s)
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		c.add(r, size)
		if c.tokens() > budget {
			break
		}
		i -= size
	}
	return s[i:], true
}

// cutItems keeps the longest prefix of n items whose estimates sum to at
// most budget, always at least one so a result never comes back empty
// because a single item is large. estimate returns item i's tokens.
func cutItems(n, budget int, estimate func(i int) int) (keep int, cut bool) {
	if budget <= 0 {
		return n, false
	}
	total := 0
	for i := 0; i < n; i++ {
		total += estimate(i)
		if total > budget && i > 0 {
			return i, true
		}
	}
	return n, false
}

// resultTokens is what a result cost the client's context: the same
// blocks contentBytes counts, estimated as tokens.
func resultTokens(r *mcp.CallToolResult) int64 {
	var n int64
	for _, c := range r.Content {
		switch x := c.(type) {
		case *mcp.TextContent:
			n += int64(agent.EstimateTokens(x.Text))
		case *mcp.ImageContent:
			n += int64(len(x.Data) / 4)
		case *mcp.AudioContent:
			n += int64(len(x.Data) / 4)
		case *mcp.EmbeddedResource:
			if x.Resource != nil {
				n += int64(agent.EstimateTokens(x.Resource.Text)) + int64(len(x.Resource.Blob)/4)
			}
		}
	}
	if r.StructuredContent != nil {
		if b, ok := r.StructuredContent.([]byte); ok {
			n += int64(agent.EstimateTokensBytes(b))
		} else if b, err := marshalStructured(r.StructuredContent); err == nil {
			n += int64(agent.EstimateTokensBytes(b))
		}
	}
	return n
}
