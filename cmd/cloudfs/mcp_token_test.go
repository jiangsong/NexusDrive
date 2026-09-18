package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"cloudfs/internal/agent"
)

var plainTokenRE = regexp.MustCompile(`cfs_[A-Za-z0-9_-]{20,}`)

func TestMCPTokenCreatePrintsThePlainTokenOnce(t *testing.T) {
	cfg, cfgPath := uploadCLIConfig(t)
	var out bytes.Buffer
	if err := runMCPToken(context.Background(), &out, []string{"create", "--config", cfgPath, "--name", "codex", "--read", "/work", "--write", "/work/.agent", "--ttl", "720h"}); err != nil {
		t.Fatal(err)
	}
	plain := plainTokenRE.FindString(out.String())
	if plain == "" {
		t.Fatalf("no token printed: %s", out.String())
	}
	if strings.Count(out.String(), plain) != 1 {
		t.Fatalf("the token must be printed exactly once:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "read /work") || !strings.Contains(out.String(), "write /work/.agent") {
		t.Fatalf("the summary must show the scope:\n%s", out.String())
	}
	out.Reset()
	if err := runMCPToken(context.Background(), &out, []string{"list", "--config", cfgPath}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), plain) || !strings.Contains(out.String(), plain[4:8]) {
		t.Fatalf("list must show the fingerprint and never the token: %s", out.String())
	}
	for _, want := range []string{"codex", "active", "/work/.agent", "FINGERPRINT"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list lacks %q:\n%s", want, out.String())
		}
	}
	// The store the daemon opens sees the same token, hashed.
	st, err := agent.Open(filepath.Join(cfg.StateDir(), "agent"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err := st.VerifyToken(context.Background(), plain)
	if err != nil || p.Name != "codex" || p.ExpiresAt.IsZero() {
		t.Fatalf("verify through the store: %+v %v", p, err)
	}
	out.Reset()
	if err := runMCPToken(context.Background(), &out, []string{"list", "--config", cfgPath, "--json"}); err != nil {
		t.Fatal(err)
	}
	var listed []agent.Principal
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].TokenPrefix != plain[4:8] {
		t.Fatalf("json list: %s %v", out.String(), err)
	}
	if bytes.Contains(out.Bytes(), []byte(plain)) || bytes.Contains(out.Bytes(), []byte("token_hash")) {
		t.Fatalf("json list leaks token material: %s", out.String())
	}
}

func TestMCPTokenCreateJSONCarriesTheTokenAndPrincipal(t *testing.T) {
	_, cfgPath := uploadCLIConfig(t)
	var out bytes.Buffer
	if err := runMCPToken(context.Background(), &out, []string{"create", "--config", cfgPath, "--name", "ci", "--read-only", "--json"}); err != nil {
		t.Fatal(err)
	}
	var created struct {
		Token     string          `json:"token"`
		Principal agent.Principal `json:"principal"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatalf("%s: %v", out.String(), err)
	}
	if !strings.HasPrefix(created.Token, agent.TokenPlainPrefix) || created.Principal.Name != "ci" || !created.Principal.Scope.ReadOnly || !created.Principal.ExpiresAt.IsZero() {
		t.Fatalf("%+v", created)
	}
}

func TestMCPTokenRevokeNeedsConfirm(t *testing.T) {
	_, cfgPath := uploadCLIConfig(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := runMCPToken(ctx, &out, []string{"create", "--config", cfgPath, "--name", "codex"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := runMCPToken(ctx, &out, []string{"revoke", "codex", "--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("revoke without --confirm: %v", err)
	}
	if err := runMCPToken(ctx, &out, []string{"list", "--config", cfgPath}); err != nil || strings.Contains(out.String(), "revoked") {
		t.Fatalf("a refused revoke must change nothing: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runMCPToken(ctx, &out, []string{"revoke", "codex", "--config", cfgPath, "--confirm"}); err != nil || !strings.Contains(out.String(), "revoked") {
		t.Fatalf("revoke: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runMCPToken(ctx, &out, []string{"list", "--config", cfgPath}); err != nil || !strings.Contains(out.String(), "revoked") {
		t.Fatalf("list after revoke: %s %v", out.String(), err)
	}
	if err := runMCPToken(ctx, &out, []string{"revoke", "codex", "--config", cfgPath, "--confirm"}); !errors.Is(err, agent.ErrPrincipalNotFound) {
		t.Fatalf("a revoked name is no longer a live token: %v", err)
	}
	if err := runMCPToken(ctx, &out, []string{"revoke", "--config", cfgPath, "--confirm"}); err == nil || !strings.Contains(err.Error(), "name or id") {
		t.Fatalf("revoke without a target: %v", err)
	}
}

func TestMCPTokenFlagsAreValidated(t *testing.T) {
	_, cfgPath := uploadCLIConfig(t)
	ctx := context.Background()
	var out bytes.Buffer
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", cfgPath}, "expected create, list or revoke"},
		{[]string{"rotate", "--config", cfgPath}, "unknown action"},
		{[]string{"create", "--config", cfgPath}, "--name is required"},
		{[]string{"create", "--config", cfgPath, "--name", "Bad Name"}, "must match"},
		{[]string{"create", "--config", cfgPath, "--name", "x", "--ttl", "soon"}, "--ttl"},
		{[]string{"create", "--config", cfgPath, "--name", "x", "--read", "/work", "--write", "/gd"}, "inside a read prefix"},
		{[]string{"create", "--config", cfgPath, "--name", "x", "--bogus", "1"}, "unknown flag"},
		{[]string{"list", "--config", cfgPath, "extra"}, "unexpected argument"},
	} {
		out.Reset()
		err := runMCPToken(ctx, &out, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
	// --ttl never and an empty --write are accepted spellings.
	if err := runMCPToken(ctx, &out, []string{"create", "--config", cfgPath, "--name", "ok", "--read", "/work,/docs", "--write=", "--ttl", "never"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runMCPToken(ctx, &out, []string{"list", "--config", cfgPath}); err != nil || !strings.Contains(out.String(), "/work,/docs") || !strings.Contains(out.String(), "none") {
		t.Fatalf("list: %s %v", out.String(), err)
	}
}

func TestMCPTokenArgsDropOnlyTheSubcommandWord(t *testing.T) {
	for _, tc := range []struct {
		in, want []string
	}{
		{[]string{"token", "create", "--name", "codex"}, []string{"create", "--name", "codex"}},
		{[]string{"--config", "c.yaml", "token", "list"}, []string{"--config", "c.yaml", "list"}},
		{[]string{"--read-only", "token", "create", "--name", "token"}, []string{"--read-only", "create", "--name", "token"}},
		{[]string{"token", "revoke", "token", "--confirm"}, []string{"revoke", "token", "--confirm"}},
	} {
		got := mcpTokenArgs(tc.in)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%v: got %v want %v", tc.in, got, tc.want)
		}
	}
}
