package control

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
)

// One account, as the connection settings page shows and edits it. The
// credential boundary is the same as for creating one: no secret value is
// ever in a reply, and a request naming a secret key is refused before the
// file is touched. Every edit here is durable at once and takes effect on the
// next start; the reply says so rather than let the page imply otherwise.

// AccountDetail is GET /accounts/{name}.
type AccountDetail struct {
	Name          string            `json:"name"`
	Type          string            `json:"type"`
	Proxy         string            `json:"proxy,omitempty"`
	QPS           *config.QPS       `json:"qps,omitempty"`
	UploadWorkers int               `json:"upload_workers,omitempty"`
	Fields        map[string]string `json:"fields"`
	// HasCredentials says whether any credential is configured, never which
	// or what.
	HasCredentials bool           `json:"has_credentials"`
	Caps           *CapsView      `json:"caps,omitempty"`
	Mounts         []AccountMount `json:"mounts"`
	// Live is true when this daemon has the remote assembled, so Caps come
	// from the running backend rather than being absent.
	Live bool `json:"live"`
}

// AccountMount is one layout that shows this remote.
type AccountMount struct {
	Path   string `json:"path"`
	Prefix string `json:"prefix"`
	Mode   string `json:"mode"`
}

// CapsView is provider.Caps for the wire: the facts the page greys options
// out by. It is built from the live provider, so it is the truth of this
// build, not a table kept by hand.
type CapsView struct {
	HashTypes      []string     `json:"hash_types"`
	RapidUpload    []string     `json:"rapid_upload"`
	RangeRead      bool         `json:"range_read"`
	Delta          bool         `json:"delta"`
	ServerCopy     bool         `json:"server_copy"`
	ServerMove     bool         `json:"server_move"`
	ServerRename   bool         `json:"server_rename"`
	SinglePutMax   int64        `json:"single_put_max"`
	PartSize       int64        `json:"part_size"`
	UploadParallel int          `json:"upload_parallel"`
	LinkTTL        string       `json:"link_ttl,omitempty"`
	LinkShareable  bool         `json:"link_shareable"`
	QPS            provider.QPS `json:"qps"`
	Tier           string       `json:"tier"`
}

// AccountPatch is PATCH /accounts/{name}. A nil field value deletes the key;
// "" for proxy and 0 for upload_workers return them to the default.
type AccountPatch struct {
	Fields        map[string]*string `json:"fields,omitempty"`
	Proxy         *string            `json:"proxy,omitempty"`
	QPS           *config.QPS        `json:"qps,omitempty"`
	UploadWorkers *int               `json:"upload_workers,omitempty"`
}

// AccountMutationResponse follows every account edit.
type AccountMutationResponse struct {
	Name            string         `json:"name"`
	RestartRequired bool           `json:"restart_required"`
	Detail          *AccountDetail `json:"detail,omitempty"`
}

// AccountCheckResponse is POST /accounts/{name}/check.
type AccountCheckResponse struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func capsView(c provider.Caps) *CapsView {
	hashes := func(in []provider.HashType) []string {
		out := make([]string, 0, len(in))
		for _, h := range in {
			out = append(out, string(h))
		}
		return out
	}
	v := &CapsView{
		HashTypes: hashes(c.HashTypes), RapidUpload: hashes(c.RapidUpload),
		RangeRead: c.RangeRead, Delta: c.Delta,
		ServerCopy: c.ServerCopy, ServerMove: c.ServerMove, ServerRename: c.ServerRename,
		SinglePutMax: c.SinglePutMax, PartSize: c.PartSize, UploadParallel: c.UploadParallel,
		LinkShareable: c.LinkShareable, QPS: c.QPS, Tier: string(c.Tier),
	}
	if c.LinkTTL > 0 {
		v.LinkTTL = c.LinkTTL.Round(time.Second).String()
	}
	return v
}

