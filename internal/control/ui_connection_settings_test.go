package control

import (
	"strings"
	"testing"
)

// The connection settings overlay is the fourth board of the design: it is
// where a drive's own settings, its reachability probe and the mount layouts
// that point at it are edited. Without it the sidebar can only browse, the
// PATCH and check routes have no caller at all, and the delete path dead-ends
// at "unbind the mount first" with nowhere in the page to unbind it. Every
// mutation here changes the configuration only, so the overlay has to say the
// daemon keeps serving the old layout until it restarts.
func TestConnectionSettingsOverlayReachesTheAccountAndMountRoutes(t *testing.T) {
	b, err := webFS.ReadFile("web/connection.js")
	if err != nil {
		t.Fatalf("the connection settings overlay is not in the embedded tree: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"api.get('/accounts/",   // detail: type, proxy, qps, workers, caps, mounts
		"api.patch('/accounts/", // edit the non-secret settings
		"/check'",               // probe the account without leaving the page
		"api.post('/mounts'",    // bind a prefix
		"api.del('/mounts?",     // ...and unbind it, which is what unblocks delete
		"t('conn.restart'",      // every edit here is durable at once, live at restart
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the connection settings overlay has no %s", want)
		}
	}
	if strings.Contains(src, "type: 'password'") || strings.Contains(src, `type="password"`) {
		t.Error("the connection settings overlay renders a credential field; credentials never travel through the browser")
	}
}

// Selecting a connection used to reset the browser to the root and nothing
// else, so every row in the sidebar behaved identically. The row is the only
// way into the settings for that drive.
func TestSelectingAConnectionOpensItsSettings(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	if !strings.Contains(src, "/ui/connection.js") || !strings.Contains(src, "openConnection(") {
		t.Fatal("the connection list never opens the settings overlay")
	}
}
