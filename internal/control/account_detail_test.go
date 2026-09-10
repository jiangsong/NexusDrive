package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

func TestAccountDetailShowsPublicSettingsAndCapsAndNeverASecret(t *testing.T) {
	srv, cfg, path := accountsServer(t)
	// A legacy plaintext secret still sitting in the file, and a proxy.
	body, _ := os.ReadFile(path)
	body = append(body, []byte("  drive: {type: fake, refresh_token: 'never-show-this', drive_id: team-1, proxy: hk}\nproxy:\n  outbounds:\n    - {name: hk, type: socks5, addr: 'socks5://user:pw@127.0.0.1:7890'}\n")...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	*cfg = *fresh
	srv.collector.Providers = map[string]provider.Provider{"drive": fakeprovider.New("drive")}

	rr := accountRequest(t, srv, http.MethodGet, "/accounts/drive", nil)
	if rr.Code != 200 {
		t.Fatalf("GET detail: %d %s", rr.Code, rr.Body)
	}
	var d AccountDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Type != "fake" || d.Proxy != "hk" || d.Fields["drive_id"] != "team-1" || !d.HasCredentials || !d.Live || d.Caps == nil {
		t.Fatalf("detail: %+v", d)
	}
	if strings.Contains(rr.Body.String(), "never-show-this") || strings.Contains(rr.Body.String(), "refresh_token") {
		t.Fatalf("detail leaks a credential: %s", rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/accounts/nobody", nil); rr.Code != 404 {
		t.Fatalf("unknown account: %d", rr.Code)
	}
	// The mux redirects a dotted path before the handler sees it; a name
	// with a slash or other junk that reaches the handler is refused.
	if rr := accountRequest(t, srv, http.MethodGet, "/accounts/../etc", nil); rr.Code != 400 && rr.Code != 404 && rr.Code != 307 {
		t.Fatalf("hostile name: %d", rr.Code)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/accounts/bad%20name", nil); rr.Code != 400 {
		t.Fatalf("name with a space: %d", rr.Code)
	}

	// The proxy view redacts the credential in the address, both ways.
	rr = accountRequest(t, srv, http.MethodGet, "/proxy/config", nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "user:pw") || !strings.Contains(rr.Body.String(), "127.0.0.1:7890") {
		t.Fatalf("proxy config view: %d %s", rr.Code, rr.Body)
	}
}

// Clearing a box in the connection settings sends JSON null for that field,
// which is the only way the page can remove a setting: an empty scalar and an
// absent key mean the same thing to a driver, so the wire form has to be the
// deletion. This locks the contract the overlay depends on.
func TestAccountPatchWithANullFieldRemovesIt(t *testing.T) {
	srv, _, path := accountsServer(t)
	if rr := accountRequest(t, srv, http.MethodPatch, "/accounts/existing", AccountPatch{
		Fields: map[string]*string{"timeout": str("30s")},
	}); rr.Code != 200 {
		t.Fatalf("setting the field: %d %s", rr.Code, rr.Body)
	}
	rr := accountRequest(t, srv, http.MethodPatch, "/accounts/existing", AccountPatch{
		Fields: map[string]*string{"timeout": nil},
	})
	if rr.Code != 200 {
		t.Fatalf("clearing the field: %d %s", rr.Code, rr.Body)
	}
	var out AccountMutationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Detail == nil {
		t.Fatal("no detail in the reply")
	}
	if _, still := out.Detail.Fields["timeout"]; still {
		t.Errorf("the cleared field is still in the reply: %v", out.Detail.Fields)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "timeout") {
		t.Errorf("the cleared field is still in the file:\n%s", after)
	}
}

func TestAccountPatchEditsAndRefusesSecrets(t *testing.T) {
	srv, _, path := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodPatch, "/accounts/existing", AccountPatch{Fields: map[string]*string{"pass": str("x")}})
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "cloudfs config auth existing") {
		t.Fatalf("a credential through PATCH: %d %s", rr.Code, rr.Body)
	}
	rr = accountRequest(t, srv, http.MethodPatch, "/accounts/existing", AccountPatch{Fields: map[string]*string{"_http_client": str("x")}})
	if rr.Code != 400 {
		t.Fatalf("an injection slot through PATCH: %d %s", rr.Code, rr.Body)
	}
	rr = accountRequest(t, srv, http.MethodPatch, "/accounts/existing", AccountPatch{Proxy: str("nowhere")})
	if rr.Code != 400 {
		t.Fatalf("an unknown proxy through PATCH: %d %s", rr.Code, rr.Body)
	}
	before, _ := os.ReadFile(path)

	workers := 3
	rr = accountRequest(t, srv, http.MethodPatch, "/accounts/existing", AccountPatch{
		Fields:        map[string]*string{"url": str("https://nas.local/newdav"), "timeout": str("30s")},
		UploadWorkers: &workers,
	})
	if rr.Code != 200 {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body)
	}
	var out AccountMutationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.RestartRequired || out.Detail == nil || out.Detail.Fields["url"] != "https://nas.local/newdav" || out.Detail.UploadWorkers != 3 {
		t.Fatalf("PATCH reply: %+v", out)
	}
	after, _ := os.ReadFile(path)
	if string(before) == string(after) {
		t.Fatal("PATCH did not write the file")
	}
	if !strings.Contains(string(after), "newdav") {
		t.Fatalf("file after PATCH: %s", after)
	}
	// The next GET reflects the file, not the daemon's stale view.
	rr = accountRequest(t, srv, http.MethodGet, "/accounts/existing", nil)
	if !strings.Contains(rr.Body.String(), "newdav") {
		t.Fatalf("GET after PATCH is stale: %s", rr.Body)
	}
}

