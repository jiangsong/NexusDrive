package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/embed"
	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
)

// fakeIndex is IndexControl over in-memory data: three healthy documents,
// whatever rules the test seeds, and a progress channel the test pushes
// into. It records every mutation and search so a route test can prove
// what reached the indexer.
type fakeIndex struct {
	mu       sync.Mutex
	status   index.Status
	identity string
	rules    []index.RuleView
	added    []index.Rule
	removed  []string
	rebuilt  int
	retried  []string
	failed   []index.FailedDoc
	searches []index.SearchQuery
	texts    []textCall
	progress chan index.Progress
	// removeErr, when set, is what RemoveRule answers.
	removeErr error
	// embedder is what Embedder answers; recordedModel and recordedDim
	// what index_meta says the vectors were made with.
	embedder      embed.Embedder
	recordedModel string
	recordedDim   int
}

type textCall struct {
	path string
	off  int64
	max  int
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{
		status: index.Status{Enabled: true, Docs: index.DocCounts{OK: 3}, ChunksTotal: 12, Pending: 2,
			TextBytes: 1000, MaxTotalText: 10000, FetchBudget: index.BudgetUse{Used: 5, Limit: 50},
			FetchBytesTotal: 77, Progress: index.Progress{Running: true, Failed: 1, Paused: index.PausedBusy}},
		identity: "meta-1",
		progress: make(chan index.Progress, 64),
	}
}

func (f *fakeIndex) Status(ctx context.Context, p string) (index.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status
	if p != "" {
		st.State = index.StateOK
		st.Covered = p
	}
	return st, nil
}

func (f *fakeIndex) Rules(ctx context.Context) ([]index.RuleView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]index.RuleView(nil), f.rules...), nil
}

func (f *fakeIndex) AddRule(ctx context.Context, r index.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, r)
	f.rules = append(f.rules, index.RuleView{Rule: r})
	return nil
}

func (f *fakeIndex) RemoveRule(ctx context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, p)
	return nil
}

func (f *fakeIndex) Rebuild(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rebuilt++
	return nil
}

func (f *fakeIndex) Retry(ctx context.Context, p string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, p)
	return int64(f.status.Docs.Failed), nil
}

func (f *fakeIndex) Failed(ctx context.Context, cursor string, limit int) ([]index.FailedDoc, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := 0
	if cursor != "" {
		for i, d := range f.failed {
			if d.Path == cursor {
				start = i
			}
		}
	}
	end := min(start+limit, len(f.failed))
	next := ""
	if end < len(f.failed) {
		next = f.failed[end].Path
	}
	return append([]index.FailedDoc(nil), f.failed[start:end]...), next, nil
}

func (f *fakeIndex) Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, q)
	return index.SearchResult{Hits: []index.Hit{{Path: "/work/a.md", Snippet: q.Query}}, ModeUsed: "keyword", Docs: 3}, nil
}

func (f *fakeIndex) Text(ctx context.Context, p string, off int64, max int) (index.TextPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, textCall{p, off, max})
	return index.TextPage{Path: p, Text: "hello", Offset: off, NextOffset: off + 5, EOF: true, Kind: "file"}, nil
}

func (f *fakeIndex) Watch() (<-chan index.Progress, func()) {
	return f.progress, func() {}
}

func (f *fakeIndex) Identity(ctx context.Context) (string, error) { return f.identity, nil }

func (f *fakeIndex) Embedder() embed.Embedder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.embedder
}

func (f *fakeIndex) RecordedEmbedding(ctx context.Context) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recordedModel, f.recordedDim, nil
}

func indexServer(t *testing.T) (*fixture, *fakeIndex, http.Handler) {
	t.Helper()
	f := newFixture(t)
	fi := newFakeIndex()
	f.coll.Index = fi
	return f, fi, NewServer(f.coll).Handler()
}

// TestIndexStatusWhenDisabled: a daemon without an index still answers
// /index/status, with the one field the console needs to show the
// "not enabled" screen, and answers nothing else under /index.
func TestIndexStatusWhenDisabled(t *testing.T) {
	f := newFixture(t)
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/index/status", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"enabled":false}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/index/rules", ""); w.Code != 404 {
		t.Fatalf("rules while disabled: %d", w.Code)
	}
	if w := uiCallControl(t, h, "POST", "/index/rebuild", `{"confirm":true}`); w.Code != 404 {
		t.Fatalf("rebuild while disabled: %d", w.Code)
	}
	if w := uiCallControl(t, h, "GET", "/index/search?q=x", ""); w.Code != 404 {
		t.Fatalf("search while disabled: %d", w.Code)
	}
}

