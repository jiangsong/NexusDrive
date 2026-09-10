package control

import (
	"strings"
	"testing"

	"cloudfs/internal/i18n"
)

// Two pool checks add a clause to what they already said — "and N tree
// operations are still queued on it". Appending it to the rendered Detail
// meant Localize, which rebuilds Detail from the key, threw the clause away
// in every language including the one it was written in. A detail is a list
// of fragments, so a second fragment survives translation like the first.
func TestAnAddedDetailClauseSurvivesLocalization(t *testing.T) {
	var c Check
	c.setDetail("doctor.pool.member.ok", "up")
	c.addDetail("doctor.pool.member.pending", 3)

	en := c.Localize(i18n.EN)
	zh := c.Localize(i18n.ZH)
	for _, got := range []string{en.Detail, zh.Detail} {
		if !strings.Contains(got, "3") {
			t.Errorf("the added clause was dropped: %q", got)
		}
	}
	if en.Detail == zh.Detail {
		t.Fatalf("both fragments must follow the language: %q", en.Detail)
	}
}
