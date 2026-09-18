package gdrive

import (
	"strings"
	"testing"

	"cloudfs/internal/provider"
)

// Google Drive is the one backend a person has to register an application for
// themselves, because the scope a mounted drive needs is restricted and no
// shipped registration can carry it. A note saying "paste a client id" is not
// enough to get anyone through the Cloud console, so the driver describes the
// whole path, and the console renders it.
//
// Two of these steps exist because of failure modes that are otherwise silent:
// a Web application client rejects the loopback callback unless the exact port
// is registered, while a Desktop client accepts any; and an application left
// in Testing hands out a refresh token that stops working after seven days,
// with nothing but invalid_grant to say why.
func TestDriveDescribesHowToRegisterAnApplication(t *testing.T) {
	creds := provider.CredentialsFor("gdrive")
	if len(creds.Setup) == 0 {
		t.Fatal("gdrive offers no setup steps, so the console has nothing to show")
	}
	joined := strings.ToLower(strings.Join(creds.Setup, "\n"))
	for _, want := range []string{"drive api", "desktop", "production", "client id", "client secret"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the setup steps never mention %q:\n%s", want, strings.Join(creds.Setup, "\n"))
		}
	}
}

// A backend whose authorization the daemon drives itself must not grow a
// registration walkthrough: the whole point of the built-in application is
// that there is nothing to walk through.
func TestBackendsWithNothingToRegisterOfferNoSteps(t *testing.T) {
	if steps := provider.CredentialsFor("webdav").Setup; len(steps) != 0 {
		t.Errorf("webdav offers setup steps it has no use for: %v", steps)
	}
}
