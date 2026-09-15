package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/export"
	"cloudfs/internal/provider"

	"github.com/google/uuid"
)

// The export endpoints (docs/pool-v2.md §5.6). An export copies bytes out of
// the mount onto ordinary local storage, so a job is long-lived, durable and
// resumable — which is why it has a record a person can list, inspect and act
// on rather than a fire-and-forget button.
//
// Nothing here serves a remote object id, a member's path or a cache path:
// the plan's rows carry those, and the views below copy only the fields a
// person reads. An export is also the one thing on this API that writes to
// the machine's own filesystem, so the destination goes back out exactly as
// it was recorded and is never taken from a query parameter.

// ExportManager is the part of *export.Manager the control plane uses. It is
// an interface so that a test can exercise these routes without standing up a
// filesystem, a provider and a destination disk.
type ExportManager interface {
	Create(ctx context.Context, req export.Request) (export.Job, error)
	Job(ctx context.Context, id string) (export.Job, error)
	Jobs(ctx context.Context, limit int, after string) ([]export.Job, string, error)
	Items(ctx context.Context, id string) ([]export.Item, error)
	ItemsPage(ctx context.Context, id, after string, limit int, state export.ItemState) ([]export.Item, string, error)
	Progress(ctx context.Context, id string) (export.Progress, error)
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Cancel(ctx context.Context, id string) error
	Forget(ctx context.Context, id string) error
}

// exportActions are the four mutations, as POST /exports/<action>.
var exportActions = map[string]bool{"pause": true, "resume": true, "cancel": true, "forget": true}

// ExportRequest is POST /export.
type ExportRequest struct {
	Sources []string `json:"sources"`
	Dest    string   `json:"dest"`
	// Mirror deletes whatever the destination holds that this plan does not,
	// so it needs Confirm as well as an export marker the runner checks.
	Mirror bool `json:"mirror,omitempty"`
	Verify bool `json:"verify,omitempty"`
	// Transfers, Streams and RangeSize override the configured defaults for
	// this job only; zero means "use the configuration".
	Transfers int   `json:"transfers,omitempty"`
	Streams   int   `json:"streams,omitempty"`
	RangeSize int64 `json:"range_size,omitempty"`
	Confirm   bool  `json:"confirm,omitempty"`
}

// maxExportSources bounds one request. A plan walks each source, so the cap
// is about the size of the request, not about the size of the export.
const maxExportSources = 256

func (q ExportRequest) Validate() error {
	if len(q.Sources) == 0 {
		return errors.New("export: give at least one source path")
	}
	if len(q.Sources) > maxExportSources {
		return fmt.Errorf("export: at most %d sources per job", maxExportSources)
	}
	for _, s := range q.Sources {
		if !strings.HasPrefix(s, "/") {
			return fmt.Errorf("export: %q is not an absolute virtual path", s)
		}
	}
	if strings.TrimSpace(q.Dest) == "" {
		return errors.New("export: give a destination directory")
	}
	if q.Transfers < 0 || q.Streams < 0 || q.RangeSize < 0 {
		return errors.New("export: transfers, streams and range_size cannot be negative")
	}
	return nil
}

// ExportStartResponse is what POST /export answers.
type ExportStartResponse struct {
	ID string `json:"id"`
}

// ExportMutationRequest is POST /exports/pause|resume|cancel|forget.
type ExportMutationRequest struct {
	Action  string `json:"-"`
	ID      string `json:"id"`
	Confirm bool   `json:"confirm,omitempty"`
}

func (q ExportMutationRequest) Validate() error {
	if !exportActions[q.Action] {
		return errors.New("exports: pause, resume, cancel or forget")
	}
	if !validExportID(q.ID) {
		return errors.New("exports: this action needs exactly one export job ID")
	}
	if (q.Action == "forget") != q.Confirm {
		return errors.New("exports: forget requires confirm=true; the other actions do not accept confirmation")
	}
	return nil
}

// ExportsRequest is the read side: a page of jobs, or one job by ID.
type ExportsRequest struct {
	ID     string
	Limit  int
	Cursor string
}

const (
	defaultExportLimit = 100
	maxExportLimit     = 500
)

func (q ExportsRequest) Validate() error {
	if q.ID != "" && !validExportID(q.ID) {
		return errors.New("exports: invalid export job ID")
	}
	if q.Limit < 0 || q.Limit > maxExportLimit {
		return fmt.Errorf("exports: --limit must be between 1 and %d", maxExportLimit)
	}
	if len(q.Cursor) > 128 || q.Cursor != "" && !validExportID(q.Cursor) {
		return errors.New("exports: invalid cursor; restart the listing")
	}
	return nil
}

