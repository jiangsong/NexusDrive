package daemon

import (
	"strings"
	"testing"
)

// No built-in application may request a Google restricted scope.
//
// Mounting a whole Drive needs https://www.googleapis.com/auth/drive, which
// Google classes as restricted: an application that ships it has to pass brand
// verification and then repeat a third-party CASA security assessment every
// twelve months to keep working. A registration added to this table without
// that in place does not fail here — it fails months later, on every user at
// once, when the application is suspended.
//
// The supported path is the account's own client id, which ResolveOAuthClient
// already prefers over anything in this table. This test is what keeps a
// convenient-looking one-line addition from turning into that outage.
func TestNoBuiltinApplicationShipsARestrictedGoogleScope(t *testing.T) {
	for typ, app := range builtinOAuthApps {
		if app.ClientID == "" {
			continue // a commented placeholder, not a registration
		}
		profile, ok := OAuthProfileFor(typ)
		if !ok {
			continue
		}
		for _, scope := range strings.Fields(profile.Scope) {
			if scope == "https://www.googleapis.com/auth/drive" || scope == "https://www.googleapis.com/auth/drive.readonly" {
				t.Errorf("%s ships a built-in application requesting the restricted scope %q; "+
					"that needs brand verification plus an annual CASA assessment, and every account "+
					"authorized under it stops refreshing the day it lapses", typ, scope)
			}
		}
	}
}

// gdrive in particular is expected to have no built-in registration, so that
// the browser flow falls through to the account's own client id. This is
// narrower than the scope rule above and states the decision directly.
func TestGoogleDriveHasNoBuiltInApplication(t *testing.T) {
	if _, ok := BuiltinOAuthAppFor("gdrive"); ok {
		t.Error("gdrive has a built-in OAuth application; see docs/plans/2026-09-18-single-home-dir-onboarding-design.md")
	}
}
