package control

import (
	"encoding/json"
	"net/http"
	"testing"

	"cloudfs/internal/config"
)

// An account named for a pool that does not exist must create nothing. The
// atomicity of the two writes themselves is pinned one layer down, in
// TestAddRemoteWritesBothTheRemoteAndTheMemberOrNeither; this is the boundary
// check that keeps a typo from reaching it at all.
func TestAddingAnAccountToAnUnknownPoolCreatesNothing(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.CreatePool(path, "home", []config.PoolMember{{Remote: "existing"}}, 2, 1, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.collector.Config = cfg
	srv.reloadConfigView()

	rr := accountRequest(t, srv, http.MethodPost, "/accounts", AddAccountRequest{
		Name: "second", Type: "webdav", Fields: map[string]string{"url": "https://nas.local/dav"}, Pool: "nope",
	})
	if rr.Code == http.StatusOK {
		t.Fatalf("an unknown pool was accepted: %s", rr.Body)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Remotes["second"]; ok {
		t.Fatal("the account was created even though the join failed")
	}
}

// The successful path still reports which pool was joined, and the membership
// is really in the file.
func TestAddingAnAccountWithAPoolStillReportsTheJoinedPool(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.CreatePool(path, "home", []config.PoolMember{{Remote: "existing"}}, 2, 1, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.collector.Config = cfg
	srv.reloadConfigView()

	rr := accountRequest(t, srv, http.MethodPost, "/accounts", AddAccountRequest{
		Name: "second", Type: "webdav", Fields: map[string]string{"url": "https://nas2.local/dav"}, Pool: "home",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /accounts = %d %s", rr.Code, rr.Body)
	}
	var out AddAccountResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.JoinedPool != "home" {
		t.Errorf("joined_pool = %q, want home", out.JoinedPool)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var joined bool
	for _, m := range after.Pools["home"].Members {
		if m.Remote == "second" {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("the account did not join the pool: %+v", after.Pools["home"].Members)
	}
}