func validExportID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// ExportJobView is one job as a person reads it. The plan's bindings, remote
// ids and versions stay in the store.
type ExportJobView struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	PauseReason string   `json:"pause_reason,omitempty"`
	Sources     []string `json:"sources"`
	Dest        string   `json:"dest"`
	Mirror      bool     `json:"mirror"`
	Verify      bool     `json:"verify"`
	Transfers   int      `json:"transfers"`
	Streams     int      `json:"streams"`
	RangeSize   int64    `json:"range_size"`

	FilesTotal   int64 `json:"files_total"`
	FilesDone    int64 `json:"files_done"`
	FilesSkipped int64 `json:"files_skipped"`
	FilesFailed  int64 `json:"files_failed"`
	BytesTotal   int64 `json:"bytes_total"`
	BytesDone    int64 `json:"bytes_done"`

	// Rate and ETASeconds are the live figures, filled only for a job that is
	// still moving: a finished job has no rate, and asking for one would mean
	// a store read per row of every listing.
	Rate       float64 `json:"rate,omitempty"`
	ETASeconds float64 `json:"eta_seconds,omitempty"`

	LastError  string `json:"last_error,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// ExportProgressView is the live rate and estimate of a running job.
type ExportProgressView struct {
	BytesDone  int64                      `json:"bytes_done"`
	BytesTotal int64                      `json:"bytes_total"`
	Rate       float64                    `json:"rate"`
	ETASeconds float64                    `json:"eta_seconds"`
	Members    []ExportMemberProgressView `json:"members"`
}

type ExportMemberProgressView struct {
	Remote   string  `json:"remote"`
	Inflight int     `json:"inflight"`
	Bytes    int64   `json:"bytes"`
	Rate     float64 `json:"rate"`
}

// ExportItemView is one planned file. Rel is where it lands under the
// destination; VPath is where it came from.
type ExportItemView struct {
	Rel       string `json:"rel"`
	VPath     string `json:"vpath"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	State     string `json:"state"`
	DoneBytes int64  `json:"done_bytes"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
}

// ExportsResponse answers the list, the detail view and every mutation, the
// way CopiesResponse does: one shape the CLI and the page can both render.
type ExportsResponse struct {
	Jobs       []ExportJobView     `json:"jobs"`
	NextCursor string              `json:"next_cursor,omitempty"`
	Progress   *ExportProgressView `json:"progress,omitempty"`
	// Items is filled for a single job only; a listing would otherwise carry
	// every planned file of every job.
	Items          []ExportItemView `json:"items,omitempty"`
	ItemNextCursor string           `json:"item_next_cursor,omitempty"`
	// Forgotten names the job whose record and part files were removed.
	Forgotten string `json:"forgotten,omitempty"`
}

func exportJobView(j export.Job) ExportJobView {
	v := ExportJobView{
		ID: j.ID, State: string(j.State), PauseReason: string(j.PauseReason),
		Sources: j.Sources, Dest: j.Dest,
		Mirror: j.Options.Mirror, Verify: j.Options.Verify,
		Transfers: j.Options.Transfers, Streams: j.Options.Streams, RangeSize: j.Options.RangeSize,
		FilesTotal: j.FilesTotal, FilesDone: j.FilesDone, FilesSkipped: j.FilesSkipped, FilesFailed: j.FilesFailed,
		BytesTotal: j.BytesTotal, BytesDone: j.BytesDone,
		LastError: j.LastError,
	}
	if v.Sources == nil {
		v.Sources = []string{}
	}
	for _, p := range []struct {
		t   time.Time
		out *string
	}{{j.CreatedAt, &v.CreatedAt}, {j.UpdatedAt, &v.UpdatedAt}, {j.FinishedAt, &v.FinishedAt}} {
		if !p.t.IsZero() {
			*p.out = p.t.Format(time.RFC3339)
		}
	}
	return v
}

// exportKind names the two kinds a plan holds. provider.Kind is an integer,
// and a page showing "0" for a file would be a riddle.
func exportKind(k provider.Kind) string {
	if k == provider.KindDir {
		return "directory"
	}
	return "file"
}

func exportProgressView(p export.Progress) ExportProgressView {
	v := ExportProgressView{
		BytesDone: p.BytesDone, BytesTotal: p.BytesTotal,
		Rate: p.Rate, ETASeconds: p.ETA.Seconds(), Members: []ExportMemberProgressView{},
	}
	for _, member := range p.Members {
		v.Members = append(v.Members, ExportMemberProgressView{
			Remote: member.Remote, Inflight: member.Inflight, Bytes: member.Bytes, Rate: member.Rate,
		})
	}
	return v
}

func exportItemViews(items []export.Item) []ExportItemView {
	out := make([]ExportItemView, 0, len(items))
	for _, it := range items {
		out = append(out, ExportItemView{
			Rel: it.Rel, VPath: it.VPath, Kind: exportKind(it.Kind), Size: it.Size,
			State: string(it.State), DoneBytes: it.DoneBytes, Attempts: it.Attempts,
			LastError: it.LastError,
		})
	}
	return out
}

// CallExport starts one export against the running daemon.
func CallExport(ctx context.Context, socket, tcp string, q ExportRequest) (ExportStartResponse, bool, error) {
	var out ExportStartResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return out, false, err
	}
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/export", body, &out)
	return out, online, err
}

// CallExports reads a page of jobs, or one job with its progress and plan.
func CallExports(ctx context.Context, socket, tcp string, q ExportsRequest) (ExportsResponse, bool, error) {
	var out ExportsResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	route := "/exports"
	if q.ID != "" {
		route += "/" + q.ID
	} else {
		params := make([]string, 0, 2)
		if q.Limit > 0 {
			params = append(params, "limit="+strconv.Itoa(q.Limit))
		}
		if q.Cursor != "" {
			params = append(params, "cursor="+q.Cursor)
		}
		if len(params) > 0 {
			route += "?" + strings.Join(params, "&")
		}
	}
	online, err := callControl(ctx, socket, tcp, http.MethodGet, route, nil, &out)
	return out, online, err
}

// CallExportMutation pauses, resumes, cancels or forgets one job.
func CallExportMutation(ctx context.Context, socket, tcp string, q ExportMutationRequest) (ExportsResponse, bool, error) {
	var out ExportsResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return out, false, err
	}
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/exports/"+q.Action, body, &out)
	return out, online, err
}

// exportManager answers the request itself when this daemon runs no exports.
func (s *Server) exportManager(w http.ResponseWriter, r *http.Request) (ExportManager, bool) {
	if s.collector.Export == nil {
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.export_unwired")
		return nil, false
	}
	return s.collector.Export, true
}

// exportError maps an export failure onto a status code without repeating the
// manager's own English text, which may name a destination path or a backend
// response.
func exportStatus(err error) int {
	switch {
	case errors.Is(err, export.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, export.ErrInvalid), errors.Is(err, export.ErrMirrorMarkerMissing):
		return http.StatusBadRequest
	case errors.Is(err, export.ErrNotOwner):
		return http.StatusConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	default:
		return http.StatusConflict
	}
}

// POST /export
func (s *Server) startExport(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		httpErrorT(w, r, http.StatusBadRequest, "err.no_query_params")
		return
	}
	var q ExportRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A mirror is the only export that deletes anything, so it is the only
	// one that needs saying twice. The runner still refuses it without a
	// marker from an earlier export of the same sources.
	if q.Mirror && !confirmed(w, r, q.Confirm, "confirm.export_mirror") {
		return
	}
	m, ok := s.exportManager(w, r)
	if !ok {
		return
	}
	job, err := m.Create(r.Context(), export.Request{
		Sources: q.Sources, Dest: q.Dest, Mirror: q.Mirror, Verify: q.Verify,
		Transfers: q.Transfers, Streams: q.Streams, RangeSize: q.RangeSize,
	})
	if err != nil {
		status := exportStatus(err)
		if status == http.StatusBadRequest {
			// The refusals a person has to act on name the path or the
			// destination that was wrong; a generic sentence would hide it.
			http.Error(w, err.Error(), status)
			return
		}
		httpErrorT(w, r, status, "err.export_start_failed")
		return
	}
	writeJSON(w, ExportStartResponse{ID: job.ID})
}

// GET /exports?limit&cursor
func (s *Server) exports(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	q := ExportsRequest{Cursor: r.URL.Query().Get("cursor"), Limit: defaultExportLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpErrorT(w, r, http.StatusBadRequest, "err.invalid_limit")
			return
		}
		q.Limit = n
	}
	if err := q.Validate(); err != nil {
		httpErrorT(w, r, http.StatusBadRequest, "err.limit_range", maxExportLimit)
		return
	}
	m, ok := s.exportManager(w, r)
	if !ok {
		return
	}
	jobs, next, err := m.Jobs(r.Context(), q.Limit, q.Cursor)
	if err != nil {
		httpErrorT(w, r, exportStatus(err), "err.export_list_failed")
		return
	}
	out := ExportsResponse{Jobs: make([]ExportJobView, 0, len(jobs)), NextCursor: next}
	for _, j := range jobs {
		v := exportJobView(j)
		if j.State == export.StateRunning || j.State == export.StatePlanning {
			if p, err := m.Progress(r.Context(), j.ID); err == nil {
				v.Rate, v.ETASeconds = p.Rate, p.ETA.Seconds()
			}
		}
		out.Jobs = append(out.Jobs, v)
	}
	writeJSON(w, out)
}

// GET /exports/<id> and POST /exports/pause|resume|cancel|forget.
//
// One prefix serves both because a job ID is a UUID and the four action names
// are not, so the two can never be confused for one another.
func (s *Server) exportByPath(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/exports/")
	if strings.HasSuffix(tail, "/items") {
		s.exportItems(w, r, strings.TrimSuffix(tail, "/items"))
		return
	}
	if exportActions[tail] {
		s.mutateExport(w, r, tail)
		return
	}
	if !validExportID(tail) {
		http.NotFound(w, r)
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	m, ok := s.exportManager(w, r)
	if !ok {
		return
	}
	out, err := s.exportDetail(r.Context(), m, tail)
	if err != nil {
		httpErrorT(w, r, exportStatus(err), "err.export_not_found")
		return
	}
	writeJSON(w, out)
}

// exportDetail is the job, its live progress and its plan.
func (s *Server) exportDetail(ctx context.Context, m ExportManager, id string) (ExportsResponse, error) {
	job, err := m.Job(ctx, id)
	if err != nil {
		return ExportsResponse{}, err
	}
	view := exportJobView(job)
	p, err := m.Progress(ctx, id)
	if err != nil {
		return ExportsResponse{}, err
	}
	pv := exportProgressView(p)
	view.Rate, view.ETASeconds = pv.Rate, pv.ETASeconds
	out := ExportsResponse{Jobs: []ExportJobView{view}, Progress: &pv}
	items, next, err := m.ItemsPage(ctx, id, "", defaultExportLimit, "")
	if err != nil {
		return ExportsResponse{}, err
	}
	out.Items = exportItemViews(items)
	out.ItemNextCursor = next
	return out, nil
}

// GET /exports/<id>/items?cursor=&limit=&state=
func (s *Server) exportItems(w http.ResponseWriter, r *http.Request, id string) {
	if !validExportID(id) {
		http.NotFound(w, r)
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	limit := defaultExportLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxExportLimit {
			httpErrorT(w, r, http.StatusBadRequest, "err.invalid_limit")
			return
		}
		limit = n
	}
	state := export.ItemState(r.URL.Query().Get("state"))
	switch state {
	case "", export.ItemPending, export.ItemActive, export.ItemDone, export.ItemSkipped, export.ItemFailed:
	default:
		http.Error(w, "exports: invalid item state", http.StatusBadRequest)
		return
	}
	m, ok := s.exportManager(w, r)
	if !ok {
		return
	}
	items, next, err := m.ItemsPage(r.Context(), id, r.URL.Query().Get("cursor"), limit, state)
	if err != nil {
		httpErrorT(w, r, exportStatus(err), "err.export_not_found")
		return
	}
	writeJSON(w, ExportsResponse{Jobs: []ExportJobView{}, Items: exportItemViews(items), ItemNextCursor: next})
}

func (s *Server) mutateExport(w http.ResponseWriter, r *http.Request, action string) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		httpErrorT(w, r, http.StatusBadRequest, "err.no_query_params")
		return
	}
	q := ExportMutationRequest{Action: action}
	if !decodeMutationLimit(w, r, &q, 4096) {
		return
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Forget removes the record and the partial files underneath it; the
	// finished copies are the user's own and are never touched.
	if action == "forget" && !confirmed(w, r, q.Confirm, "confirm.export_forget") {
		return
	}
	m, ok := s.exportManager(w, r)
	if !ok {
		return
	}
	var err error
	switch action {
	case "pause":
		err = m.Pause(r.Context(), q.ID)
	case "resume":
		err = m.Resume(r.Context(), q.ID)
	case "cancel":
		err = m.Cancel(r.Context(), q.ID)
	case "forget":
		err = m.Forget(r.Context(), q.ID)
	}
	if err != nil {
		httpErrorT(w, r, exportStatus(err), "err.export_action_failed", action)
		return
	}
	if action == "forget" {
		writeJSON(w, ExportsResponse{Jobs: []ExportJobView{}, Forgotten: q.ID})
		return
	}
	out, err := s.exportDetail(r.Context(), m, q.ID)
	if err != nil {
		httpErrorT(w, r, exportStatus(err), "err.export_state_unverified")
		return
	}
	writeJSON(w, out)
}
