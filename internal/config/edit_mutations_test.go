package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editable writes a configuration the way a person would: with comments,
// which every editor here must leave alone.
func editable(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := `# my notes: keep this line
cache:
  dir: /tmp/cloudfs-cache # cache lives here
proxy:
  outbounds:
    - { name: hk, type: socks5, addr: 127.0.0.1:7890 }
  groups:
    - { name: auto, type: url-test, members: [hk] }
  rules:
    - FINAL,direct
remotes:
  gd: # the work drive
    type: gdrive
    client_id: abc
  nas:
    type: sftp
    host: nas.local
mounts:
  - path: /mnt/cloud
    layout:
      /gd: { remote: gd, mode: writeback }
      /nas: { remote: nas, mode: readonly }
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoad(t *testing.T, p string) *Config {
	t.Helper()
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustContain(t *testing.T, p string, wants ...string) {
	t.Helper()
	b, _ := os.ReadFile(p)
	for _, w := range wants {
		if !strings.Contains(string(b), w) {
			t.Fatalf("configuration lost %q:\n%s", w, b)
		}
	}
}

func str(s string) *string { return &s }

func TestSetRemoteFieldEditsPublicSettingsAndKeepsComments(t *testing.T) {
	p := editable(t)
	workers := 4
	err := SetRemoteField(p, "gd", SetRemoteFieldOptions{
		Fields:        map[string]*string{"drive_id": str("team-1"), "client_id": nil},
		Proxy:         str("auto"),
		QPS:           &QPS{Meta: 5, Download: 10, Upload: 2},
		UploadWorkers: &workers,
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mustLoad(t, p)
	gd := c.Remotes["gd"]
	if gd.Proxy != "auto" || gd.UploadWorkers != 4 || gd.QPS == nil || gd.QPS.Download != 10 || gd.Extra["drive_id"] != "team-1" {
		t.Fatalf("remote after edit: %+v", gd)
	}
	if _, still := gd.Extra["client_id"]; still {
		t.Fatal("a nil field value did not delete the key")
	}
	// The whole point of node surgery over re-marshalling.
	mustContain(t, p, "# my notes: keep this line", "# cache lives here", "# the work drive")

	// Back to defaults removes the keys rather than writing zero values.
	zero := 0
	if err := SetRemoteField(p, "gd", SetRemoteFieldOptions{Proxy: str(""), QPS: &QPS{}, UploadWorkers: &zero}); err != nil {
		t.Fatal(err)
	}
	c = mustLoad(t, p)
	if gd := c.Remotes["gd"]; gd.Proxy != "" || gd.QPS != nil || gd.UploadWorkers != 0 {
		t.Fatalf("resetting to default left values behind: %+v", gd)
	}
	// And the keys themselves are gone from the remote's block, not written
	// as zero values. The block runs from "gd:" to the next remote.
	b, _ := os.ReadFile(p)
	text := string(b)
	block := text[strings.Index(text, "  gd:"):strings.Index(text, "  nas:")]
	for _, gone := range []string{"proxy:", "qps:", "upload_workers:"} {
		if strings.Contains(block, gone) {
			t.Fatalf("resetting to default left %q in the remote block:\n%s", gone, block)
		}
	}
}

func TestSetRemoteFieldRefusesWhatARequestMustNotWrite(t *testing.T) {
	p := editable(t)
	before, _ := os.ReadFile(p)
	for name, opt := range map[string]SetRemoteFieldOptions{
		"secret":          {Fields: map[string]*string{"refresh_token": str("x")}},
		"structural key":  {Fields: map[string]*string{"type": str("s3")}},
		"injection slot":  {Fields: map[string]*string{"_http_client": str("x")}},
		"unknown proxy":   {Proxy: str("nowhere")},
		"control char":    {Fields: map[string]*string{"url": str("a\nb")}},
		"negative qps":    {QPS: &QPS{Meta: -1}},
		"too many worker": {UploadWorkers: func() *int { n := 1000; return &n }()},
	} {
		t.Run(name, func(t *testing.T) {
			if err := SetRemoteField(p, "gd", opt); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if err := SetRemoteField(p, "missing", SetRemoteFieldOptions{Proxy: str("hk")}); err == nil {
		t.Fatal("edited a remote that does not exist")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("a refused edit changed the file")
	}
}

func TestRemoveRemoteRefusesWhileMountedThenRemoves(t *testing.T) {
	p := editable(t)
	if err := RemoveRemote(p, "nas"); err == nil || !strings.Contains(err.Error(), "/mnt/cloud/nas") {
		t.Fatalf("removed a mounted remote, or did not say where it is mounted: %v", err)
	}
	if err := RemoveMount(p, "/mnt/cloud", "/nas"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRemote(p, "nas"); err != nil {
		t.Fatal(err)
	}
	c := mustLoad(t, p)
	if _, still := c.Remotes["nas"]; still {
		t.Fatal("remote survived removal")
	}
	if _, gone := c.Mounts[0].Layout["/gd"]; !gone || len(c.Mounts[0].Layout) != 1 {
		t.Fatalf("the other layout was disturbed: %+v", c.Mounts)
	}
	mustContain(t, p, "# my notes: keep this line", "# the work drive")
	if err := RemoveRemote(p, "nas"); err == nil {
		t.Fatal("removing twice succeeded")
	}
}

func TestMountEditorsAddReplaceAndRemoveLayouts(t *testing.T) {
	p := editable(t)
	if err := AddMount(p, "/mnt/cloud", "/gd", Layout{Remote: "gd"}); err == nil {
		t.Fatal("AddMount overwrote an existing prefix; that is SetLayout's job")
	}
	if err := AddMount(p, "/mnt/cloud", "/gd2", Layout{Remote: "gd", Root: "sub", Mode: ModeStrict}); err != nil {
		t.Fatal(err)
	}
	if err := AddMount(p, "/mnt/other", "/nas", Layout{Remote: "nas"}); err != nil {
		t.Fatal(err)
	}
	if err := AddMount(p, "/mnt/other", "relative", Layout{Remote: "nas"}); err == nil {
		t.Fatal("accepted a relative prefix")
	}
	// A newline in the mount path or prefix must not reach the YAML: it is the
	// classic injection an HTTP mounts API would otherwise pass straight
	// through. Both fields are validated at the write boundary.
	if err := AddMount(p, "/mnt/evil\ninjected: true", "/x", Layout{Remote: "nas"}); err == nil {
		t.Fatal("accepted a newline in the mount path")
	}
	if err := AddMount(p, "/mnt/other", "/x\ninjected: true", Layout{Remote: "nas"}); err == nil {
		t.Fatal("accepted a newline in the prefix")
	}
	if c := mustLoad(t, p); func() bool {
		for _, m := range c.Mounts {
			if strings.Contains(m.Path, "injected") {
				return true
			}
		}
		return false
	}() {
		t.Fatal("an injected value reached the configuration")
	}
	if err := AddMount(p, "/mnt/other", "/ghost", Layout{Remote: "ghost"}); err == nil {
		t.Fatal("accepted a layout for a remote that does not exist")
	}
	if err := AddMount(p, "/mnt/other", "/bad", Layout{Remote: "nas", Mode: "sometimes"}); err == nil {
		t.Fatal("accepted an unknown mode")
	}
	c := mustLoad(t, p)
	if len(c.Mounts) != 2 || c.Mounts[0].Layout["/gd2"].Mode != ModeStrict || c.Mounts[0].Layout["/gd2"].Root != "sub" || c.Mounts[1].Layout["/nas"].Mode != ModeWriteback {
		t.Fatalf("mounts after add: %+v", c.Mounts)
	}

	if err := SetLayout(p, "/mnt/cloud", "/gd2", Layout{Remote: "gd", Mode: ModeReadonly}); err != nil {
		t.Fatal(err)
	}
	if err := SetLayout(p, "/mnt/cloud", "/nope", Layout{Remote: "gd"}); err == nil {
		t.Fatal("SetLayout created a prefix that did not exist")
	}
	if mustLoad(t, p).Mounts[0].Layout["/gd2"].Mode != ModeReadonly {
		t.Fatal("SetLayout did not replace the mode")
	}

	// Removing the only layout under a mount removes the mount.
	if err := RemoveMount(p, "/mnt/other", "/nas"); err != nil {
		t.Fatal(err)
	}
	if c := mustLoad(t, p); len(c.Mounts) != 1 {
		t.Fatalf("an empty mount entry survived: %+v", c.Mounts)
	}
	if err := RemoveMount(p, "/mnt/other", "/nas"); err == nil {
		t.Fatal("removing a missing layout succeeded")
	}
	mustContain(t, p, "# my notes: keep this line", "# cache lives here")
}

func TestSetProxyReplacesTheSectionAndValidatesCrossReferences(t *testing.T) {
	p := editable(t)
	// A group that names an outbound the new section no longer has.
	err := SetProxy(p, Proxy{
		Outbounds: []Outbound{{Name: "sg", Type: "http", Addr: "gw:8443"}},
		Groups:    []Group{{Name: "auto", Type: "url-test", Members: []string{"hk"}}},
		Rules:     []string{"FINAL,direct"},
	})
	if err == nil || !strings.Contains(err.Error(), "hk") {
		t.Fatalf("a dangling member was accepted: %v", err)
	}
	// A remote still pointing at a name the new section drops.
	if err := SetRemoteField(p, "gd", SetRemoteFieldOptions{Proxy: str("auto")}); err != nil {
		t.Fatal(err)
	}
	if err := SetProxy(p, Proxy{Rules: []string{"FINAL,direct"}}); err == nil || !strings.Contains(err.Error(), "auto") {
		t.Fatalf("dropped a group a remote still uses: %v", err)
	}
	// A consistent replacement lands whole.
	err = SetProxy(p, Proxy{
		Outbounds: []Outbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}, {Name: "sg", Type: "http", Addr: "gw:8443"}},
		Groups:    []Group{{Name: "auto", Type: "url-test", Members: []string{"hk", "sg"}}},
		Rules:     []string{"DOMAIN-SUFFIX,googleapis.com,auto", "FINAL,direct"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mustLoad(t, p)
	if len(c.Proxy.Outbounds) != 2 || len(c.Proxy.Groups[0].Members) != 2 || len(c.Proxy.Rules) != 2 {
		t.Fatalf("proxy after replace: %+v", c.Proxy)
	}
	mustContain(t, p, "# my notes: keep this line", "# the work drive")
	// Emptying the section removes it rather than writing three empty lists.
	if err := SetRemoteField(p, "gd", SetRemoteFieldOptions{Proxy: str("")}); err != nil {
		t.Fatal(err)
	}
	if err := SetProxy(p, Proxy{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "proxy:") {
		t.Fatalf("an empty proxy section was written:\n%s", b)
	}
}
