package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/service"
)

// davServer answers the one PROPFIND an account check makes. A real host is
// deliberately not used: the check that matters here is "did the hook see this
// account at all", and pointing it at a name that does not resolve turns that
// question into a thirty-second DNS timeout whose error is indistinguishable
// from the failure being tested.
func davServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response>`+
			`<d:href>/dav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype>`+
			`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/dav"
}

// publishSecondAccount adds an account to the file the way the API does and
// republishes the configuration as a copy, which is what makes a hook that
// captured the original pointer go blind.
func publishSecondAccount(t *testing.T, col *control.Collector, cfg *config.Config, path, url string) {
	t.Helper()
	if err := config.AddRemote(path, "second", config.Remote{Type: "webdav", Extra: map[string]any{"url": url}}, config.AddRemoteOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	next := *cfg
	next.Remotes = fresh.Remotes
	col.PublishConfigView(&next)
}

// The control plane republishes the configuration after every edit, and it
// publishes a copy rather than rewriting the old one in place — a reader that
// had already taken the pointer would otherwise see one section from before the
// edit and another from after it. Every hook the daemon hands the collector has
// to read through that publication, or an account added through the API is
// invisible to the very next call about it.
//
// The account check asserts on success rather than on a message: the collector
// wraps it in SanitizeAccountError, which reduces every failure to one
// sentence, so "unknown remote" and "the server hung up" arrive identical and a
// test that pattern-matches the text cannot fail.
func TestTheDaemonsAccountHooksSeeAccountsAddedAfterTheyWereBuilt(t *testing.T) {
	url := davServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes:\n  first: {type: webdav, url: '" + url + "'}\nmounts: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Config: cfg}
	col := d.Collector()
	publishSecondAccount(t, col, cfg, path, url)

	if col.CheckAccount == nil {
		t.Fatal("the collector has no account check")
	}
	if err := col.CheckAccount(context.Background(), "second"); err != nil {
		t.Fatalf("the account check does not see the account that was just added: %v", err)
	}

	if col.AccountQuota == nil {
		t.Fatal("the collector has no quota hook")
	}
	if _, _, err := col.AccountQuota(context.Background(), "second"); err != nil {
		t.Fatalf("the quota hook does not see the account that was just added: %v", err)
	}

	if col.Auth == nil || col.Auth.OAuth == nil {
		t.Fatal("the collector has no authorization hook to check")
	}
	if _, _, err := col.Auth.OAuth(context.Background(), "second", func(string) {}); err != nil && strings.Contains(err.Error(), "unknown remote") {
		t.Fatalf("the authorization hook still reads the configuration it was built with: %v", err)
	}
}

// The diagnostics run over the accounts, so a Doctor holding the pointer the
// daemon started with silently reports on a subset: every drive added through
// the browser since start-up is missing from the credentials check, and the
// page says everything is fine because it never looked.
func TestTheDiagnosticsSeeAccountsAddedAfterTheDaemonStarted(t *testing.T) {
	url := davServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes:\n  first: {type: webdav, url: '" + url + "'}\nmounts: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Config: cfg}
	col := d.Collector()
	publishSecondAccount(t, col, cfg, path, url)

	if col.Doctor == nil || col.Doctor.Config == nil {
		t.Fatal("the collector has no diagnostics reading the configuration")
	}
	seen := col.Doctor.Config()
	if seen == nil {
		t.Fatal("the diagnostics read a nil configuration")
	}
	if _, ok := seen.Remotes["second"]; !ok {
		names := make([]string, 0, len(seen.Remotes))
		for n := range seen.Remotes {
			names = append(names, n)
		}
		t.Fatalf("the diagnostics see %v; the account added after start-up is missing", names)
	}
}

// The service unit bakes the mount directory into ExecStart, RequiresMountsFor
// and the path uninstall detaches. Built from the configuration the daemon
// started with, installing after the mount was edited writes a unit for the old
// directory — and a later uninstall detaches that one while the live mount
// stays attached.
func TestInstallingTheServiceUsesTheMountAsItIsNow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	before, after := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") +
		"\nremotes:\n  nas: {type: webdav, url: 'https://nas.local/dav'}\n" +
		"mounts:\n  - path: " + before + "\n    layout:\n      /: {remote: nas}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	restore := serviceRuntime
	t.Cleanup(func() { serviceRuntime = restore })
	serviceRuntime = func(out io.Writer) (service.Runtime, error) {
		return service.Runtime{
			GOOS: "linux", Home: home, ConfigDir: filepath.Join(home, ".config"),
			Executable: filepath.Join(dir, "cloudfs"), UID: 501, Out: out,
			Run:     func(string, ...string) ([]byte, error) { return nil, nil },
			Mounted: func(string) (bool, error) { return false, nil },
			Unmount: func(string) error { return nil },
		}, nil
	}

	d := &Daemon{Config: cfg}
	col := d.Collector()
	if col.Service == nil || col.Service.Install == nil {
		t.Fatal("the collector has no service control")
	}

	// The mount is edited through the API, which republishes a copy.
	if err := config.SetLayout(path, before, "/", config.Layout{Remote: "nas"}); err != nil {
		t.Fatal(err)
	}
	edited := *cfg
	edited.Mounts = []config.Mount{{Path: after, Layout: map[string]config.Layout{"/": {Remote: "nas", Mode: config.ModeWriteback}}}}
	col.PublishConfigView(&edited)

	if err := col.Service.Install(); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "cloudfs.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), after) {
		t.Fatalf("the unit does not mount %s; it was written from the configuration the daemon started with:\n%s", after, unit)
	}
	if strings.Contains(string(unit), before) {
		t.Fatalf("the unit still names the old mount %s:\n%s", before, unit)
	}
}
