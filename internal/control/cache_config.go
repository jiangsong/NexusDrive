package control

import (
	"errors"
	"io/fs"
	"net/http"
	"syscall"

	"cloudfs/internal/config"
)

// CacheBudget is the cache's size cap and disk headroom, as the console
// shows and edits them. Bytes, not strings: the page formats them.
type CacheBudget struct {
	// MaxBytes caps what the cache keeps on disk; 0 means no cap.
	MaxBytes int64 `json:"max_bytes"`
	// MinFree is how much of the cache filesystem stays free. A write that
	// would take the disk below it is refused with ENOSPC; 0 keeps no
	// headroom at all.
	MinFree int64 `json:"min_free"`
	// FreeBytes is what the cache filesystem has free right now, so the
	// page can say whether writes are being refused.
	FreeBytes int64 `json:"free_bytes"`
	// Dir is the cache directory, which is what the free space is measured
	// on.
	Dir string `json:"dir,omitempty"`
	// Applied says the running daemon took the new figures. When it is
	// false the file was saved and a restart applies them.
	Applied bool `json:"applied,omitempty"`
}

// cacheConfig answers GET /cache/config with the live budget and PUT with a
// change to it: saved to the configuration file first, so it survives a
// restart, then applied to the running cache, so a full disk that is
// refusing every write lets them through the moment the headroom is
// lowered.
func (s *Server) cacheConfig(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.cacheBudgetView())
	case http.MethodPut:
		cfg := s.collector.ConfigView()
		if cfg == nil || cfg.SourcePath == "" {
			httpErrorT(w, r, http.StatusConflict, "err.no_config")
			return
		}
		var in CacheBudget
		if !decodeMutation(w, r, &in) {
			return
		}
		if in.MaxBytes < 0 || in.MinFree < 0 {
			httpErrorT(w, r, http.StatusBadRequest, "cache.budget_negative")
			return
		}
		if err := config.SetCacheBudget(cfg.SourcePath, config.Size(in.MaxBytes), config.Size(in.MinFree)); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, fs.ErrNotExist) {
				status = http.StatusInternalServerError
			}
			http.Error(w, err.Error(), status)
			return
		}
		s.reloadConfigView()
		if s.collector.Cache != nil {
			s.collector.Cache.SetBudget(in.MaxBytes, in.MinFree)
		}
		out := s.cacheBudgetView()
		out.Applied = s.collector.Cache != nil
		writeJSON(w, out)
	default:
		allowMethod(w, r, http.MethodGet, http.MethodPut)
	}
}

// cacheBudgetView reads the budget from the running cache when there is
// one — that is what admits or refuses writes — and from the configuration
// otherwise.
func (s *Server) cacheBudgetView() CacheBudget {
	var out CacheBudget
	if c := s.collector.Cache; c != nil {
		out.MaxBytes, out.MinFree = c.Budget()
		out.Dir = c.Dir()
		if s.collector.FreeSpace != nil {
			if free, err := s.collector.FreeSpace(c.Dir()); err == nil {
				out.FreeBytes = free
			}
		}
	} else if cfg := s.collector.ConfigView(); cfg != nil {
		out.MaxBytes, out.MinFree = int64(cfg.Cache.MaxSize), int64(cfg.Cache.MinFree)
	}
	return out
}
