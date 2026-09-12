package pool

import (
	"context"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/test/fakeprovider"
)

// TestSettingsEpochChangesWithRulesAndFailureDomain: the marker's settings
// epoch is what tells another machine "someone changed the pool settings".
// It must move when Rules or FailureDomain change and stay put otherwise,
// or upgrading to placement rules would silently fail to notify peers.
func TestSettingsEpochChangesWithRulesAndFailureDomain(t *testing.T) {
	ctx := context.Background()
	a := fakeprovider.New("a")
	dir := t.TempDir()
	settings := config.Pool{Replicas: 3, MinReplicas: 2}
	p := newTestPoolWith(t, dir, settings, a)

	e1 := p.settingsEpoch(ctx)
	e2 := p.settingsEpoch(ctx)
	if e1 != e2 {
		t.Fatalf("epoch moved with no settings change: %d -> %d", e1, e2)
	}

	p.settings.Rules = []config.PoolRule{{Prefix: "/photos", Replicas: 2}}
	e3 := p.settingsEpoch(ctx)
	if e3 == e1 {
		t.Fatal("epoch did not change when Rules changed")
	}
	e4 := p.settingsEpoch(ctx)
	if e3 != e4 {
		t.Fatalf("epoch moved again with no further change: %d -> %d", e3, e4)
	}

	p.settings.FailureDomain = config.FailureDomainProvider
	e5 := p.settingsEpoch(ctx)
	if e5 == e3 {
		t.Fatal("epoch did not change when FailureDomain changed")
	}
}
