package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/i18n"

	_ "cloudfs/internal/provider/gdrive"
	_ "cloudfs/internal/provider/webdav"
)

// The console has to carry the registration walkthrough, not just the
// one-line credential note. Google Drive is the backend that needs one: its
// scope is restricted, so no application ships with the binary and the person
// registers their own. A console that only says "client id" leaves them to
// discover the Desktop-app client type and the Production publish on their
// own, and both failures are silent ones — a rejected callback, and a grant
// that dies a week later.
func TestAccountTypesCarryTheRegistrationWalkthrough(t *testing.T) {
	srv := NewServer(&Collector{})
	rr := accountRequest(t, srv, http.MethodGet, "/accounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var out AccountsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var drive, dav *AccountType
	for i := range out.Types {
		switch out.Types[i].Type {
		case "gdrive":
			drive = &out.Types[i]
		case "webdav":
			dav = &out.Types[i]
		}
	}
	if drive == nil || dav == nil {
		t.Fatalf("expected gdrive and webdav among %d types", len(out.Types))
	}
	if len(drive.Setup) == 0 {
		t.Fatal("gdrive carries no setup steps, so the add-drive screen cannot show them")
	}
	if len(dav.Setup) != 0 {
		t.Errorf("webdav carries setup steps it has no use for: %v", dav.Setup)
	}

	// Both traps have to survive translation. A walkthrough that mentions the
	// Desktop client type in one language and not the other leaves half the
	// readers with redirect_uri_mismatch, and one that drops the Production
	// publish leaves them with a drive that dies after seven days.
	col := &Collector{}
	for _, tc := range []struct {
		lang  i18n.Lang
		wants []string
	}{
		{i18n.EN, []string{"Desktop app", "Production"}},
		{i18n.ZH, []string{"桌面应用", "Production"}},
	} {
		var steps []string
		for _, entry := range col.accountTypes(tc.lang) {
			if entry.Type == "gdrive" {
				steps = entry.Setup
			}
		}
		joined := strings.Join(steps, "\n")
		for _, want := range tc.wants {
			if !strings.Contains(joined, want) {
				t.Errorf("the %v walkthrough never mentions %q:\n%s", tc.lang, want, joined)
			}
		}
	}
}

// The add-drive screen renders the steps it is given. Asserted against the
// shipped asset so the route and the page cannot drift apart.
func TestAddDriveScreenRendersTheWalkthrough(t *testing.T) {
	src := webSource(t, "web/add_drive.js")
	for _, want := range []string{"tp.setup", "t('add.setup.title')"} {
		if !strings.Contains(src, want) {
			t.Errorf("the add-drive screen has no %s", want)
		}
	}
}
