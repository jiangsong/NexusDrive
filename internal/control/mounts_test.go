package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMountsEditTheConfigurationAndSayRestart(t *testing.T) {
	srv, _, _ := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodPost, "/mounts", MountRequest{Path: "/mnt/cloud", Prefix: "/nas", Remote: "existing", Mode: "readonly", DirTTL: "24h"})
	if rr.Code != 200 {
		t.Fatalf("add: %d %s", rr.Code, rr.Body)
	}
	var out MountMutationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.RestartRequired || len(out.Mounts) != 1 || out.Mounts[0].Mode != "readonly" || out.Mounts[0].DirTTL != "24h0m0s" || out.Mounts[0].Active {
		t.Fatalf("add reply: %+v", out)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/mounts", MountRequest{Path: "/mnt/cloud", Prefix: "/nas", Remote: "existing"}); rr.Code != 409 {
		t.Fatalf("adding an existing prefix: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/mounts", MountRequest{Path: "/mnt/cloud", Prefix: "/ghost", Remote: "ghost"}); rr.Code != 409 {
		t.Fatalf("a layout for an unknown remote: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/mounts", MountRequest{Path: "/mnt/cloud", Prefix: "rel", Remote: "existing"}); rr.Code != 409 {
		t.Fatalf("a relative prefix: %d %s", rr.Code, rr.Body)
	}
	rr = accountRequest(t, srv, http.MethodPatch, "/mounts", MountRequest{Path: "/mnt/cloud", Prefix: "/nas", Remote: "existing", Mode: "strict"})
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out.Mounts[0].Mode != "strict" {
		t.Fatalf("replace: %d %+v", rr.Code, out)
	}
	if rr := accountRequest(t, srv, http.MethodDelete, "/mounts?path=/mnt/cloud&prefix=/nas", nil); rr.Code != 400 || !strings.Contains(rr.Body.String(), "confirm") {
		t.Fatalf("delete without confirm: %d %s", rr.Code, rr.Body)
	}
	rr = accountRequest(t, srv, http.MethodDelete, "/mounts?path=/mnt/cloud&prefix=/nas&confirm=true", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || len(out.Mounts) != 0 {
		t.Fatalf("delete: %d %+v", rr.Code, out)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/mounts", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"mounts":[]`) {
		t.Fatalf("list after delete: %d %s", rr.Code, rr.Body)
	}
}
