package control

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// SearchHit is one match. The path is the virtual path, never a provider id.
type SearchHit struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Kind is "dir" or "file".
	Kind  string    `json:"kind"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	// Cached says the whole file is in the local block cache.
	Cached bool `json:"cached"`
}

// SearchCoverage is how much of the known tree the index holds, the same
// listed/known pair Status carries, so a results page can say what it
// could not see.
type SearchCoverage struct {
	Listed   int64 `json:"listed"`
	Known    int64 `json:"known"`
	Crawling bool  `json:"crawling"`
}

// SearchResponse says whether the answer is the whole answer: the index has
// a work budget, and a client that is not told the budget was hit would take
// a partial list for the full one.
type SearchResponse struct {
	Results  []SearchHit    `json:"results"`
	Complete bool           `json:"complete"`
	Coverage SearchCoverage `json:"coverage"`
}

// SearchQuery is the client side of /search: what cloudfs find sends.
type SearchQuery struct {
	Q, Path, Ext, Glob, Kind, After, Sort string
	MinSize, MaxSize                      int64
	Limit                                 int
}

func (q SearchQuery) values() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("q", q.Q)
	set("path", q.Path)
	set("ext", q.Ext)
	set("glob", q.Glob)
	set("kind", q.Kind)
	set("after", q.After)
	set("sort", q.Sort)
	if q.MinSize > 0 {
		v.Set("min_size", strconv.FormatInt(q.MinSize, 10))
	}
	if q.MaxSize > 0 {
		v.Set("max_size", strconv.FormatInt(q.MaxSize, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// CallSearch asks a running daemon. The bool is false when none answers.
func CallSearch(ctx context.Context, socket, tcp string, q SearchQuery) (SearchResponse, bool, error) {
	var out SearchResponse
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/search?"+q.values().Encode(), nil, &out)
	return out, online, err
}

const searchMax = 500

// GET /search?q=name&path=/scope&limit=50&ext=go,md&glob=*.md&kind=file
// &min_size=1&max_size=1048576&after=2026-09-01&sort=mtime searches known
// metadata — what has been listed — not the remote. That is what makes it
// instant, and the coverage pair is what lets the UI say how much of the
// tree that is. q may be empty when any filter is given.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	filter, err := meta.ParseQuery(strings.TrimSpace(q.Get("q")))
	if err != nil {
		httpErrorT(w, r, http.StatusBadRequest, "err.search_param", "q")
		return
	}
	if glob := q.Get("glob"); glob != "" {
		filter.Terms = append(filter.Terms, meta.Term{Text: glob, Glob: true})
	}
	for _, ext := range strings.Split(q.Get("ext"), ",") {
		if ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), "."); ext != "" {
			filter.Ext = append(filter.Ext, ext)
		}
	}
	for _, p := range []struct {
		key string
		dst *int64
	}{{"min_size", &filter.MinSize}, {"max_size", &filter.MaxSize}} {
		if raw := q.Get(p.key); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				httpErrorT(w, r, http.StatusBadRequest, "err.search_param", p.key)
				return
			}
			*p.dst = n
		}
	}
	if raw := q.Get("after"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			if t, err = time.ParseInLocation("2006-01-02", raw, time.Local); err != nil {
				httpErrorT(w, r, http.StatusBadRequest, "err.search_param", "after")
				return
			}
		}
		filter.ModifiedAfter = t
	}
	switch kind := q.Get("kind"); kind {
	case "":
	case "dir", "file":
		filter.Kind = kind
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.search_param", "kind")
		return
	}
	if filter.Empty() {
		httpErrorT(w, r, http.StatusBadRequest, "err.q_required")
		return
	}
	root := "/"
	if raw := q.Get("path"); raw != "" {
		p, ok := s.fsPath(w, raw)
		if !ok {
			return
		}
		root = p
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > searchMax {
			httpErrorT(w, r, http.StatusBadRequest, "err.limit_range", searchMax)
			return
		}
		limit = n
	}
	answer, err := s.collector.FS.Search(r.Context(), meta.SearchQuery{Filter: filter, Roots: []string{root}, Limit: limit, Sort: q.Get("sort")})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out := SearchResponse{
		Results:  make([]SearchHit, 0, len(answer.Results)),
		Complete: answer.Complete,
		Coverage: SearchCoverage{Listed: answer.Coverage.Listed, Known: answer.Coverage.Known, Crawling: answer.Crawling},
	}
	for _, hit := range answer.Results {
		kind := "file"
		if hit.Kind == provider.KindDir {
			kind = "dir"
		}
		out.Results = append(out.Results, SearchHit{Name: hit.Name, Path: hit.Path, Kind: kind, Size: hit.Size, MTime: hit.MTime, Cached: hit.Cached})
	}
	writeJSON(w, out)
}
