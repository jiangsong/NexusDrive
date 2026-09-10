package control

import (
	"strings"
	"testing"
)

// Removing a connection is a configuration edit with consequences the file
// table does not have: the daemon keeps serving the old layout until it
// restarts, and config.RemoveRemote refuses outright while a mount or pool
// points at the drive. The page therefore checks both references first and
// says so in the reader's language, then goes through the same typed
// confirmation and guarded DELETE route the terminal would use. These
// assertions read the shipped asset so the wiring cannot quietly regress.
func TestConnectionsScreenDeletesADriveThroughTheGuardedRoute(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{
		"api.get('/mounts')",                  // a mounted drive is turned away before anything is asked
		"t('conn.remove.mounted'",             // ...and told which layout to unbind, in its own language
		"api.get('/pool/status')",             // a pool member has to be drained, never silently detached
		"t('conn.remove.pooled'",              // ...and the page explains that workflow before confirmation
		"member.pending_restart !== 'remove'", // a removed runtime member must not block account deletion before restart
		"confirmDelete(",                      // the destructive button stays disabled until the name is typed
		"t('conn.remove.title'",
		"api.del('/accounts/", // the guarded route, never a config write of its own
		"?confirm=true",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the connections list has no %s in its delete path", want)
		}
	}
}
