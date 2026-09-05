package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRemoteCreatesAndPreservesConfiguration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new", "config.yaml")
	if err := AddRemote(p, "nas", Remote{Type: "webdav", Extra: map[string]any{"url": "https://example.invalid/dav"}}, AddRemoteOptions{MountPath: "/mnt/cloud"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Remotes["nas"].Type != "webdav" || c.Mounts[0].Layout["/nas"].Mode != ModeWriteback {
		t.Fatalf("config=%+v", c)
	}
	if !ValidAccountBinding(c.Remotes["nas"].AccountBinding) {
		t.Fatalf("missing account generation: %q", c.Remotes["nas"].AccountBinding)
	}
	before, _ := os.ReadFile(p)
	if err := AddRemote(p, "nas", Remote{Type: "sftp"}, AddRemoteOptions{}); err == nil {
		t.Fatal("overwrote remote")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("duplicate changed config")
	}
	for _, prefix := range []string{"relative", "/a/../b", "//a", "/a/"} {
		if err := AddRemote(p, "bad", Remote{Type: "sftp"}, AddRemoteOptions{MountPath: "/mnt/cloud", Prefix: prefix}); err == nil {
			t.Fatalf("accepted prefix %s", prefix)
		}
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal("config permissions")
	}
}

func credentialsConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	b := "# preserve me\nsecrets: {backend: file}\nremotes:\n  ali:\n    type: aliyun\n    refresh_token: old-refresh # preserve credential comment\n    access_token: old-access\n    client_secret: app-secret\n"
	if err := os.WriteFile(p, []byte(b), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSaveCredentialsMigratesAndReplacesAccessIdentity(t *testing.T) {
	p := credentialsConfig(t)
	if fallback, err := SaveCredentials(p, "ali", map[string]string{"refresh_token": "new-refresh"}); err != nil || !fallback {
		t.Fatalf("save=%v %v", fallback, err)
	}
	b, _ := os.ReadFile(p)
	for _, secret := range []string{"old-refresh", "old-access", "new-refresh", "app-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("inline secret remained")
		}
	}
	if !strings.Contains(string(b), "preserve credential comment") {
		t.Fatal("lost comment")
	}
	c, _ := Load(p)
	r, err := NewSecretStore(c).ResolveRemote(c.Remotes["ali"])
	if err != nil || r.Extra["refresh_token"] != "new-refresh" || r.Extra["client_secret"] != "app-secret" {
		t.Fatalf("resolve failed: %v", err)
	}
	if _, ok := r.Extra["access_token"]; ok {
		t.Fatal("new refresh token retained old access identity")
	}
}

func TestRotationCannotOverwriteNewAuthorization(t *testing.T) {
	p := credentialsConfig(t)
	c, _ := Load(p)
	persist := TokenPersister(c, "ali")
	if err := persist(map[string]string{"refresh_token": "rotated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredentials(p, "ali", map[string]string{"refresh_token": "reauthorized"}); err != nil {
		t.Fatal(err)
	}
	if err := persist(map[string]string{"refresh_token": "stale-daemon"}); !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("stale save=%v", err)
	}
	c, _ = Load(p)
	r, err := NewSecretStore(c).ResolveRemote(c.Remotes["ali"])
	if err != nil || r.Extra["refresh_token"] != "reauthorized" {
		t.Fatal("old daemon replaced new authorization")
	}
}

func TestImportedReferenceLookingTokenIsStoredLiterally(t *testing.T) {
	p := credentialsConfig(t)
	if _, err := SaveCredentials(p, "ali", map[string]string{"refresh_token": "secretfile:literal-token"}); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(p)
	r, err := NewSecretStore(c).ResolveRemote(c.Remotes["ali"])
	if err != nil || r.Extra["refresh_token"] != "secretfile:literal-token" {
		t.Fatalf("reference-looking token: %v", err)
	}
}

func TestCredentialSizeGuardsRunBeforeKeyring(t *testing.T) {
	c := Default()
	c.Secrets = Secrets{Backend: "keyring", Dir: t.TempDir()}
	s := NewSecretStore(&c)
	s.set = func(string, string, string) error { t.Error("unsafe keyring call"); return nil }
	if err := s.Update("keyring:r/token", strings.Repeat("s", 2001)); err == nil {
		t.Fatal("accepted oversized keyring value")
	}
	if _, err := s.Put(strings.Repeat("k", 513), "value"); err == nil {
		t.Fatal("accepted oversized key")
	}
	if _, err := s.Put("key", strings.Repeat("s", (1<<20)+1)); err == nil {
		t.Fatal("accepted unreadable file value")
	}
}

func TestAccessOnlyImportCannotRefreshIntoPreviousAccount(t *testing.T) {
	p := credentialsConfig(t)
	if _, err := SaveCredentials(p, "ali", map[string]string{"access_token": "new-account-access"}); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(p)
	r, err := NewSecretStore(c).ResolveRemote(c.Remotes["ali"])
	if err != nil || r.Extra["access_token"] != "new-account-access" {
		t.Fatal("access import failed")
	}
	if _, ok := r.Extra["refresh_token"]; ok {
		t.Fatal("new account could fall back to old refresh token")
	}
}

func TestAuthorizationRefusesConcurrentRemoteEdit(t *testing.T) {
	p := credentialsConfig(t)
	c, _ := Load(p)
	if err := UpdateRemoteFields(p, "ali", map[string]string{"client_id": "another-app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredentialsForRemote(p, "ali", map[string]string{"refresh_token": "new-grant"}, c.Remotes["ali"]); !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("concurrent auth=%v", err)
	}
	after, _ := Load(p)
	if after.Remotes["ali"].Extra["refresh_token"] != "old-refresh" {
		t.Fatal("authorization overwritten after account edit")
	}
}
