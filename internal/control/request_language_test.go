package control

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/i18n"
)

// The daemon renders its refusals, so a CLI that says nothing about language
// gets the fallback — Chinese — no matter what locale the shell is in. Status
// negotiated its language and every other subcommand did not, so the same
// session printed English for `status` and Chinese for `copies`.
func TestControlCallsCarryTheClientLanguage(t *testing.T) {
	f := newFixture(t)
	p := socketPath(t)
	r, err := NewServer(f.coll).Start(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })

	const unknownID = "00000000-0000-4000-8000-000000000000"
	SetClientLanguage(i18n.EN)
	t.Cleanup(func() { SetClientLanguage("") })

	_, _, err = CallCopies(context.Background(), p, "", CopiesRequest{ID: unknownID})
	if err == nil {
		t.Fatal("an unknown copy ID was accepted")
	}
	if !strings.Contains(err.Error(), "could not") && !strings.Contains(err.Error(), "copy") {
		t.Errorf("the refusal did not come back in English: %v", err)
	}
	if strings.ContainsAny(err.Error(), "复制状态检查失败") {
		t.Errorf("the refusal is still rendered in the fallback language: %v", err)
	}
}
