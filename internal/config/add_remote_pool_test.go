package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func poolJoinConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`remotes:
  ali: {type: aliyun}
  home: {type: pool, pool: home}
pools:
  home:
    members:
      - {remote: ali}
    replicas: 2
mounts:
  - path: /mnt/cloud
    layout:
      /: {remote: home}
`), 0o600)
	return path
}

// Creating an account and joining it to a pool were two writes, so a failure
// between them left a remote that exists and belongs to nothing — a state the
// person never asked for and the API had to admit to in its error text. One
// editConfig closure validates and renames once.
//
// The name says "in one write" rather than "both or neither" on purpose. Every
// way the join can now be refused is checked BEFORE the remote node is built —
// an unknown pool here, a pool remote and a duplicate member in
// appendPoolMember, and the whole-document Validate that editConfig runs on the
// result — so a refusal never gets far enough to need rolling back. The
// end-state half of the promise is asserted below and in
// TestAddRemoteRefusesAnUnknownPoolWithoutCreatingTheRemote: after a refused
// join there is no account. What no test can express through this API is the
// intermediate state, because the API no longer has one.
func TestAddRemoteWritesTheRemoteAndItsPoolMembershipInOneWrite(t *testing.T) {
	path := poolJoinConfig(t)
	if err := AddRemote(path, "gd", Remote{Type: "gdrive"}, AddRemoteOptions{Pool: "home"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Remotes["gd"]; !ok {
		t.Fatal("the remote was not written")
	}
	var joined bool
	for _, m := range c.Pools["home"].Members {
		if m.Remote == "gd" {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("the remote did not join the pool: %+v", c.Pools["home"].Members)
	}

	// And the end state after a refused join: no account, no member, nothing
	// half-written for someone to find later and wonder about.
	err = AddRemote(path, "nested", Remote{Type: PoolType, Extra: map[string]any{"pool": "home"}}, AddRemoteOptions{Pool: "home"})
	if err == nil {
		t.Fatal("a pool remote was accepted as a member of another pool")
	}
	if c, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Remotes["nested"]; ok {
		t.Fatal("the remote survived a join that failed; the two writes are not one transaction")
	}
}

// The pool has to exist before the remote is written, or a typo would create
// an account bound to nothing and report a failure at the same time.
func TestAddRemoteRefusesAnUnknownPoolWithoutCreatingTheRemote(t *testing.T) {
	path := poolJoinConfig(t)
	err := AddRemote(path, "gd", Remote{Type: "gdrive"}, AddRemoteOptions{Pool: "nope"})
	if err == nil {
		t.Fatal("an unknown pool was accepted")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the refusal does not name the pool: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Remotes["gd"]; ok {
		t.Fatal("the remote was created even though the join failed")
	}
}

// A pool exposed as a remote is still a remote, and joining it to another pool
// would nest them. The guard lived inside AddPoolMember; extracting the
// surgery so AddRemote can call it must not leave the guard behind.
func TestAPoolStillCannotBeAddedAsAMemberOfAnotherPool(t *testing.T) {
	path := poolJoinConfig(t)
	os.WriteFile(path, []byte(`remotes:
  ali: {type: aliyun}
  nas: {type: webdav, url: 'https://nas/dav'}
  home: {type: pool, pool: home}
  spare: {type: pool, pool: other}
pools:
  home:
    members:
      - {remote: ali}
    replicas: 2
  other:
    members:
      - {remote: nas}
    replicas: 1
mounts:
  - path: /mnt/cloud
    layout:
      /: {remote: home}
`), 0o600)
	if err := AddPoolMember(path, "other", PoolMember{Remote: "home"}); err == nil {
		t.Fatal("a pool was accepted as a member of another pool")
	}
	err := AddRemote(path, "nested", Remote{Type: PoolType, Extra: map[string]any{"pool": "home"}}, AddRemoteOptions{Pool: "other"})
	if err == nil {
		t.Fatal("a new pool remote was accepted as a member of another pool")
	}
	if !strings.Contains(err.Error(), "nest") {
		t.Errorf("the refusal does not explain that pools do not nest: %v", err)
	}
}
