package control

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"cloudfs/internal/index"
)

// IndexControl is what the control plane needs from the content indexer:
// the status and rules the console shows, the actions its buttons take,
// and the progress feed /events relays. The daemon adapts *index.Indexer
// to it; a daemon started with index.enabled false leaves Collector.Index
// nil, /index/status then answers {"enabled":false} and every other
// /index route answers 404.
type IndexControl interface {
	// Status reports the index as a whole and, when p is not empty, how
	// the index sees that path.
	Status(ctx context.Context, p string) (index.Status, error)
	Rules(ctx context.Context) ([]index.RuleView, error)
	AddRule(ctx context.Context, r index.Rule) error
	// RemoveRule answers index.ErrConfigRule for a rule the configuration
	// file owns and index.ErrNoRule for one that does not exist.
	RemoveRule(ctx context.Context, p string) error
	Rebuild(ctx context.Context) error
	// Retry requeues the failed documents under p ("" for all) and reports
	// how many.
	Retry(ctx context.Context, p string) (int64, error)
	Failed(ctx context.Context, cursor string, limit int) ([]index.FailedDoc, string, error)
	Search(ctx context.Context, q index.SearchQuery) (index.SearchResult, error)
	Text(ctx context.Context, p string, off int64, max int) (index.TextPage, error)
	// Watch delivers a progress snapshot after every change; the returned
	// function ends the subscription.
	Watch() (<-chan index.Progress, func())
	// Identity is the meta store identity index.db was built against, ""
	// before the index was ever bound to one. The doctor compares it with
	// the live meta store.
	Identity(ctx context.Context) (string, error)
}

// IndexRulesResponse is GET /index/rules.
type IndexRulesResponse struct {
	Rules []index.RuleView `json:"rules"`
}

// IndexAddRequest is POST /index/add. The rule it creates is recorded with
// source "ui", so the console can offer to remove it again; a rule the
// configuration file owns is never created here.
type IndexAddRequest struct {
	Path        string   `json:"path"`
	Include     []string `json:"include,omitempty"`
	MaxFileSize int64    `json:"max_file_size,omitempty"`
}

// IndexRemoveRequest is POST /index/remove.
type IndexRemoveRequest struct {
	Path    string `json:"path"`
	Confirm bool   `json:"confirm"`
}

// IndexRebuildRequest is POST /index/rebuild.
type IndexRebuildRequest struct {
	Confirm bool `json:"confirm"`
}

// IndexRetryRequest is POST /index/retry; an empty path retries every
// failed document.
type IndexRetryRequest struct {
	Path string `json:"path,omitempty"`
}

// IndexRetryResponse says how many documents /index/retry requeued.
type IndexRetryResponse struct {
	Requeued int64 `json:"requeued"`
}

