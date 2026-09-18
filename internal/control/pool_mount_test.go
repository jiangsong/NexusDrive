package control

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// A pool with no mount point is a pool nobody can open. The daemon assembles a
// filesystem from the mounts in the configuration, so a person who creates a
// pool in the console and restarts still finds /fs/list answering "this daemon
// has no filesystem" — with nothing on the page having said so.
//
// The create form therefore has to ask where the pool goes, and the daemon has
// to say what to put in that box: the mount that already exists, so a second
// pool joins the folder the person already uses, or the default folder when
// there is none.

func TestPoolStatusSaysWhereANewPoolWouldBeMounted(t *testing.T) {
	f := newPoolFixture(t)
	var out PoolStatusResponse
	w := f.do(t, http.MethodGet, "/pool/status", "")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mount != "/mnt/cloud" {
		t.Fatalf("mount suggestion = %q, want the mount the configuration already declares", out.Mount)
	}
}

func TestPoolStatusFallsBackToTheDefaultFolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("remotes:\n  a: {type: fake}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&Collector{Version: "pool-mount-test", Config: cfg})
	rr := accountRequest(t, srv, http.MethodGet, "/pool/status", nil)
	var out PoolStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mount != DefaultMountPath {
		t.Fatalf("mount suggestion = %q, want %q", out.Mount, DefaultMountPath)
	}
}

// The form has to send it, too: the page used to post `mount: res.mount || ”`
// against a field the daemon never filled, so every pool created from this
// screen was written without a mount and the filesystem stayed empty.
func TestThePoolCreateFormAsksWhereThePoolGoes(t *testing.T) {
	src := webSource(t, "web/screens/pool.js")
	for _, want := range []string{"t('pool.create.mount')", "mount: mountPath.value.trim()"} {
		if !strings.Contains(src, want) {
			t.Errorf("the pool create form has no %s", want)
		}
	}
	if strings.Contains(src, "mount: res.mount || ''") {
		t.Error("the form still posts a mount path the person was never shown")
	}
}
