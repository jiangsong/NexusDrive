package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPoolEditsKeepCommentsAndValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`# my drives
remotes:
  ali: {type: aliyun}   # the big one
  gd:  {type: gdrive}
  nas: {type: webdav, url: 'https://nas/dav'}
mounts:
  - path: /mnt/cloud
    layout:
      /gd: {remote: gd}
`), 0o600)
	if err := CreatePool(path, "home", []PoolMember{{Remote: "ali"}, {Remote: "gd", Root: "/cloudfs"}}, 3, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := AddPoolMember(path, "home", PoolMember{Remote: "nas", Weight: 2}); err != nil {
		t.Fatal(err)
	}
	if err := SetPoolField(path, "home", "replicas", "2"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pools["home"]
	if len(p.Members) != 3 || p.Members[1].Root != "/cloudfs" || p.Members[2].Weight != 2 || p.Replicas != 2 || c.Remotes["home"].PoolOf() != "home" {
		t.Fatalf("pool after edits = %+v, remote %+v", p, c.Remotes["home"])
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# my drives") || !strings.Contains(string(b), "# the big one") {
		t.Fatalf("comments lost:\n%s", b)
	}
	for _, bad := range []func() error{
		func() error { return AddPoolMember(path, "home", PoolMember{Remote: "ali"}) },
		func() error { return AddPoolMember(path, "home", PoolMember{Remote: "home"}) },
		func() error { return AddPoolMember(path, "nope", PoolMember{Remote: "ali"}) },
		func() error { return SetPoolField(path, "home", "min_replicas", "9") },
		func() error { return SetPoolField(path, "home", "colour", "blue") },
		func() error { return CreatePool(path, "home", []PoolMember{{Remote: "ali"}}, 1, 1, "") },
		func() error { return CreatePool(path, "bad name", []PoolMember{{Remote: "ali"}}, 1, 1, "") },
	} {
		if err := bad(); err == nil {
			t.Fatal("a bad edit was accepted")
		}
	}
	if err := RemovePoolMember(path, "home", "nas"); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(path)
	if len(c.Pools["home"].Members) != 2 {
		t.Fatalf("members after remove = %+v", c.Pools["home"].Members)
	}
	if err := RemovePoolMember(path, "home", "ali"); err != nil {
		t.Fatal(err)
	}
	if err := RemovePoolMember(path, "home", "gd"); err == nil {
		t.Fatal("removing the last member should be refused")
	}
}

func TestPoolRuleAndMemberClassEditsKeepCommentsAndValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`# my drives
remotes:
  ali: {type: aliyun}   # the big one
  gd:  {type: gdrive}
mounts: []
`), 0o600)
	if err := CreatePool(path, "home", []PoolMember{{Remote: "ali"}, {Remote: "gd", Root: "/cloudfs"}}, 3, 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := SetPoolMemberField(path, "home", "ali", "class", "fast, cheap"); err != nil {
		t.Fatal(err)
	}
	if err := SetPoolMemberField(path, "home", "gd", "class", "fast"); err != nil {
		t.Fatal(err)
	}
	if err := AddPoolRule(path, "home", PoolRule{Prefix: "/photos", Replicas: 2, Prefer: []string{"fast"}, Avoid: []string{"cheap"}}); err != nil {
		t.Fatal(err)
	}
	if err := AddPoolRule(path, "home", PoolRule{Prefix: "/video", Require: []string{"fast"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetPoolField(path, "home", "write_mode", "strict"); err != nil {
		t.Fatal(err)
	}
	if err := SetPoolField(path, "home", "failure_domain", "provider"); err != nil {
		t.Fatal(err)
	}
	if err := SetPoolField(path, "home", "min_replicas_timeout", "90s"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pools["home"]
	if len(p.Members[0].Class) != 2 || p.Members[0].Class[0] != "fast" || p.Members[0].Class[1] != "cheap" {
		t.Fatalf("ali class = %v", p.Members[0].Class)
	}
	if len(p.Rules) != 2 || p.Rules[0].Prefix != "/photos" || p.Rules[0].Replicas != 2 || p.Rules[1].Prefix != "/video" {
		t.Fatalf("rules = %+v", p.Rules)
	}
	if p.WriteMode != "strict" || p.FailureDomain != "provider" || p.MinReplicasTimeout != 90*time.Second {
		t.Fatalf("scalar fields = %+v", p)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "# my drives") || !strings.Contains(string(b), "# the big one") {
		t.Fatalf("comments lost:\n%s", b)
	}
	if err := RemovePoolRule(path, "home", "/video"); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(path)
	if len(c.Pools["home"].Rules) != 1 {
		t.Fatalf("rules after remove = %+v", c.Pools["home"].Rules)
	}
	// The remaining /photos rule avoids "cheap", so clearing ali's class
	// (its only holder) must be dropped first, or the resulting document
	// would reference a class no member declares.
	if err := RemovePoolRule(path, "home", "/photos"); err != nil {
		t.Fatal(err)
	}
	// Clearing a member's class list drops the key rather than writing an
	// empty sequence.
	if err := SetPoolMemberField(path, "home", "ali", "class", ""); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(path)
	if len(c.Pools["home"].Members[0].Class) != 0 {
		t.Fatalf("ali class after clearing = %v", c.Pools["home"].Members[0].Class)
	}
	if err := AddPoolRule(path, "home", PoolRule{Prefix: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func() error{
		func() error { return AddPoolRule(path, "home", PoolRule{Prefix: "photos"}) },
		func() error { return AddPoolRule(path, "home", PoolRule{Prefix: "/tmp"}) },
		func() error { return AddPoolRule(path, "nope", PoolRule{Prefix: "/x"}) },
		func() error { return AddPoolRule(path, "home", PoolRule{Prefix: "/x", Prefer: []string{"nope"}}) },
		func() error { return RemovePoolRule(path, "home", "/does-not-exist") },
		func() error { return RemovePoolRule(path, "nope", "/x") },
		func() error { return SetPoolMemberField(path, "home", "ali", "colour", "blue") },
		func() error { return SetPoolMemberField(path, "home", "nope", "class", "fast") },
		func() error { return SetPoolField(path, "home", "write_mode", "yolo") },
		func() error { return SetPoolField(path, "home", "failure_domain", "datacenter") },
		func() error { return SetPoolField(path, "home", "min_replicas_timeout", "not-a-duration") },
	} {
		if err := bad(); err == nil {
			t.Fatal("a bad edit was accepted")
		}
	}
}
