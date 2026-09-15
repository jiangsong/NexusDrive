package control

import (
	"strings"
	"testing"
)

// An authorization session is server state: the daemon holds a callback
// listener or a device-code poll open for it, and only one flow per account
// is allowed at a time. Closing the sheet without cancelling left that state
// behind, so the next attempt met a 409 from a flow nobody was watching.
func TestClosingTheAddDriveSheetCancelsTheAuthSession(t *testing.T) {
	src := webSource(t, "web/add_drive.js")
	if !strings.Contains(src, "/auth/cancel") {
		t.Error("the add-drive sheet never cancels a started authorization")
	}
	authStep := webSource(t, "web/auth_step.js")
	if !strings.Contains(authStep, "/auth/qr?session=") || !strings.Contains(src, "startAuthorization") {
		t.Fatal("browser authorization does not render a session-bound QR image")
	}
	// Finishing has to leave the list showing what was just created,
	// otherwise the new drive appears only after a manual refresh.
	if !strings.Contains(src, "onDone") && !strings.Contains(src, "opts.onClose") {
		t.Error("the add-drive sheet never tells its opener that something changed")
	}
}

// A pin declared in the configuration cannot be removed through the API — the
// daemon reads it from the file every start. The row said so in grey text and
// stopped there, which leaves the reader with a fact and no next step.
func TestConfiguredPinRowSaysWhereToChangeIt(t *testing.T) {
	if !strings.Contains(webSource(t, "web/screens/storage.js"), "t('storage.editconfig.help'") {
		t.Fatal("a configuration-sourced pin gives the reader no way forward")
	}
}

func TestExportUIUsesLiveEventsAndPagedDetails(t *testing.T) {
	exports := webSource(t, "web/screens/exports.js")
	if strings.Contains(exports, "setInterval(") {
		t.Fatal("the export screen still polls instead of consuming SSE progress")
	}
	for _, want := range []string{"onExportChange", "/items?", "item_next_cursor", "exports.details", "pickSource"} {
		if !strings.Contains(exports, want) {
			t.Errorf("export screen missing %q", want)
		}
	}
	ui := webSource(t, "web/ui.js")
	if !strings.Contains(ui, "validate ? validate()") || !strings.Contains(ui, "el('label', { for: id") {
		t.Fatal("shared forms do not keep validation inline with semantic labels")
	}
}
