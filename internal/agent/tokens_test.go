package agent

import (
	"strings"
	"testing"
)

func TestEstimateTokensCountsCJKPerCharacterAndASCIIPerFourBytes(t *testing.T) {
	ascii := strings.Repeat("abcd", 100) // 400 bytes → 100 tokens + 10%
	if got := EstimateTokens(ascii); got != 110 {
		t.Fatalf("ascii: got %d, want 110", got)
	}
	cjk := strings.Repeat("中", 100) // 100 chars → 100 tokens + 10%
	if got := EstimateTokens(cjk); got != 110 {
		t.Fatalf("cjk: got %d, want 110", got)
	}
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("empty: got %d", got)
	}
	// Mixed text is the sum of the two rules: 4 ASCII bytes and 2 CJK chars.
	if got := EstimateTokens("abcd中文"); got != 3 {
		t.Fatalf("mixed: got %d, want 3", got)
	}
}

func TestEstimateTokensCJKIsDenserThanBytesSuggest(t *testing.T) {
	// The reason the budget is tokens and not bytes: 300 bytes of Chinese
	// (100 chars) cost ~4x what 300 bytes of English do.
	zh, en := EstimateTokens(strings.Repeat("中", 100)), EstimateTokens(strings.Repeat("a", 300))
	if zh <= en {
		t.Fatalf("cjk %d should exceed ascii %d for the same byte count", zh, en)
	}
}
