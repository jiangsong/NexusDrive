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
