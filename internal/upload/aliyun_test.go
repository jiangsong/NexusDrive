package upload

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/aliyun"
	"cloudfs/internal/provider/httpx"
)

// The driver is wrapped just as in a daemon. No ProofBytes option is supplied:
// only the journal/uploader can answer the challenge for this queued content.
func TestQueuedAliyunUploadAnswersContentProof(t *testing.T) {
	f := newFixture(t)
	content := bytes.Repeat([]byte("cloudfs-content-proof-"), 200)
	f.queueWithHashes(t, "proof.bin", content, "", []provider.HashType{provider.HashSHA1, provider.HashPreSHA1})
	const token = "test-access-token"
	rounds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/adrive/v1.0/openFile/create" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("incorrect bearer token")
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		rounds++
		if rounds == 1 {
			want := strings.ToUpper(fmt.Sprintf("%x", sha1.Sum(content[:1024])))
			if req["pre_hash"] != want {
				t.Errorf("pre_hash = %v, want %s", req["pre_hash"], want)
			}
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"code":"PreHashMatched","message":"matched"}`))
			return
		}
		if rounds != 2 {
			t.Errorf("unexpected create round %d", rounds)
		}
		off, n := aliyun.ProofRange(token, int64(len(content)))
		want := base64.StdEncoding.EncodeToString(content[off : off+n])
		if req["proof_code"] != want || req["proof_version"] != "v1" {
			t.Errorf("invalid proof: %v", req)
		}
		if req["content_hash"] != strings.ToUpper(fmt.Sprintf("%x", sha1.Sum(content))) {
			t.Error("wrong whole-file hash")
		}
		w.Write([]byte(`{"file_id":"rapid-file","file_name":"proof.bin","rapid_upload":true}`))
	}))
	defer srv.Close()
	p, err := aliyun.New(aliyun.Options{Name: "ali", Client: httpx.New(httpx.Options{}), BaseURL: srv.URL, AccessToken: token, DriveID: "drive"})
	if err != nil {
		t.Fatal(err)
	}
	pWrapped := provider.Instrument(p, provider.NewStats())
	f.up.opt.Providers = func(string) (provider.Provider, bool) { return pWrapped, true }
	if _, err := f.up.DrainAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rounds != 2 || len(f.ok) != 1 || !f.ok[0].Rapid || f.ok[0].Entry.ID != "rapid-file" {
		t.Fatalf("rounds=%d results=%+v dead=%v", rounds, f.ok, f.dead)
	}
}
