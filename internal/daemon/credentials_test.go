package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"cloudfs/internal/config"
	_ "cloudfs/internal/provider/aliyun"
)

func TestRotatedTokenSurvivesDaemonRestart(t *testing.T) {
	var exchanges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/access_token":
			n := exchanges.Add(1)
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != fmt.Sprintf("private-refresh-%d", n-1) {
				t.Errorf("restart used old credential: exchange %d", n)
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": fmt.Sprintf("private-refresh-%d", n), "expires_in": 7200})
		case "/adrive/v1.0/openFile/list":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "next_marker": ""})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("cache:\n  dir: %q\nsecrets:\n  backend: file\nremotes:\n  ali:\n    type: aliyun\n    refresh_token: private-refresh-0\n    drive_id: test\n    base_url: %q\nmounts:\n  - path: /unused\n    layout:\n      /: {remote: ali, root: root}\n", filepath.Join(dir, "cache"), srv.URL)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Open(context.Background(), Options{Config: cfg, SkipWrite: true})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = d.Providers["ali"].List(context.Background(), "root", "")
		d.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "private-refresh-") || !strings.Contains(string(b), "secretfile:") {
			t.Fatal("credential was not migrated to private storage")
		}
	}
	if exchanges.Load() != 2 {
		t.Fatalf("exchanges=%d", exchanges.Load())
	}
}
