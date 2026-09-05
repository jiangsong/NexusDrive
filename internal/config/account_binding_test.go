package config

import (
	"strings"
	"testing"
)

func TestEffectiveAccountBindingFencesAuthorizationAndLocatorNotTokenRefresh(t *testing.T) {
	generation := NewAccountBinding()
	base := Remote{Type: "webdav", AccountBinding: generation, Extra: map[string]any{
		"url": "https://one.invalid/dav", "user": "alice", "password": "secret-one",
	}}
	want, err := EffectiveAccountBinding(base)
	if err != nil || !strings.HasPrefix(want, effectiveBindingPrefix) {
		t.Fatalf("binding=%q %v", want, err)
	}
	rotatedToken := cloneRemote(base)
	rotatedToken.Extra["password"] = "secret-two"
	if got, err := EffectiveAccountBinding(rotatedToken); err != nil || got != want {
		t.Fatalf("token refresh changed binding: %q %v", got, err)
	}
	changedEndpoint := cloneRemote(base)
	changedEndpoint.Extra["url"] = "https://two.invalid/dav"
	if got, _ := EffectiveAccountBinding(changedEndpoint); got == want {
		t.Fatal("endpoint change retained binding")
	}
	changedGeneration := cloneRemote(base)
	changedGeneration.AccountBinding = NewAccountBinding()
	if got, _ := EffectiveAccountBinding(changedGeneration); got == want {
		t.Fatal("reauthorization generation retained binding")
	}
}

func TestLegacyAccountBindingIsDeterministicAndTokenPersisterFreezesIt(t *testing.T) {
	p := credentialsConfig(t)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want, err := EffectiveAccountBinding(c.Remotes["ali"])
	if err != nil || !strings.HasPrefix(want, legacyBindingPrefix) {
		t.Fatalf("legacy=%q %v", want, err)
	}
	if err := TokenPersister(c, "ali")(map[string]string{"refresh_token": "rotated"}); err != nil {
		t.Fatal(err)
	}
	after, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Remotes["ali"].AccountBinding != want {
		t.Fatalf("legacy generation not frozen: %q want %q", after.Remotes["ali"].AccountBinding, want)
	}
	if got, err := EffectiveAccountBinding(after.Remotes["ali"]); err != nil || got != want {
		t.Fatalf("rotation changed legacy binding: %q %v", got, err)
	}
}

func TestExplicitAuthorizationRotatesAccountBinding(t *testing.T) {
	p := credentialsConfig(t)
	if _, err := SaveCredentials(p, "ali", map[string]string{"refresh_token": "first"}); err != nil {
		t.Fatal(err)
	}
	first, _ := Load(p)
	generation := first.Remotes["ali"].AccountBinding
	if !ValidAccountBinding(generation) || !strings.HasPrefix(generation, accountBindingPrefix) {
		t.Fatalf("missing generated binding: %q", generation)
	}
	if _, err := SaveCredentials(p, "ali", map[string]string{"refresh_token": "second"}); err != nil {
		t.Fatal(err)
	}
	second, _ := Load(p)
	if second.Remotes["ali"].AccountBinding == generation {
		t.Fatal("explicit authorization did not rotate binding")
	}
}
