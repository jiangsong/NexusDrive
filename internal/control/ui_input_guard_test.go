package control

import (
	"strings"
	"testing"
)

// Rename refuses a name containing a slash; creating a folder concatenated
// the name into the path unchecked, so "a/b" made a directory somewhere other
// than the one on screen. Two inputs of the same kind should not disagree
// about what a name is.
func TestNewFolderRefusesASeparatorLikeRenameDoes(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	// One definition of what a name is, used by both inputs. Counting the
	// occurrences of the check instead would have failed the moment somebody
	// deduplicated them, which is the fix this test exists to ask for.
	if !strings.Contains(src, "function validName(") {
		t.Fatal("there is no single definition of what a name may contain")
	}
	if strings.Count(src, "validName(") < 3 {
		t.Fatal("renaming and creating a folder do not both go through it")
	}
}

// The authorization URL comes from the daemon, but the endpoint it is built
// from is an editable per-account field, so it is not beyond doubt. It ends
// up in an href; only https belongs there, and the page should say so itself
// rather than leave it to the content security policy.
func TestTheAuthorizationLinkAcceptsOnlyHTTPS(t *testing.T) {
	// The check belongs wherever the link is built, and that moved once
	// already when the add-drive modal and the guided setup came to share one
	// authorization step. Naming the file was what made this test wrong then,
	// so it now asks the question of whichever module renders the link.
	src := webSource(t, "web/auth_step.js")
	if !strings.Contains(src, "httpsOnly(") {
		t.Fatal("the authorization URL reaches an href with no scheme check")
	}
	if !strings.Contains(src, "protocol === 'https:'") {
		t.Fatal("the scheme check does not compare against https")
	}
	if strings.Contains(webSource(t, "web/add_drive.js"), "href: safe") {
		t.Fatal("the modal builds its own authorization href again, bypassing the shared check")
	}
}