// accountDetail assembles the view from the configuration and, when the
// remote is live in this daemon, its capabilities.
func (s *Server) accountDetail(name string) (AccountDetail, bool) {
	cfg := s.collector.Config
	if cfg == nil {
		return AccountDetail{}, false
	}
	r, ok := cfg.Remotes[name]
	if !ok {
		return AccountDetail{}, false
	}
	d := AccountDetail{Name: name, Type: r.Type, Proxy: r.Proxy, QPS: r.QPS, UploadWorkers: r.UploadWorkers, Fields: map[string]string{}, Mounts: []AccountMount{}}
	for k, v := range r.Extra {
		// A legacy inline secret can still sit in Extra before migration;
		// filter, do not assume none is there.
		if config.IsSecretField(k) {
			if text, ok := v.(string); ok && text != "" {
				d.HasCredentials = true
			}
			continue
		}
		if strings.HasPrefix(k, "_") {
			continue
		}
		d.Fields[k] = fmt.Sprint(v)
	}
	for _, m := range cfg.Mounts {
		prefixes := make([]string, 0, len(m.Layout))
		for prefix := range m.Layout {
			prefixes = append(prefixes, prefix)
		}
		sort.Strings(prefixes)
		for _, prefix := range prefixes {
			l := m.Layout[prefix]
			if l.Remote != name {
				continue
			}
			mode := string(l.Mode)
			if mode == "" {
				mode = string(config.ModeWriteback)
			}
			d.Mounts = append(d.Mounts, AccountMount{Path: m.Path, Prefix: prefix, Mode: mode})
		}
	}
	if p, ok := s.collector.Providers[name]; ok && p != nil {
		d.Live = true
		d.Caps = capsView(p.Capabilities())
	}
	return d, true
}

// accountByName dispatches /accounts/{name}[/check].
func (s *Server) accountByName(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/accounts/")
	name, action, _ := strings.Cut(rest, "/")
	if !safeRemoteName(name) {
		http.Error(w, "invalid remote name", http.StatusBadRequest)
		return
	}
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no configuration file", http.StatusConflict)
		return
	}
	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			d, ok := s.accountDetail(name)
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, d)
		case http.MethodPatch:
			s.patchAccount(w, r, name)
		case http.MethodDelete:
			s.deleteAccount(w, r, name)
		default:
			w.Header().Set("Allow", "GET, PATCH, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "check":
		s.checkAccount(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) patchAccount(w http.ResponseWriter, r *http.Request, name string) {
	var q AccountPatch
	if !decodeMutation(w, r, &q) {
		return
	}
	keys := map[string]string{}
	for k := range q.Fields {
		keys[k] = ""
	}
	if err := rejectSecretFields(name, keys); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for k := range q.Fields {
		if !config.SafeExtraFieldName(k) {
			http.Error(w, fmt.Sprintf("invalid setting name %q", k), http.StatusBadRequest)
			return
		}
	}
	if _, ok := s.collector.Config.Remotes[name]; !ok {
		http.NotFound(w, r)
		return
	}
	err := config.SetRemoteField(s.collector.Config.SourcePath, name, config.SetRemoteFieldOptions{
		Fields: q.Fields, Proxy: q.Proxy, QPS: q.QPS, UploadWorkers: q.UploadWorkers,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.reloadConfigView()
	d, _ := s.accountDetail(name)
	writeJSON(w, AccountMutationResponse{Name: name, RestartRequired: true, Detail: &d})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, name string) {
	if err := requireConfirm(r.URL.Query().Get("confirm") == "true", "removes the account from the configuration"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := s.collector.Config.Remotes[name]; !ok {
		http.NotFound(w, r)
		return
	}
	if err := config.RemoveRemote(s.collector.Config.SourcePath, name); err != nil {
		status := http.StatusConflict
		if errors.Is(err, errConfirmRequired) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	s.reloadConfigView()
	writeJSON(w, AccountMutationResponse{Name: name, RestartRequired: true})
}

func (s *Server) checkAccount(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.collector.CheckAccount == nil {
		http.Error(w, "account checks are not wired on this daemon", http.StatusNotImplemented)
		return
	}
	if _, ok := s.collector.Config.Remotes[name]; !ok {
		http.NotFound(w, r)
		return
	}
	out := AccountCheckResponse{Name: name, OK: true}
	if err := s.collector.CheckAccount(r.Context(), name); err != nil {
		// Already sanitised by the daemon; still, never the raw provider text.
		out.OK, out.Error = false, err.Error()
	}
	writeJSON(w, out)
}

// reloadConfigView re-reads the configuration file after an edit so that the
// next GET reflects it. The daemon's own assembled state does not change —
// that is what restart_required tells the caller — but the page should not
// have to guess what the file now says.
func (s *Server) reloadConfigView() {
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		return
	}
	fresh, err := config.Load(cfg.SourcePath)
	if err != nil {
		return
	}
	// Only the sections the editors touch; the rest of the running
	// configuration stays what the daemon started with.
	cfg.Remotes = fresh.Remotes
	cfg.Mounts = fresh.Mounts
	cfg.Proxy = fresh.Proxy
}
