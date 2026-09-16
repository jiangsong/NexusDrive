package agent

import "unicode/utf8"

// EstimateTokens approximates how many tokens a model will spend on s. A
// CJK character is about one token; other text runs at about four bytes a
// token; ten percent is added for JSON framing and the odd long identifier.
// The point is not accuracy but a budget an agent can plan around
// (docs/agent-first-design.md §5.3): a result that estimates at 20k tokens
// is one Claude Code will refuse, whatever the exact count.
func EstimateTokens(s string) int {
	return EstimateTokensBytes([]byte(s))
}

// EstimateTokensBytes is EstimateTokens over raw bytes.
func EstimateTokensBytes(b []byte) int {
	cjk, other := 0, 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if IsCJK(r) {
			cjk++
		} else {
			other += size
		}
		b = b[size:]
	}
	n := cjk + (other+3)/4
	return n + n/10
}

// IsCJK reports whether r is a Han, kana or Hangul character, which
// tokenizers spend roughly a token each on.
func IsCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF, // CJK unified ideographs
		r >= 0x3400 && r <= 0x4DBF,   // extension A
		r >= 0x20000 && r <= 0x2A6DF, // extension B
		r >= 0x3000 && r <= 0x30FF,   // CJK punctuation, hiragana, katakana
		r >= 0xFF00 && r <= 0xFFEF,   // full-width forms
		r >= 0xAC00 && r <= 0xD7AF:   // Hangul syllables
		return true
	}
	return false
}
