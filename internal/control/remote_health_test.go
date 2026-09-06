package control

import (
	"context"
	"testing"

	"cloudfs/internal/provider"
)

type unreachableProvider struct {
	provider.Provider
	err error
}

func (u *unreachableProvider) Name() string                { return "u" }
func (u *unreachableProvider) Capabilities() provider.Caps { return provider.Caps{} }
func (u *unreachableProvider) Stat(context.Context, string) (provider.Entry, error) {
	return provider.Entry{}, u.err
}

// TestStatusReportsRemoteReachability: /status says whether each remote
// answers, from the calls actually made to it. A drive whose API is
// unreachable never trips the breaker; before this it looked healthy while
// every call failed.
func TestStatusReportsRemoteReachability(t *testing.T) {
	st := provider.NewStats()
	up := &unreachableProvider{err: provider.ErrTransient}
	p := provider.Instrument(up, st)
	for i := 0; i < 3; i++ {
		_, _ = p.Stat(context.Background(), "x")
	}
	c := &Collector{Version: "t", Remotes: []string{"nas", "quiet"}, CallStats: map[string]*provider.Stats{"nas": st}}
	s := c.Collect(context.Background())
	byName := map[string]RemoteStatus{}
	for _, r := range s.Remotes {
		byName[r.Remote] = r
	}
	if got := byName["nas"]; got.State != "down" || got.LastError == "" || got.DownSince == "" {
		t.Fatalf("unreachable remote reported as %+v", got)
	}
	if got := byName["quiet"]; got.State != "up" {
		t.Fatalf("a remote with no calls yet should be up, got %+v", got)
	}
	up.err = nil
	_, _ = p.Stat(context.Background(), "x")
	s = c.Collect(context.Background())
	for _, r := range s.Remotes {
		if r.Remote == "nas" && (r.State != "up" || r.LastOK == "") {
			t.Fatalf("after a success: %+v", r)
		}
	}
}
