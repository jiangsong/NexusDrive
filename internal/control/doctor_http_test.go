package control

import (
	"net/http"
	"strings"
	"testing"
)

// TestDoctorRoutesRunAndFixOnTheLiveDaemon: the diagnostics page shows the
// same checks, under the same names, that `cloudfs doctor` prints — and Fix
// does not run without confirm.
func TestDoctorRoutesRunAndFixOnTheLiveDaemon(t *testing.T) {
	f := newFixture(t)
	f.coll.Doctor = &Doctor{Journal: f.j, Meta: f.meta, Cache: f.cache, CacheDir: f.dir,
		FreeSpace: func(string) (int64, error) { return 100 << 30, nil }}
	s := NewServer(f.coll)

	if w := call(t, s, "GET", "/doctor/run", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET run: %d (Run writes a probe file; it is a POST)", w.Code)
	}
	out := decode[DoctorResponse](t, call(t, s, "POST", "/doctor/run", ""))
	names := map[string]bool{}
	for _, c := range out.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"platform", "cache_dir", "metadata_db", "upload_queue", "durability"} {
		if !names[want] {
			t.Fatalf("check %q missing from %v", want, names)
		}
	}
	if out.OK+out.Warn+out.Fail != len(out.Checks) {
		t.Fatalf("summary does not add up: %+v", out)
	}

	if w := call(t, s, "POST", "/doctor/fix", `{}`); w.Code != 400 || !strings.Contains(w.Body.String(), "confirm") {
		t.Fatalf("fix without confirm: %d %s", w.Code, w.Body.String())
	}
	fixed := decode[DoctorFixResponse](t, call(t, s, "POST", "/doctor/fix", `{"confirm":true}`))
	if len(fixed.Done) == 0 {
		t.Fatalf("fix reported nothing: %+v", fixed)
	}

	unwired := NewServer(newFixture(t).coll)
	if w := call(t, unwired, "POST", "/doctor/run", ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired doctor: %d", w.Code)
	}
}
