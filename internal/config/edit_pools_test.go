package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
