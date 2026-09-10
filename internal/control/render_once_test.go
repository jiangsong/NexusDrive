package control

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/i18n"
)

// A doctor report keeps the catalog key behind each line, so the same run can
// be answered in another language at the HTTP boundary without the doctor
// knowing a language exists. A clause added after the fact travels with it.
func TestDoctorChecksCarryTheirKeys(t *testing.T) {
	dir := t.TempDir()
	d := &Doctor{CacheDir: filepath.Join(dir, "cache")}
	checks := d.Run(context.Background())
	if len(checks) == 0 {
		t.Fatal("no checks ran")
	}
	en := LocalizeChecks(checks, i18n.EN)
	zh := LocalizeChecks(checks, i18n.ZH)
	same := 0
	for i := range en {
		if en[i].Detail == zh[i].Detail {
			same++
		}
	}
	if same == len(en) {
		t.Fatal("no check followed the language")
	}
}

// The same for a status snapshot: the warnings are rendered once, in the
// language asked for, and the ones a filesystem hands over as prose have no
// catalog entry and are passed through as they arrived.
func TestStatusWarningsRenderInTheRequestedLanguage(t *testing.T) {
	// The prose warning goes on BEFORE the rendering step, which is what makes
	// the pass-through assertion mean anything: appended afterwards it would be
	// the same literal on both sides by construction and could never fail.
	const prose = "the pin store said something we cannot translate"
	build := func(lang i18n.Lang) Status {
		s := Status{}
		s.Uploads.Dead = 2
		s.Warnings = append(s.Warnings, prose)
		warnings(&s, lang)
		return s
	}
	en, zh := build(i18n.EN), build(i18n.ZH)
	if len(en.Warnings) != 2 || len(zh.Warnings) != 2 {
		t.Fatalf("warnings: %v / %v", en.Warnings, zh.Warnings)
	}
	if en.Warnings[0] != prose || zh.Warnings[0] != prose {
		t.Fatalf("a warning with no catalog entry did not survive rendering: %q / %q", en.Warnings[0], zh.Warnings[0])
	}
	if !strings.Contains(en.Warnings[1], "failed permanently") || en.Warnings[1] == zh.Warnings[1] {
		t.Fatalf("the warning did not follow the language: %q / %q", en.Warnings[1], zh.Warnings[1])
	}
}
