package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// uploadOneFile drives a whole session upload so the counters the pool page
// reads are the ones a real upload leaves behind, not numbers a test wrote by
// hand.
func uploadOneFile(t *testing.T, p provider.Provider, name string, body []byte) {
	t.Helper()
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, fakeprovider.RootID, name, int64(len(body)), nil)
	if err != nil {
		t.Fatalf("BeginUpload(%s): %v", name, err)
	}
	tok, err := p.UploadPart(ctx, s, 0, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("UploadPart(%s): %v", name, err)
	}
	if _, err := p.CompleteUpload(ctx, s, []provider.PartToken{tok}); err != nil {
		t.Fatalf("CompleteUpload(%s): %v", name, err)
	}
}

func poolStatusMember(t *testing.T, f *poolFixture, remote string) map[string]any {
	t.Helper()
	rr := f.do(t, http.MethodGet, "/pool/status", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /pool/status = %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Pools []struct {
			Members []map[string]any `json:"members"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(out.Pools))
	}
	for _, m := range out.Pools[0].Members {
		if m["remote"] == remote {
			return m
		}
	}
	t.Fatalf("member %q missing from %v", remote, out.Pools[0].Members)
	return nil
}

// Free space decides which member becomes a file's primary, so the roomiest
// drive wins even when it is the slowest one in the pool. The page has to
// carry the other half of that trade-off, or the person choosing a write
// preference is choosing blind.
func TestPoolStatusReportsWhatOneFileCostsOnEachMember(t *testing.T) {
	f := newPoolFixture(t)
	stats := provider.NewStats()
	instrumented := provider.Instrument(f.a, stats)
	uploadOneFile(t, instrumented, "cost-one.txt", []byte("one"))
	uploadOneFile(t, instrumented, "cost-two.txt", []byte("two"))
	f.srv.collector.CallStats = map[string]*provider.Stats{"a": stats}

	m := poolStatusMember(t, f, "a")

	samples, ok := m["write_samples"].(float64)
	if !ok || int(samples) != 2 {
		t.Fatalf("write_samples = %v, want the 2 uploads that finished", m["write_samples"])
	}
	cost, ok := m["write_ms_per_file"].(float64)
	if !ok || cost <= 0 {
		t.Fatalf("write_ms_per_file = %v, want the time those uploads took", m["write_ms_per_file"])
	}
}

// A member the daemon has not written to yet must not be reported as costing
// nothing: on the page that would read as "infinitely fast" and would argue
// for making it every file's primary.
func TestPoolStatusOmitsTheCostOfAMemberThatHasFinishedNoUpload(t *testing.T) {
	f := newPoolFixture(t)
	f.srv.collector.CallStats = map[string]*provider.Stats{"a": provider.NewStats()}

	m := poolStatusMember(t, f, "a")

	if _, present := m["write_ms_per_file"]; present {
		t.Fatalf("write_ms_per_file = %v, want the key left out until a file lands", m["write_ms_per_file"])
	}
	if _, present := m["write_samples"]; present {
		t.Fatalf("write_samples = %v, want the key left out until a file lands", m["write_samples"])
	}
}
