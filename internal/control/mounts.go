package control

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"cloudfs/internal/config"
)

// The mounts page edits the configuration only. Which mount this daemon
// serves, and the *vfs.FS built for it, are fixed when the process starts:
// every change here is durable at once and takes effect at the next start,
// and every reply says restart_required so the page cannot imply otherwise.

// MountView is one layout as configured, and whether this daemon serves it.
type MountView struct {
	Path   string `json:"path"`
	Prefix string `json:"prefix"`
	Remote string `json:"remote"`
	Root   string `json:"root,omitempty"`
	Mode   string `json:"mode"`
	Pin    bool   `json:"pin,omitempty"`
	DirTTL string `json:"dir_ttl,omitempty"`
	Active bool   `json:"active"`
}

// MountRequest is POST (add) and PATCH (replace) /mounts.
type MountRequest struct {
	Path   string `json:"path"`
	Prefix string `json:"prefix"`
	Remote string `json:"remote"`
	Root   string `json:"root,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Pin    bool   `json:"pin,omitempty"`
	DirTTL string `json:"dir_ttl,omitempty"`
}

// MountMutationResponse follows every mount edit.
type MountMutationResponse struct {
	Mounts          []MountView `json:"mounts"`
	RestartRequired bool        `json:"restart_required"`
}

func (s *Server) mountViews() []MountView {
	out := []MountView{}
	cfg := s.collector.Config
	if cfg == nil {
		return out
	}
	active := map[string]bool{}
	if s.collector.FS != nil {
		for _, m := range s.collector.FS.Mounts() {
			active[m.Remote+"\x00"+m.Prefix] = true
		}
	}
	for _, m := range cfg.Mounts {
		prefixes := make([]string, 0, len(m.Layout))
		for prefix := range m.Layout {
			prefixes = append(prefixes, prefix)
		}
		sort.Strings(prefixes)
		for _, prefix := range prefixes {
			l := m.Layout[prefix]
			v := MountView{Path: m.Path, Prefix: prefix, Remote: l.Remote, Root: l.Root, Mode: string(l.Mode), Pin: l.Pin, Active: active[l.Remote+"\x00"+prefix]}
			if v.Mode == "" {
				v.Mode = string(config.ModeWriteback)
			}
			if l.DirTTL > 0 {
				v.DirTTL = l.DirTTL.String()
			}
			out = append(out, v)
		}
	}
	return out
}

func layoutFromRequest(in MountRequest) (config.Layout, error) {
	l := config.Layout{Remote: in.Remote, Root: in.Root, Mode: config.Mode(in.Mode), Pin: in.Pin}
	if in.Path == "" || in.Prefix == "" || in.Remote == "" {
		return l, fmt.Errorf("path, prefix and remote are required")
	}
	if in.DirTTL != "" {
		d, err := time.ParseDuration(in.DirTTL)
		if err != nil || d < 0 {
			return l, fmt.Errorf("invalid dir_ttl")
		}
		l.DirTTL = d
	}
	return l, nil
}

// GET /mounts · POST /mounts · PATCH /mounts · DELETE /mounts?path=&prefix=&confirm=true
func (s *Server) mounts(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, MountMutationResponse{Mounts: s.mountViews()})
		return
	}
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no configuration file", http.StatusConflict)
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPatch:
		var in MountRequest
		if !decodeMutation(w, r, &in) {
			return
		}
		l, err := layoutFromRequest(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost {
			err = config.AddMount(cfg.SourcePath, in.Path, in.Prefix, l)
		} else {
			err = config.SetLayout(cfg.SourcePath, in.Path, in.Prefix, l)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	case http.MethodDelete:
		q := r.URL.Query()
		if err := requireConfirm(q.Get("confirm") == "true", "removes the mount layout from the configuration"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if q.Get("path") == "" || q.Get("prefix") == "" {
			http.Error(w, "path and prefix are required", http.StatusBadRequest)
			return
		}
		if err := config.RemoveMount(cfg.SourcePath, q.Get("path"), q.Get("prefix")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.reloadConfigView()
	writeJSON(w, MountMutationResponse{Mounts: s.mountViews(), RestartRequired: true})
}
