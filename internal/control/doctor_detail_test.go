package control

import (
	"strings"
	"testing"

	"cloudfs/internal/i18n"
)

// A healthy pool member's state has no catalog entry: it is passed through.
// Writing it into Detail directly left it outside c.detail, so the first
// addDetail rebuilt Detail from the keys alone and the member's state
// vanished from the report exactly when something else was worth saying
// about it.
func TestPassedThroughDetailSurvivesALaterClause(t *testing.T) {
	c := Check{Name: "pool/home/member/a", Level: LevelOK}
	c.passDetail("up")
	c.addDetail("doctor.pool.member.pending", 2)

	if !strings.Contains(c.Detail, "up") {
		t.Errorf("the member's state was dropped: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "2 tree operations") {
		t.Errorf("the pending clause is missing: %q", c.Detail)
	}
	zh := c.Localize(i18n.ZH)
	if !strings.Contains(zh.Detail, "up") {
		t.Errorf("a passed-through state must not be translated away: %q", zh.Detail)
	}
	if !strings.Contains(zh.Detail, "还有 2 个") {
		t.Errorf("the pending clause did not translate: %q", zh.Detail)
	}
}