func TestAccountDeleteNeedsConfirmAndRefusesWhileMounted(t *testing.T) {
	srv, _, path := accountsServer(t)
	if rr := accountRequest(t, srv, http.MethodDelete, "/accounts/existing", nil); rr.Code != 400 || !strings.Contains(rr.Body.String(), "confirm") {
		t.Fatalf("delete without confirm: %d %s", rr.Code, rr.Body)
	}
	if err := config.AddMount(path, "/mnt/x", "/existing", config.Layout{Remote: "existing"}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()
	if rr := accountRequest(t, srv, http.MethodDelete, "/accounts/existing?confirm=true", nil); rr.Code != 409 {
		t.Fatalf("delete while mounted: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodDelete, "/mounts?path=/mnt/x&prefix=/existing&confirm=true", nil); rr.Code != 200 {
		t.Fatalf("remove mount: %d %s", rr.Code, rr.Body)
	}
	rr := accountRequest(t, srv, http.MethodDelete, "/accounts/existing?confirm=true", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"restart_required":true`) {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/accounts/existing", nil); rr.Code != 404 {
		t.Fatalf("deleted account still served: %d", rr.Code)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "existing") {
		t.Fatalf("file still names the remote: %s", b)
	}
}

func TestAccountDeleteRefusesPoolMemberInTheRequestedLanguage(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.AddRemote(path, "other", config.Remote{Type: "webdav", Extra: map[string]any{"url": "https://other.local/dav"}}, config.AddRemoteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := config.CreatePool(path, "home", []config.PoolMember{{Remote: "existing"}, {Remote: "other"}}, 2, 1, ""); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()
	before, _ := os.ReadFile(path)

	for _, tc := range []struct {
		lang string
		want string
	}{
		{lang: "zh", want: "仍是存储池 \"home\" 的成员"},
		{lang: "en", want: "still a member of pool \"home\""},
	} {
		rr := accountRequest(t, srv, http.MethodDelete, "/accounts/existing?confirm=true&lang="+tc.lang, nil)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tc.want) || strings.Contains(rr.Body.String(), "unknown remote") {
			t.Errorf("%s refusal: %d %s", tc.lang, rr.Code, rr.Body)
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatalf("a refused HTTP deletion changed the configuration:\n%s", after)
	}
}

func TestAccountCheckReportsOnlyTheKind(t *testing.T) {
	srv, _, _ := accountsServer(t)
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/existing/check", nil); rr.Code != 501 {
		t.Fatalf("unwired check: %d", rr.Code)
	}
	srv.collector.CheckAccount = func(ctx context.Context, name string) error {
		if name != "existing" {
			return errors.New("wrong name")
		}
		// What the daemon hands back is already sanitised; the route must
		// not add anything to it.
		return errors.New("account check failed (authentication rejected); verify credentials, proxy and root settings")
	}
	rr := accountRequest(t, srv, http.MethodPost, "/accounts/existing/check", nil)
	if rr.Code != 200 {
		t.Fatalf("check: %d %s", rr.Code, rr.Body)
	}
	var out AccountCheckResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OK || !strings.Contains(out.Error, "authentication rejected") {
		t.Fatalf("check reply: %+v", out)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/ghost/check", nil); rr.Code != 404 {
		t.Fatalf("check unknown: %d", rr.Code)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/accounts/existing/check", nil); rr.Code != 405 {
		t.Fatalf("GET check: %d", rr.Code)
	}
}

func str(s string) *string { return &s }
