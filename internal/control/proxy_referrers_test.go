package control

import (
	"encoding/json"
	"net/http"
	"testing"

	"cloudfs/internal/config"
)

// A remote can name an outbound, and config.Validate refuses the whole write
// when that name stops existing (config: remote %q references unknown proxy
// %q). The page edits the proxy section alone, so it cannot follow a rename
// into the remotes — but it can say what a rename is about to break, instead
// of letting the save fail with a refusal that names nothing the page shows.
func TestTheProxyViewNamesWhatRefersToEachOutbound(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "office", Type: "socks5", Addr: "socks5://10.0.0.9:1080"}},
		Groups:    []config.Group{{Name: "pool", Type: "fallback", Members: []string{"office"}}},
		Rules:     []string{"FINAL,pool"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetRemoteField(path, "existing", config.SetRemoteFieldOptions{Proxy: strPtr("office")}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv.collector.Config = cfg
	srv.reloadConfigView()

	rr := accountRequest(t, srv, http.MethodGet, "/proxy/config", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /proxy/config = %d %s", rr.Code, rr.Body)
	}
	var view ProxyConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if got := view.Referrers["office"]; len(got) != 1 || got[0] != "existing" {
		t.Fatalf("the view does not say which remotes use the outbound: %v", view.Referrers)
	}
	if got := view.Referrers["pool"]; len(got) != 0 {
		t.Errorf("nothing refers to the group, but the view says %v", got)
	}
}

func strPtr(s string) *string { return &s }