func TestIndexStatusWithAnIndexReportsThePath(t *testing.T) {
	_, _, h := indexServer(t)
	w := uiCallControl(t, h, "GET", "/index/status?path=/work", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var st index.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || !st.Enabled || st.Docs.OK != 3 || st.State != "ok" || st.Covered != "/work" {
		t.Fatalf("%+v %v", st, err)
	}
	if w := uiCallControl(t, h, "GET", "/index/status?path=relative", ""); w.Code != 400 {
		t.Fatalf("relative path: %d", w.Code)
	}
	if w := uiCallControl(t, h, "POST", "/index/status", `{}`); w.Code != 405 {
		t.Fatalf("POST status: %d", w.Code)
	}
}

func TestIndexRemoveAndRebuildNeedConfirm(t *testing.T) {
	_, fi, h := indexServer(t)
	if w := uiCallControl(t, h, "POST", "/index/remove", `{"path":"/notes"}`); w.Code != 400 || len(fi.removed) != 0 || !strings.Contains(w.Body.String(), "/notes") {
		t.Fatalf("remove without confirm: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/index/remove", `{"path":"/notes","confirm":true}`); w.Code != 200 || len(fi.removed) != 1 || fi.removed[0] != "/notes" {
		t.Fatalf("remove: %d %s %v", w.Code, w.Body, fi.removed)
	}
	if w := uiCallControl(t, h, "POST", "/index/rebuild", `{}`); w.Code != 400 || fi.rebuilt != 0 {
		t.Fatalf("rebuild without confirm: %d", w.Code)
	}
	if w := uiCallControl(t, h, "POST", "/index/rebuild", `{"confirm":true}`); w.Code != 200 || fi.rebuilt != 1 {
		t.Fatalf("rebuild: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/index/rebuild", ""); w.Code != 405 {
		t.Fatalf("GET rebuild: %d", w.Code)
	}
}

// The refusal names the consequence in the reader's language.
func TestIndexConfirmationsAreLocalized(t *testing.T) {
	_, _, h := indexServer(t)
	seen := map[string]bool{}
	for _, lang := range []string{"zh", "en"} {
		w := uiCallControl(t, h, "POST", "/index/rebuild?lang="+lang, `{}`)
		body := strings.TrimSpace(w.Body.String())
		if w.Code != 400 || body == "" || seen[body] {
			t.Fatalf("%s: %d %q", lang, w.Code, body)
		}
		seen[body] = true
	}
	for _, key := range []string{"confirm.index_remove", "confirm.index_rebuild", "err.index_disabled", "doctor.index.ok", "doctor.index.failed", "doctor.index.failed.fix", "doctor.index.budget", "fix.index_requeued"} {
		if !i18n.Has(key) || i18n.T(i18n.EN, key) == i18n.T(i18n.ZH, key) {
			t.Errorf("%s is missing from a catalog or untranslated", key)
		}
	}
}

func TestIndexRemoveOfAConfigRuleIs409(t *testing.T) {
	_, fi, h := indexServer(t)
	fi.removeErr = index.ErrConfigRule
	w := uiCallControl(t, h, "POST", "/index/remove?lang=en", `{"path":"/notes","confirm":true}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "configuration file") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	fi.removeErr = index.ErrNoRule
	if w := uiCallControl(t, h, "POST", "/index/remove", `{"path":"/notes","confirm":true}`); w.Code != 404 {
		t.Fatalf("unknown rule: %d %s", w.Code, w.Body)
	}
	fi.removeErr = errors.New("disk on fire")
	if w := uiCallControl(t, h, "POST", "/index/remove", `{"path":"/notes","confirm":true}`); w.Code != 500 || !strings.Contains(w.Body.String(), "disk on fire") {
		t.Fatalf("other error: %d %s", w.Code, w.Body)
	}
}

func TestIndexSearchRoutePassesQuery(t *testing.T) {
	_, fi, h := indexServer(t)
	w := uiCallControl(t, h, "GET", "/index/search?q=%E7%BD%91%E7%9B%98&path=/work&mode=hybrid&limit=5", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(fi.searches) != 1 || fi.searches[0].Query != "网盘" || fi.searches[0].Roots[0] != "/work" || fi.searches[0].TopK != 5 || fi.searches[0].Mode != "hybrid" {
		t.Fatalf("%+v", fi.searches)
	}
	var res index.SearchResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || len(res.Hits) != 1 || res.Hits[0].Snippet != "网盘" {
		t.Fatalf("%s %v", w.Body, err)
	}
	// Defaults: the whole tree, twenty hits.
	if w := uiCallControl(t, h, "GET", "/index/search?q=x", ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if q := fi.searches[1]; q.Roots[0] != "/" || q.TopK != defaultIndexSearchLimit {
		t.Fatalf("defaults: %+v", q)
	}
	for _, bad := range []string{"/index/search?q=x&limit=0", "/index/search?q=x&limit=101", "/index/search?q=x&mode=magic", "/index/search?q=x&path=relative"} {
		if w := uiCallControl(t, h, "GET", bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
}

func TestIndexTextAndFailedRoutesPage(t *testing.T) {
	_, fi, h := indexServer(t)
	w := uiCallControl(t, h, "GET", "/index/text?path=/a&offset=10&max_bytes=20", "")
	if w.Code != 200 || len(fi.texts) != 1 || fi.texts[0] != (textCall{"/a", 10, 20}) {
		t.Fatalf("%d %s %+v", w.Code, w.Body, fi.texts)
	}
	if w := uiCallControl(t, h, "GET", "/index/text?path=/a", ""); w.Code != 200 || fi.texts[1].max != defaultIndexTextBytes || fi.texts[1].off != 0 {
		t.Fatalf("defaults: %d %+v", w.Code, fi.texts)
	}
	for _, bad := range []string{"/index/text", "/index/text?path=/a&offset=-1", "/index/text?path=/a&max_bytes=0", "/index/text?path=/a&max_bytes=2000000"} {
		if w := uiCallControl(t, h, "GET", bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}

	for _, p := range []string{"/f1", "/f2", "/f3"} {
		fi.failed = append(fi.failed, index.FailedDoc{Path: p, Kind: "pdf", Error: "no text", IndexedAt: time.Now()})
	}
	w = uiCallControl(t, h, "GET", "/index/failed?limit=2", "")
	var page IndexFailedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Documents) != 2 || page.NextCursor != "/f3" {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	w = uiCallControl(t, h, "GET", "/index/failed?limit=2&cursor="+page.NextCursor, "")
	var second IndexFailedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || len(second.Documents) != 1 || second.NextCursor != "" || second.Documents[0].Path != "/f3" {
		t.Fatalf("second page: %s %v", w.Body, err)
	}
}

func TestIndexAddRouteMarksUISource(t *testing.T) {
	_, fi, h := indexServer(t)
	w := uiCallControl(t, h, "POST", "/index/add", `{"path":"/notes/","include":["*.md"],"max_file_size":1024}`)
	if w.Code != 200 || len(fi.added) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r := fi.added[0]; r.Source != "ui" || r.Path != "/notes" || len(r.Include) != 1 || r.MaxFileSize != 1024 {
		t.Fatalf("%+v", r)
	}
	var rules IndexRulesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rules); err != nil || len(rules.Rules) != 1 || rules.Rules[0].Source != "ui" {
		t.Fatalf("reply is not the rule table: %s %v", w.Body, err)
	}
	for _, bad := range []string{`{"path":"notes"}`, `{"path":""}`, `{"path":"/n","max_file_size":-1}`, `{"path":"/n","source":"config"}`} {
		if w := uiCallControl(t, h, "POST", "/index/add", bad); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if len(fi.added) != 1 {
		t.Fatalf("a refused request reached the indexer: %+v", fi.added)
	}
}

func TestIndexRetryRouteReportsTheCount(t *testing.T) {
	_, fi, h := indexServer(t)
	fi.status.Docs.Failed = 4
	w := uiCallControl(t, h, "POST", "/index/retry", `{"path":"/docs"}`)
	var out IndexRetryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || out.Requeued != 4 || len(fi.retried) != 1 || fi.retried[0] != "/docs" {
		t.Fatalf("%d %s %v %v", w.Code, w.Body, err, fi.retried)
	}
	if w := uiCallControl(t, h, "POST", "/index/retry", `{}`); w.Code != 200 || fi.retried[1] != "" {
		t.Fatalf("retry all: %d %v", w.Code, fi.retried)
	}
}

func TestStatusCarriesIndexEnabled(t *testing.T) {
	_, _, h := indexServer(t)
	w := uiCallControl(t, h, "GET", "/status", "")
	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.Index == nil {
		t.Fatalf("%s %v", w.Body, err)
	}
	if !st.Index.Enabled || st.Index.Docs.OK != 3 || st.Index.Pending != 2 || st.Index.Chunks != 12 || st.Index.TextBytes != 1000 || st.Index.MaxTotalText != 10000 ||
		st.Index.FetchedThisHour != 5 || st.Index.FetchBudget != 50 || st.Index.Paused != "busy" || !st.Index.Running || st.Index.Failures != 1 {
		t.Fatalf("%+v", st.Index)
	}
	if !strings.Contains(w.Body.String(), `"index": {`) || !strings.Contains(w.Body.String(), `"enabled": true`) {
		t.Fatalf("status lacks the index line: %s", w.Body)
	}

	plain := uiCallControl(t, NewServer(newFixture(t).coll).Handler(), "GET", "/status", "")
	if strings.Contains(plain.Body.String(), `"index"`) {
		t.Fatalf("a daemon without an index reports one: %s", plain.Body)
	}
}

func TestIndexMetricsAreExported(t *testing.T) {
	_, _, h := indexServer(t)
	body := uiCallControl(t, h, "GET", "/metrics", "").Body.String()
	for _, want := range []string{
		`cloudfs_index_documents{state="ok"} 3`,
		`cloudfs_index_documents{state="failed"} 0`,
		"cloudfs_index_pending 2",
		"cloudfs_index_chunks 12",
		"cloudfs_index_text_bytes 1000",
		"cloudfs_index_fetch_bytes_total 77",
		"cloudfs_index_failures_total 1",
		"# TYPE cloudfs_index_fetch_bytes_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	plain := uiCallControl(t, NewServer(newFixture(t).coll).Handler(), "GET", "/metrics", "").Body.String()
	if strings.Contains(plain, "cloudfs_index_") {
		t.Fatalf("a daemon without an index exports index metrics")
	}
}
