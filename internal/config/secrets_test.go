package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSecretFilePermissionsAndReferences(t *testing.T) {
	c := Default()
	c.Secrets = Secrets{Backend: "file", Dir: t.TempDir()}
	s := NewSecretStore(&c)
	ref, err := s.Put("../remote/cookie", "top-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ref); err != nil || got != "top-secret" {
		t.Fatalf("get: %q %v", got, err)
	}
	st, _ := os.Stat(s.file("../remote/cookie"))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	if err := s.Update(ref, "rotated"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveRemote(Remote{Extra: map[string]any{"cookie": ref, "root_id": "0"}})
	if err != nil || got.Extra["cookie"] != "rotated" {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	if err := os.Chmod(s.file("../remote/cookie"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ref); err == nil {
		t.Fatal("accepted publicly readable credential")
	}
}

func TestSecretStoreAutoFallbackIsExplicit(t *testing.T) {
	c := Default()
	c.Secrets.Dir = t.TempDir()
	s := NewSecretStore(&c)
	s.set = func(string, string, string) error { return errors.New("no keyring") }
	ref, err := s.Put("r/token", "secret")
	if err != nil || !strings.HasPrefix(ref, "secretfile:") {
		t.Fatalf("fallback: %s %v", ref, err)
	}
	s.backend = "keyring"
	if _, err := s.Put("r/token", "secret"); err == nil {
		t.Fatal("explicit keyring silently fell back")
	}
	// A missing keyring item must never read a stale fallback file.
	s.get = func(string, string) (string, error) { return "", errors.New("missing") }
	if _, err := s.Get("keyring:r/token"); err == nil {
		t.Fatal("missing keyring credential was accepted")
	}
}

func TestConcurrentConfigUpdatesPreserveOtherFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	initial := "# keep this comment\nremotes:\n  demo:\n    type: webdav\n    url: https://example.invalid\n"
	if err := os.WriteFile(p, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := UpdateRemoteFields(p, "demo", map[string]string{fmt.Sprintf("field%d", i): "reference"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Remotes["demo"].Extra) != 13 {
		t.Fatalf("lost updates: %+v", c.Remotes["demo"])
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "keep this comment") {
		t.Fatal("lost comment")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}
