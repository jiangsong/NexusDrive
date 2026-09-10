package control

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Resuming a half-finished setup means knowing which drives still need
// authorizing. Without it the page has to fetch every account one at a time
// just to find out where it left off.
func TestTheAccountListSaysWhichAccountsStillNeedAuthorization(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "authorized", "aliyun")
	srv.reloadConfigView()

	rr := accountRequest(t, srv, http.MethodGet, "/accounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /accounts = %d %s", rr.Code, rr.Body)
	}
	var out AccountsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Remotes) == 0 {
		t.Fatal("no accounts were listed")
	}
	for _, r := range out.Remotes {
		if r.Name == "existing" && r.HasCredentials {
			t.Errorf("%q has no credential stored but the list says it has one", r.Name)
		}
	}
}

// The wizard offers a browser button for the drives a browser can authorize
// and a terminal command for the rest. Which is which is the daemon's answer,
// not a list kept in the page — that list is exactly what went stale twice.
func TestTheAccountTypesSayWhichOnesCanBeAuthorizedInABrowser(t *testing.T) {
	srv, _, _ := accountsServer(t)
	fa := &fakeAuth{release: make(chan error, 1)}
	// Only the registered provider types appear in the list; this package
	// links webdav and smb, so those stand in for the two cases.
	srv.auth = fa.starter(func(s string) bool { return s == "webdav" })

	rr := accountRequest(t, srv, http.MethodGet, "/accounts", nil)
	var out AccountsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var sawBrowser, sawTerminal bool
	for _, ty := range out.Types {
		if ty.Type == "webdav" {
			sawBrowser = ty.BrowserAuth
		}
		if ty.Type == "smb" && !ty.BrowserAuth {
			sawTerminal = true
		}
	}
	if !sawBrowser {
		t.Error("webdav can be authorized by the daemon here but the list does not say so")
	}
	if !sawTerminal {
		t.Error("smb has no authorization server but the list does not say so")
	}
}
