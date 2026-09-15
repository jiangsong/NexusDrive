package control

import (
	"strings"
	"testing"

	"cloudfs/internal/export"
)

// An export is the longest-running thing this daemon does, and the one whose
// destination can be unplugged mid-job. A person needs to see where it got to
// and be able to pause, resume, stop or discard it without a terminal.
func TestExportsScreenReachesTheExportRoutes(t *testing.T) {
	src := webSource(t, "web/screens/exports.js")
	for _, want := range []string{
		"api.get('/exports",             // the list, paged like every other list
		"api.post('/export'",            // start one
		"api.post('/exports/' + action", // pause, resume and cancel share one call
		"'pause'",
		"'resume'",
		"'cancel'",
		"api.post('/exports/forget'", // drop the record, confirm required
		"confirm: true",              // ...which the daemon refuses without
		"next_cursor",
		"confirmDelete(", // both destructive actions type out the target
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the exports screen has no %s", want)
		}
	}
	// A mirror deletes destination files, so the page must not be able to
	// send one without also sending the confirmation the daemon demands.
	mirror := strings.Index(src, "mirror.checked")
	confirm := strings.Index(src, "q.confirm = true")
	if mirror < 0 || confirm < mirror {
		t.Error("the mirror checkbox does not carry a typed confirmation with it")
	}
}

// A screen nobody can navigate to is a screen that does not exist.
func TestExportsScreenIsRoutedAndInTheNav(t *testing.T) {
	router := webSource(t, "web/router.js")
	app := webSource(t, "web/app.js")
	if !strings.Contains(router, "#/exports") || !strings.Contains(router, "exports-view") {
		t.Error("the router has no route for the exports screen")
	}
	if !strings.Contains(router, "'nav.exports'") {
		t.Error("the exports screen is in no nav item")
	}
	if !strings.Contains(app, "renderExports") {
		t.Error("the shell never mounts the exports screen")
	}
}

// Export states and pause reasons are assembled at the call site —
// t('export.state.' + j.state) — so the literal scan in ui_i18n_test.go
// cannot see them. Both enums live in Go, so the catalog is checked against
// the constants rather than against a list kept by hand in two places.
func TestWebCatalogCoversExportStatesAndPauseReasons(t *testing.T) {
	known := tableKeys(t, webI18nSource(t), "zh")
	for _, state := range []export.State{
		export.StatePlanning, export.StateRunning, export.StatePaused,
		export.StateDone, export.StateFailed, export.StateCancelled, export.StatePurging,
	} {
		if key := "export.state." + string(state); !known[key] {
			t.Errorf("the catalog has no translation for export state %q", state)
		}
	}
	for _, reason := range []export.PauseReason{
		export.PauseUser, export.PauseDisk, export.PauseUnavailable,
		export.PauseRiskControl, export.PauseAuth,
	} {
		if reason == export.PauseNone {
			continue
		}
		if key := "export.pause." + string(reason); !known[key] {
			t.Errorf("the catalog has no translation for pause reason %q", reason)
		}
	}
}
