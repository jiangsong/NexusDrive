package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The OAuth callback listens on one fixed loopback port, and providers that
// require an exactly-registered redirect URI leave no room for an ephemeral
// one. So the port is a process-wide lock: a second authorization, for any
// account, cannot bind it. The registry only knew about one session per
// account, so two different accounts authorizing at once produced
// "address already in use" — surfaced as a generic 502 that named nothing a
// person could act on.
func TestASecondAccountCannotStartAuthorizationWhileAnotherIsInTheBrowser(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "first", "aliyun")
	writeConfigLine(t, cfg, "second", "aliyun")
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "aliyun" })

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/first/auth/start", AuthStartRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("first start: %d %s", rr.Code, rr.Body)
	}

	rr = accountRequest(t, srv, http.MethodPost, "/accounts/second/auth/start", AuthStartRequest{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("a second account started authorizing while the first held the callback port: %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "first") {
		t.Errorf("the refusal does not name the account holding the port: %s", rr.Body)
	}
}

// Walking away from an authorization has to give the port back, or the next
// account can never be added without restarting the daemon.
func TestTheCallbackPortIsFreedWhenAnAuthorizationIsCancelled(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "first", "aliyun")
	writeConfigLine(t, cfg, "second", "aliyun")
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "aliyun" })

	rr := accountRequest(t, srv, http.MethodPost, "/accounts/first/auth/start", AuthStartRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("first start: %d %s", rr.Code, rr.Body)
	}
	var start AuthStartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	rr = accountRequest(t, srv, http.MethodPost, "/accounts/first/auth/cancel?session="+start.Session, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/second/auth/start", AuthStartRequest{}); rr.Code != http.StatusOK {
		t.Fatalf("the callback port was not released by the cancel: %d %s", rr.Code, rr.Body)
	}
}

// An authorization nobody finishes must not hold the callback port for ever.
// Closing the tab is the ordinary way to abandon one — there is no beforeunload
// contract that guarantees a cancel arrives — and before the process-wide lock
// existed an abandoned flow only blocked its own account. Now it would block
// every account, until the daemon was restarted.
func TestAnAbandonedAuthorizationStopsHoldingTheCallbackPort(t *testing.T) {
	srv, cfg, _ := accountsServer(t)
	writeConfigLine(t, cfg, "first", "aliyun")
	writeConfigLine(t, cfg, "second", "aliyun")
	fa := &fakeAuth{release: make(chan error, 1)}
	srv.auth = fa.starter(func(s string) bool { return s == "aliyun" })

	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/first/auth/start", AuthStartRequest{}); rr.Code != http.StatusOK {
		t.Fatalf("first start: %d %s", rr.Code, rr.Body)
	}
	// Nobody polls and nobody cancels; the flow simply ends, the way it does
	// when its ten-minute context expires.
	fa.release <- errors.New("the person walked away")
	deadline := time.After(2 * time.Second)
	for {
		rr := accountRequest(t, srv, http.MethodPost, "/accounts/second/auth/start", AuthStartRequest{})
		if rr.Code == http.StatusOK {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the callback port is still held by an abandoned authorization: %d %s", rr.Code, rr.Body)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}
