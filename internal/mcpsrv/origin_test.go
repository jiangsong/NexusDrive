package mcpsrv

import (
	"testing"
	"time"

	"cloudfs/internal/vfs"
)

// TestToolCallsAreTaggedAsAPIChanges: every change a tool makes is reported
// to change subscribers with the api origin, which is what lets a trigger
// rule keep the agent's own writes from re-triggering it. The server has no
// session store here, so the tag must not depend on session resolution.
func TestToolCallsAreTaggedAsAPIChanges(t *testing.T) {
	e := newEnv(t, Options{})
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	var ok okOutput
	if res := e.call(t, "create_directory", mkdirInput{Path: "/tagged"}, &ok); res.IsError {
		t.Fatalf("mkdir failed: %s", errText(res))
	}
	if res := e.call(t, "write_file", writeInput{Path: "/tagged/f.txt", Content: "data"}, nil); res.IsError {
		t.Fatalf("write failed: %s", errText(res))
	}
	want := []struct {
		kind vfs.ChangeKind
		path string
	}{{vfs.KindMkdir, "/tagged"}, {vfs.KindCreate, "/tagged/f.txt"}, {vfs.KindWrite, "/tagged/f.txt"}}
	for _, w := range want {
		select {
		case c := <-ch:
			if c.Kind != w.kind || c.Origin != vfs.OriginAPI || len(c.Paths) != 1 || c.Paths[0] != w.path {
				t.Fatalf("change %+v (%s/%s), want %s/api at %s", c, c.Kind, c.Origin, w.kind, w.path)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no change for %s %s", w.kind, w.path)
		}
	}
}
