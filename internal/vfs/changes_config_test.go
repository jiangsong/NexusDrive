package vfs

import (
	"testing"

	"cloudfs/internal/config"
)

// TestTriggerEventNamesMatchVFS pins config.TriggerEvents and
// config.TriggerOrigins to the String names of ChangeKind and Origin:
// config cannot import vfs, so the lists are written twice and this is
// what keeps a renamed kind from silently invalidating every rule.
func TestTriggerEventNamesMatchVFS(t *testing.T) {
	kinds := []ChangeKind{KindWrite, KindCreate, KindMkdir, KindRemove, KindRename, KindRemote, KindRescan}
	if len(kinds) != len(config.TriggerEvents) {
		t.Fatalf("config knows %d events, vfs has %d kinds", len(config.TriggerEvents), len(kinds))
	}
	for i, k := range kinds {
		if k.String() != config.TriggerEvents[i] {
			t.Errorf("kind %d: vfs %q, config %q", i, k, config.TriggerEvents[i])
		}
	}
	origins := []Origin{OriginKernel, OriginAPI, OriginRemote}
	if len(origins) != len(config.TriggerOrigins) {
		t.Fatalf("config knows %d origins, vfs has %d", len(config.TriggerOrigins), len(origins))
	}
	for i, o := range origins {
		if o.String() != config.TriggerOrigins[i] {
			t.Errorf("origin %d: vfs %q, config %q", i, o, config.TriggerOrigins[i])
		}
	}
	if ChangeKind(0).String() == config.TriggerEvents[0] || Origin(0).String() == config.TriggerOrigins[0] {
		t.Fatal("the zero value must not read as a real kind or origin")
	}
}