// IndexFailedResponse is GET /index/failed.
type IndexFailedResponse struct {
	Documents  []index.FailedDoc `json:"documents"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// IndexStatusBrief is what /index/status answers on a daemon without an
// index: enough for the console to show the "not enabled" screen.
type IndexStatusBrief struct {
	Enabled bool `json:"enabled"`
}

// IndexStatus is the index line of /status: what the overview cards and
// the progress bar show, and what the cloudfs_index_* metrics are read
// from. Present whenever the daemon has an index, even when the snapshot
// could not be read (Enabled alone is then true).
type IndexStatus struct {
	Enabled bool            `json:"enabled"`
	Docs    index.DocCounts `json:"docs"`
	Pending int             `json:"pending"`
	Chunks  int             `json:"chunks"`
	// TextBytes is the extracted text held, against MaxTotalText (0 when
	// unlimited).
	TextBytes    int64 `json:"text_bytes"`
	MaxTotalText int64 `json:"max_total_text"`
	// FetchedThisHour is what the indexer downloaded in the current budget
	// window, against FetchBudget (0 when unlimited); FetchBytesTotal is
	// the process-lifetime counter.
	FetchedThisHour int64 `json:"fetched_this_hour"`
	FetchBudget     int64 `json:"fetch_budget"`
	FetchBytesTotal int64 `json:"fetch_bytes_total"`
	// Failures counts extraction failures since the process started.
	Failures int64 `json:"failures_total"`
	// Running says the extraction worker is alive in this process;
	// Crawling that it is extracting a file right now.
	Running  bool `json:"running"`
	Crawling bool `json:"crawling"`
	// Paused is why the worker stands still ("" | busy | risk_control |
	// budget | text_budget) and ResumeAt when it may go on, if known.
	Paused   string    `json:"paused,omitempty"`
	ResumeAt time.Time `json:"resume_at,omitzero"`
}

const (
	defaultIndexSearchLimit = 20
	maxIndexSearchLimit     = 100
	defaultIndexTextBytes   = 64 << 10
	maxIndexTextBytes       = 1 << 20
	defaultIndexFailedLimit = 50
	maxIndexFailedLimit     = 500
	maxIndexRequest         = 16 << 10
)

// indexStatusOf builds the /status line from a snapshot.
func indexStatusOf(st index.Status) *IndexStatus {
	return &IndexStatus{
		Enabled: true, Docs: st.Docs, Pending: st.Pending, Chunks: st.ChunksTotal,
		TextBytes: st.TextBytes, MaxTotalText: st.MaxTotalText,
		FetchedThisHour: st.FetchBudget.Used, FetchBudget: st.FetchBudget.Limit, FetchBytesTotal: st.FetchBytesTotal,
		Failures: st.Progress.Failed, Running: st.Progress.Running, Crawling: st.Progress.Extracting > 0,
		Paused: st.Progress.Paused, ResumeAt: st.Progress.ResumeAt,
	}
}

// indexReady is the guard every /index route but /index/status starts
// with: the request is local, the method is right and an index exists.
func (s *Server) indexReady(w http.ResponseWriter, r *http.Request, method string) (IndexControl, bool) {
	if !privateRequest(w, r) {
		return nil, false
	}
	if !allowMethod(w, r, method) {
		return nil, false
	}
	if s.collector.Index == nil {
		httpErrorT(w, r, http.StatusNotFound, "err.index_disabled")
		return nil, false
	}
	return s.collector.Index, true
}

// indexError maps an indexer error onto a status code and catalog key.
func indexError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, index.ErrConfigRule):
		httpErrorT(w, r, http.StatusConflict, "err.index_config_rule")
	case errors.Is(err, index.ErrNoRule):
		httpErrorT(w, r, http.StatusNotFound, "err.index_no_rule")
	case errors.Is(err, index.ErrNotIndexed):
		httpErrorT(w, r, http.StatusNotFound, "err.index_not_indexed")
	case errors.Is(err, index.ErrEmptyQuery):
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query")
	case errors.Is(err, index.ErrReadOnly):
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.index_read_only")
	default:
		httpErrorT(w, r, http.StatusInternalServerError, "err.index_failed", err)
	}
}

// GET /index/status?path=
func (s *Server) indexStatus(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.collector.Index == nil {
		writeJSON(w, IndexStatusBrief{Enabled: false})
		return
	}
	p := r.URL.Query().Get("path")
	if p != "" {
		var ok bool
		if p, ok = s.fsPath(w, p); !ok {
			return
		}
	}
	st, err := s.collector.Index.Status(r.Context(), p)
	if err != nil {
		indexError(w, r, err)
		return
	}
	writeJSON(w, st)
}

// GET /index/rules
func (s *Server) indexRules(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	writeRules(w, r, x)
}

// writeRules answers with the rule table; the add and remove routes reply
// with it too, so the console redraws from one response.
func writeRules(w http.ResponseWriter, r *http.Request, x IndexControl) {
	rules, err := x.Rules(r.Context())
	if err != nil {
		indexError(w, r, err)
		return
	}
	if rules == nil {
		rules = []index.RuleView{}
	}
	writeJSON(w, IndexRulesResponse{Rules: rules})
}

// POST /index/add
func (s *Server) indexAdd(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q IndexAddRequest
	if !decodeMutationLimit(w, r, &q, maxIndexRequest) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	if q.MaxFileSize < 0 {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	rule := index.Rule{Path: p, Include: q.Include, MaxFileSize: q.MaxFileSize, Source: "ui"}
	if err := x.AddRule(r.Context(), rule); err != nil {
		indexError(w, r, err)
		return
	}
	writeRules(w, r, x)
}

// POST /index/remove {"path":"/notes","confirm":true}
func (s *Server) indexRemove(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q IndexRemoveRequest
	if !decodeMutationLimit(w, r, &q, maxIndexRequest) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	if !confirmed(w, r, q.Confirm, "confirm.index_remove", p) {
		return
	}
	if err := x.RemoveRule(r.Context(), p); err != nil {
		indexError(w, r, err)
		return
	}
	writeRules(w, r, x)
}

// POST /index/rebuild {"confirm":true}
func (s *Server) indexRebuild(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q IndexRebuildRequest
	if !decodeMutationLimit(w, r, &q, maxIndexRequest) {
		return
	}
	if !confirmed(w, r, q.Confirm, "confirm.index_rebuild") {
		return
	}
	if err := x.Rebuild(r.Context()); err != nil {
		indexError(w, r, err)
		return
	}
	st, err := x.Status(r.Context(), "")
	if err != nil {
		indexError(w, r, err)
		return
	}
	writeJSON(w, st)
}

// POST /index/retry {"path":"/notes"}
func (s *Server) indexRetry(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodPost)
	if !ok {
		return
	}
	var q IndexRetryRequest
	if !decodeMutationLimit(w, r, &q, maxIndexRequest) {
		return
	}
	p := ""
	if q.Path != "" {
		if p, ok = s.fsPath(w, q.Path); !ok {
			return
		}
	}
	n, err := x.Retry(r.Context(), p)
	if err != nil {
		indexError(w, r, err)
		return
	}
	writeJSON(w, IndexRetryResponse{Requeued: n})
}

// GET /index/failed?cursor=&limit=
func (s *Server) indexFailed(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	limit, ok := queryLimit(w, r, defaultIndexFailedLimit, maxIndexFailedLimit)
	if !ok {
		return
	}
	docs, next, err := x.Failed(r.Context(), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		indexError(w, r, err)
		return
	}
	if docs == nil {
		docs = []index.FailedDoc{}
	}
	writeJSON(w, IndexFailedResponse{Documents: docs, NextCursor: next})
}

// GET /index/search?q=&path=/&mode=keyword|hybrid&limit=20
//
// The console shares the machine's trust boundary with /fs/*, so the
// search has no read scope of its own: path only narrows the results.
func (s *Server) indexSearch(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	params := r.URL.Query()
	root := params.Get("path")
	if root == "" {
		root = "/"
	}
	if root, ok = s.fsPath(w, root); !ok {
		return
	}
	limit, ok := queryLimit(w, r, defaultIndexSearchLimit, maxIndexSearchLimit)
	if !ok {
		return
	}
	mode := params.Get("mode")
	switch mode {
	case "", "keyword", "hybrid", "vector":
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	res, err := x.Search(r.Context(), index.SearchQuery{Query: params.Get("q"), Roots: []string{root}, TopK: limit, Mode: mode})
	if err != nil {
		indexError(w, r, err)
		return
	}
	if res.Hits == nil {
		res.Hits = []index.Hit{}
	}
	writeJSON(w, res)
}

// GET /index/text?path=&offset=&max_bytes=
func (s *Server) indexText(w http.ResponseWriter, r *http.Request) {
	x, ok := s.indexReady(w, r, http.MethodGet)
	if !ok {
		return
	}
	params := r.URL.Query()
	p, ok := s.fsPath(w, params.Get("path"))
	if !ok {
		return
	}
	var off int64
	if raw := params.Get("offset"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			httpErrorT(w, r, http.StatusBadRequest, "err.offset_invalid")
			return
		}
		off = n
	}
	max := defaultIndexTextBytes
	if raw := params.Get("max_bytes"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxIndexTextBytes {
			httpErrorT(w, r, http.StatusBadRequest, "err.length_range", maxIndexTextBytes)
			return
		}
		max = n
	}
	page, err := x.Text(r.Context(), p, off, max)
	if err != nil {
		indexError(w, r, err)
		return
	}
	writeJSON(w, page)
}
