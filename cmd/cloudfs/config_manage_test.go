package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/config"
)

func configTestPath(t *testing.T, remotes string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	b := fmt.Sprintf("# keep config comments\ncache:\n  dir: %q\nsecrets: {backend: file}\nremotes:\n%s\n", filepath.Join(dir, "cache"), remotes)
	if err := os.WriteFile(p, []byte(b), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigAddListAndTypedSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag}
	args := []string{"add", "nas", "--type", "sftp", "--host", "nas.example", "--client-id", "0012", "--set", "port=2222", "--mount", "/mnt/cloud", "--prefix", "/nas", "--config", p}
	if err := runConfig(context.Background(), args, c); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remotes["nas"].Extra["port"] != 2222 || cfg.Remotes["nas"].Extra["client_id"] != "0012" {
		t.Fatalf("typed fields=%v", cfg.Remotes["nas"].Extra)
	}
	out.Reset()
	if err := runConfig(context.Background(), []string{"list", "--json", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"name":"nas"`) {
		t.Fatalf("list=%s", out.String())
	}
	before, _ := os.ReadFile(p)
	if err := runConfig(context.Background(), args, c); err == nil {
		t.Fatal("duplicate add succeeded")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("duplicate changed config")
	}
}

func TestConfigAuthImportsAndChecksWithoutMountOrCache(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "private-password" || r.Method != "PROPFIND" {
			t.Error("incorrect account check")
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		w.Write([]byte(`<d:multistatus xmlns:d="DAV:"/>`))
	}))
	defer srv.Close()
	p := configTestPath(t, fmt.Sprintf("  nas: {type: webdav, url: %q, user: alice}", srv.URL))
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(`{"pass":"private-password"}`), Out: &out, Err: &diag}
	if err := runConfig(context.Background(), []string{"auth", "nas", "--stdin", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("check requests=%d", requests.Load())
	}
	if strings.Contains(out.String()+diag.String(), "private-password") {
		t.Fatal("credential printed")
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "private-password") || !strings.Contains(string(b), "secretfile:") {
		t.Fatal("inline credential remained")
	}
	cfg, _ := config.Load(p)
	if _, err := os.Stat(cfg.Cache.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check created cache: %v", err)
	}
	out.Reset()
	if err := runConfig(context.Background(), []string{"list", "--json", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-password") || !strings.Contains(out.String(), `"pass":"secretfile"`) {
		t.Fatalf("credential summary=%s", out.String())
	}
}

func TestConfigMigrateIsOfflineAndInvalidImportDoesNotWrite(t *testing.T) {
	p := configTestPath(t, "  nas: {type: webdav, url: 'https://example.invalid/dav', pass: private-password}")
	legacy, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	wantBinding, err := config.EffectiveAccountBinding(legacy.Remotes["nas"])
	if err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag}
	if err := runConfig(context.Background(), []string{"auth", "nas", "--migrate", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	migrated, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := config.EffectiveAccountBinding(migrated.Remotes["nas"]); err != nil || got != wantBinding {
		t.Fatalf("credential migration changed account binding: %q want %q (%v)", got, wantBinding, err)
	}
	before, _ := os.ReadFile(p)
	for _, input := range []string{`{"not_a_secret":"private-value"}`, `{"pass":""}`, `{"pass":"one","pass":"two"}`, "pass: private-value\n---\npass: ignored"} {
		c.In = strings.NewReader(input)
		if err := runConfig(context.Background(), []string{"auth", "nas", "--stdin", "--config", p}, c); err == nil {
			t.Fatal("accepted invalid import")
		}
		after, _ := os.ReadFile(p)
		if string(before) != string(after) {
			t.Fatal("invalid import changed config")
		}
	}
	if strings.Contains(out.String()+diag.String(), "private-value") {
		t.Fatal("error leaked credential input")
	}
}

func TestConfigAuthPersistsRotationEvenWhenCheckFails(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/access_token" {
			exchanges.Add(1)
			w.Write([]byte(`{"access_token":"private-access","refresh_token":"private-rotated","expires_in":7200}`))
			return
		}
		w.WriteHeader(403)
		w.Write([]byte(`{"code":"Forbidden","message":"private-access private-rotated"}`))
	}))
	defer srv.Close()
	p := configTestPath(t, fmt.Sprintf("  ali: {type: aliyun, drive_id: drive, base_url: %q}", srv.URL))
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(`{"refresh_token":"private-initial"}`), Out: &out, Err: &diag}
	err := runConfig(context.Background(), []string{"auth", "ali", "--stdin", "--config", p}, c)
	if err == nil || !strings.Contains(err.Error(), "credentials are saved") {
		t.Fatalf("check failure=%v", err)
	}
	if strings.Contains(err.Error()+out.String()+diag.String(), "private-") {
		t.Fatal("check error exposed tokens")
	}
	cfg, _ := config.Load(p)
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["ali"])
	if err != nil || r.Extra["refresh_token"] != "private-rotated" || exchanges.Load() != 1 {
		t.Fatalf("rotated credential lost: exchanges=%d err=%v", exchanges.Load(), err)
	}
}

func TestConfigBrowserAuthorizationThroughTokenStorageAndCheck(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/access_token" {
			n := exchanges.Add(1)
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if n == 1 && body["code"] != "private-code" {
				t.Error("missing authorization code")
			}
			if n == 2 && body["refresh_token"] != "private-initial" {
				t.Error("check used wrong refresh token")
			}
			refresh := "private-initial"
			if n == 2 {
				refresh = "private-rotated"
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "private-access", "refresh_token": refresh, "expires_in": 7200})
			return
		}
		w.Write([]byte(`{"items":[],"next_marker":""}`))
	}))
	defer srv.Close()
	p := configTestPath(t, fmt.Sprintf("  ali:\n    type: aliyun\n    client_id: app\n    drive_id: drive\n    base_url: %q\n    oauth_authorize_url: %q\n    oauth_token_url: %q\n", srv.URL, srv.URL+"/authorize", srv.URL+"/oauth/access_token"))
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag, ReadSecret: func(string) (string, error) { return "private-secret", nil }}
	c.OpenURL = func(ctx context.Context, address string) error {
		u, _ := url.Parse(address)
		q := u.Query()
		if q.Get("style") != "folder" {
			t.Error("missing Aliyun folder grant style")
		}
		resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"private-code"}}.Encode())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := runConfig(context.Background(), []string{"auth", "ali", "--redirect-uri", "http://127.0.0.1:0/callback", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() != 2 {
		t.Fatalf("token exchanges=%d", exchanges.Load())
	}
	if strings.Contains(out.String()+diag.String(), "private-") {
		t.Fatal("authorization printed secret/code/token")
	}
	cfg, _ := config.Load(p)
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["ali"])
	if err != nil || r.Extra["refresh_token"] != "private-rotated" || r.Extra["client_secret"] != "private-secret" {
		t.Fatalf("saved credentials failed: %v", err)
	}
}

func TestCanceledBrowserAuthorizationLeavesConfigUntouched(t *testing.T) {
	p := configTestPath(t, "  ali: {type: aliyun, client_id: app}")
	before, _ := os.ReadFile(p)
	var out, diag bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag, ReadSecret: func(string) (string, error) { return "private-secret", nil }, OpenURL: func(context.Context, string) error { cancel(); return nil }}
	err := runConfig(ctx, []string{"auth", "ali", "--redirect-uri", "http://127.0.0.1:0/callback", "--config", p}, c)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("canceled auth wrote config")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "secrets")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled auth stored secrets: %v", err)
	}
}

func TestConfigAuthRawInputAndNoCheck(t *testing.T) {
	p := configTestPath(t, "  p115: {type: pan115}")
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader("private-refresh\n"), Out: &out, Err: &diag}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runConfig(ctx, []string{"auth", "p115", "--stdin", "--field", "refresh_token", "--no-check", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(p)
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["p115"])
	if err != nil || r.Extra["refresh_token"] != "private-refresh" {
		t.Fatal("raw input not stored correctly")
	}
}

func TestConfig115QRCodeAuthorizationAndCheck(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			w.Write([]byte(`{"state":1,"data":{"uid":"u","sign":"s","qrcode":"https://115.example/scan"}}`))
		case "/poll":
			w.Write([]byte(`{"state":1,"data":{"status":2}}`))
		case "/token":
			w.Write([]byte(`{"state":1,"data":{"access_token":"private-access","refresh_token":"private-initial"}}`))
		case "/open/refreshToken":
			refreshes.Add(1)
			r.ParseForm()
			if r.Form.Get("refresh_token") != "private-initial" {
				t.Error("check used wrong token")
			}
			w.Write([]byte(`{"state":true,"data":{"access_token":"private-new-access","refresh_token":"private-rotated","expires_in":7200}}`))
		case "/open/ufile/files":
			w.Write([]byte(`{"state":true,"data":[],"count":0}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := configTestPath(t, fmt.Sprintf("  p115:\n    type: pan115\n    client_id: app\n    base_url: %q\n    passport_url: %q\n    oauth_device_url: %q\n    oauth_poll_url: %q\n    oauth_token_url: %q\n", srv.URL, srv.URL, srv.URL+"/device", srv.URL+"/poll", srv.URL+"/token"))
	var out, diag bytes.Buffer
	c := configIO{In: strings.NewReader(""), Out: &out, Err: &diag}
	if err := runConfig(context.Background(), []string{"auth", "p115", "--config", p}, c); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 || !strings.Contains(out.String(), "Scan this QR") || !strings.Contains(out.String(), "█") {
		t.Fatalf("QR flow incomplete: refresh=%d", refreshes.Load())
	}
	if strings.Contains(out.String()+diag.String(), "private-") {
		t.Fatal("QR flow printed tokens")
	}
	cfg, _ := config.Load(p)
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["p115"])
	if err != nil || r.Extra["refresh_token"] != "private-rotated" {
		t.Fatalf("QR rotated token not saved: %v", err)
	}
}
