package upload

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
	"cloudfs/internal/provider/pan115"
)

// Exercise the real driver through the journal/uploader and instrumentation;
// injecting the callback directly into the driver would miss the wiring bug.
func TestQueued115UploadAnswersContentChallenge(t *testing.T) {
	f := newFixture(t)
	content := []byte(strings.Repeat("cloudfs-upload-", 30))
	f.queue(t, "proof.bin", content, "")
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/open/upload/init":
			rounds++
			r.ParseForm()
			if rounds == 1 {
				data = map[string]any{"status": 7, "statuscode": 701, "sign_key": "challenge", "sign_check": "10-19"}
			} else {
				want := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum(content[10:20])))
				if r.Form.Get("sign_key") != "challenge" || r.Form.Get("sign_val") != want {
					t.Errorf("invalid proof: %v", r.Form)
				}
				data = map[string]any{"status": 2, "file_id": "900", "pick_code": "pc900"}
			}
		case "/open/folder/get_info":
			data = map[string]any{"file_id": "900", "file_name": "proof.bin", "file_category": "1", "size_byte": len(content)}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"state": true, "code": 0, "data": data})
	}))
	defer srv.Close()
	p, err := pan115.NewWithOptions("ali", pan115.Options{Client: httpx.New(httpx.Options{}), BaseURL: srv.URL, AccessToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	pWrapped := provider.Instrument(p, provider.NewStats())
	f.up.opt.Providers = func(string) (provider.Provider, bool) { return pWrapped, true }
	if _, err := f.up.DrainAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rounds != 2 || len(f.ok) != 1 || !f.ok[0].Rapid || f.ok[0].Entry.ID != "900" {
		t.Fatalf("rounds=%d success=%+v dead=%v", rounds, f.ok, f.dead)
	}
}
