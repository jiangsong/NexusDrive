package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"cloudfs/internal/provider"
)

// The moment a drive finishes authorizing is the moment a person most wants to
// see that it worked and how much room it brings. Reporting it here also gives
// the setup flow the one fact it needs to know which drives cannot report
// their own space and should be asked for a capacity instead.
func TestCheckingAnAccountReportsTheSpaceTheBackendKnowsAbout(t *testing.T) {
	srv, _, _ := accountsServer(t)
	srv.collector.CheckAccount = func(context.Context, string) error { return nil }
	srv.collector.AccountQuota = func(context.Context, string) (provider.Quota, bool, error) {
		return provider.Quota{Total: 100 << 30, Used: 40 << 30}, true, nil
	}

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/existing/check", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST check = %d %s", rr.Code, rr.Body)
	}
	var out AccountCheckResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("check reported failure: %+v", out)
	}
	if out.Total != 100<<30 || out.Used != 40<<30 || out.Free != 60<<30 {
		t.Fatalf("space = total %d used %d free %d", out.Total, out.Used, out.Free)
	}
}

// Dropbox-style backends report nothing. That is not a failure of the account,
// and the check must still say the drive works.
func TestCheckingAnAccountThatCannotReportSpaceStillSucceeds(t *testing.T) {
	srv, _, _ := accountsServer(t)
	srv.collector.CheckAccount = func(context.Context, string) error { return nil }
	srv.collector.AccountQuota = func(context.Context, string) (provider.Quota, bool, error) {
		return provider.Quota{}, false, nil
	}

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/existing/check", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST check = %d %s", rr.Code, rr.Body)
	}
	var out AccountCheckResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("a drive that cannot report space was reported as broken: %+v", out)
	}
	if out.Total != 0 || out.Free != 0 {
		t.Fatalf("space was invented for a backend that reports none: %+v", out)
	}
}
