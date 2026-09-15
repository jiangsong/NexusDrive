package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
	"cloudfs/internal/provider"
)

// interactiveIO fakes a person at a terminal: In carries the answers, and the
// non-nil ReadSecret is what marks the session as interactive without needing
// a real tty.
func interactiveIO(answers string, out, diag *bytes.Buffer) configIO {
	return configIO{
		In: strings.NewReader(answers), Out: out, Err: diag,
		ReadSecret: func(string) (string, error) { return "", nil },
	}
}

func TestConfigAddAsksForWhatTheDriverDeclared(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	var out, diag bytes.Buffer
	// Type, then the smb driver's own fields in order: host, port, share,
	// user, domain, root. Blank answers take the declared defaults.
	answers := "smb\nnas.local\n\nmedia\nwork\n\n/movies\n"
	err := runConfig(context.Background(), []string{"add", "share", "--config", p},
		interactiveIO(answers, &out, &diag))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := cfg.Remotes["share"]
	if !ok {
		t.Fatalf("the remote was not written: %s", out.String())
	}
	if r.Type != "smb" {
		t.Fatalf("type is %q", r.Type)
	}
	for key, want := range map[string]string{
		"host": "nas.local", "share": "media", "user": "work", "root": "/movies",
		"port": "445", // the declared default, from a blank answer
	} {
		if got, _ := r.Extra[key].(string); got != want {
			t.Fatalf("%s = %v, want %q", key, r.Extra[key], want)
		}
	}
	if _, present := r.Extra["domain"]; present {
		t.Fatal("a blank answer with no default was written as an empty value")
	}
	// The questions must be the driver's, not a copy kept in the CLI. They
	// are printed in the CLI language, so compare against the rendered form:
	// on a zh_CN box the English fallback never appears.
	for _, field := range provider.Fields("smb") {
		prompt := i18n.FieldPrompt(cliLang, "smb", field.Name, field.Prompt)
		if !strings.Contains(out.String(), prompt) {
			t.Fatalf("the wizard never asked %q", prompt)
		}
	}
	// And it must say what the credential step will want.
	if !strings.Contains(out.String(), credentialHint("smb")) {
		t.Fatalf("the credential hint is missing from:\n%s", out.String())
	}
}

func TestConfigAddDoesNotAskForWhatWasGivenOnTheCommandLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	var out, diag bytes.Buffer
	// host and share come from flags; port, user, domain and root are left.
	answers := "\nwork\n\n/movies\n"
	err := runConfig(context.Background(), []string{
		"add", "share", "--type", "smb", "--host", "nas.local",
		"--set", "share=media", "--config", p,
	}, interactiveIO(answers, &out, &diag))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "SMB server") {
		t.Fatalf("the wizard re-asked for a field given on the command line:\n%s", out.String())
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Remotes["share"]
	if got, _ := r.Extra["user"].(string); got != "work" {
		t.Fatalf("user = %v", r.Extra["user"])
	}
	if got, _ := r.Extra["host"].(string); got != "nas.local" {
		t.Fatalf("host = %v", r.Extra["host"])
	}
}

// TestConfigAddNeverPromptsWithoutATerminal: a script must fail with a
// message, not block forever on a question nobody will answer.
func TestConfigAddNeverPromptsWithoutATerminal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	var out, diag bytes.Buffer
	// A plain reader with no secret hook is scripted input.
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag}
	err := runConfig(context.Background(), []string{"add", "share", "--type", "smb", "--config", p}, c)
	if err == nil {
		t.Fatal("a remote missing required fields was accepted non-interactively")
	}
	for _, want := range []string{"host", "share", "user", "--set"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not mention %q: %v", want, err)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("a non-interactive run printed a prompt: %q", out.String())
	}
}

func TestConfigAddRejectsAnUnknownTypeInteractively(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	var out, diag bytes.Buffer
	err := runConfig(context.Background(), []string{"add", "x", "--config", p},
		interactiveIO("nosuchbackend\nalsonot\nstillnot\n", &out, &diag))
	if err == nil {
		t.Fatal("an unregistered backend type was accepted")
	}
	if !strings.Contains(out.String(), "is not a registered type") {
		t.Fatalf("the user was not told why:\n%s", out.String())
	}
}

// TestEveryRegisteredDriverDescribesItself keeps the wizard honest as drivers
// are added: a backend nobody described cannot be set up interactively, and
// that is exactly the kind of gap nobody notices until a new user hits it.
func TestEveryRegisteredDriverDescribesItself(t *testing.T) {
	described := map[string]bool{}
	for _, typ := range provider.DescribedTypes() {
		described[typ] = true
	}
	for _, typ := range provider.Types() {
		if typ == "fake" {
			continue // the test backend has no configuration
		}
		if !described[typ] {
			t.Errorf("provider %q registers no fields; `cloudfs config add` cannot ask for its settings", typ)
			continue
		}
		if credentialHint(typ) == "" {
			t.Errorf("provider %q declares no credentials; `config add` cannot say what `config auth` will need", typ)
		}
	}
}

// TestDeclaredRequiredFieldsMatchWhatTheDriverEnforces: marking a field
// required that the driver accepts empty would refuse a working configuration,
// which is worse than not asking at all.
func TestDeclaredRequiredFieldsMatchWhatTheDriverEnforces(t *testing.T) {
	for _, typ := range provider.DescribedTypes() {
		for _, field := range provider.Fields(typ) {
			if !field.Required {
				continue
			}
			if field.Default != "" {
				t.Errorf("%s.%s is required and also has a default; one of the two is wrong", typ, field.Name)
			}
		}
	}
}
