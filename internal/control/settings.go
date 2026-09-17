package control

import (
	"net/http"
	"strconv"
	"time"

	"cloudfs/internal/config"
)

// GET /settings is the read-only settings screen (ui-plan G3, 2026-09-17
// decision): the agent-facing configuration as it is in force, in a
// whitelisted view the browser renders with the YAML that would set each
// value. Nothing here is written back — configuration is defined in the
// file and only there, because a browser must not be able to define what
// runs on this machine (docs/agent-roadmap.md §6.4). The view names no
// remote, no credential and no path outside the mount-relative ones the
// agent already sees, so TestSettingsViewCarriesNoSecret can walk it.

// SettingsView is GET /settings.
type SettingsView struct {
	MCP    SettingsMCP    `json:"mcp"`
	Hooks  SettingsHooks  `json:"hooks"`
	Memory SettingsMemory `json:"memory"`
	Index  SettingsIndex  `json:"index"`
	Heat   SettingsHeat   `json:"heat"`
	Share  SettingsShare  `json:"share"`
}

// SettingsShare is the share section (T-55).
type SettingsShare struct {
	ConsoleLinks  bool   `json:"console_links"`
	DefaultExpiry string `json:"default_expiry"`
	Render        struct {
		Enabled  bool   `json:"enabled"`
		Listen   string `json:"listen,omitempty"`
		TokenTTL string `json:"token_ttl"`
	} `json:"render"`
}

// SettingsMCP is the mcp section.
type SettingsMCP struct {
	HTTP        string   `json:"http"`
	Allow       []string `json:"allow"`
	ReadOnly    bool     `json:"read_only"`
	Workspace   string   `json:"workspace,omitempty"`
	MaxTokens   int      `json:"max_tokens"`
	Transport   string   `json:"install_transport"`
	AuditRetain string   `json:"audit_retain"`
	Session     struct {
		Idle             string `json:"idle"`
		Retain           string `json:"retain"`
		RetainBlobs      string `json:"retain_blobs"`
		MaxPreimageBytes int64  `json:"max_preimage_bytes"`
		PreimageFiles    int    `json:"preimage_files"`
	} `json:"session"`
}

// SettingsHooks is the hooks section.
type SettingsHooks struct {
	Context         string `json:"context"`
	ChangedMax      int    `json:"changed_max"`
	MemoryHeadLines int    `json:"memory_head_lines"`
}

// SettingsMemory is the memory section.
type SettingsMemory struct {
	Root         string `json:"root,omitempty"`
	MaxFactBytes int64  `json:"max_fact_bytes"`
	// Layout is v1 or v2 as the drive is in (the marker, or the
	// configuration's override); "" without a memory store.
	Layout string `json:"layout,omitempty"`
}

// SettingsIndex is the index section: only whether it runs and what it
// covers by default, never the embedding endpoint (that has its own panel
// with the remote-data banner).
type SettingsIndex struct {
	Enabled bool `json:"enabled"`
	Pinned  bool `json:"pinned"`
	Rules   int  `json:"rules"`
}

// SettingsHeat is the read-heat section: whether reads are counted and
// how many days of buckets are kept before folding into the all-time row.
type SettingsHeat struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"`
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodGet) {
		return
	}
	cfg := s.collector.ConfigView()
	if cfg == nil {
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.no_config")
		return
	}
	v := settingsViewOf(cfg, s.collector.HeatStore != nil)
	if s.collector.Memory != nil {
		v.Memory.Layout = s.collector.Memory.Layout(r.Context())
	}
	writeJSON(w, v)
}

// settingsViewOf projects the configuration onto the whitelist.
func settingsViewOf(cfg *config.Config, heat bool) SettingsView {
	v := SettingsView{}
	v.MCP.HTTP = cfg.MCP.HTTP
	v.MCP.Allow = append([]string{}, cfg.MCP.Allow...)
	v.MCP.ReadOnly = cfg.MCP.ReadOnly
	v.MCP.Workspace = cfg.MCP.Workspace
	v.MCP.MaxTokens = cfg.MCP.Limits.MaxTokens
	v.MCP.Transport = cfg.MCP.Install.Transport
	if v.MCP.Transport == "" {
		v.MCP.Transport = "auto"
	}
	v.MCP.AuditRetain = durationText(cfg.MCP.Audit.Retain)
	v.MCP.Session.Idle = durationText(cfg.MCP.Session.Idle)
	v.MCP.Session.Retain = durationText(cfg.MCP.Session.Retain)
	v.MCP.Session.RetainBlobs = durationText(cfg.MCP.Session.RetainBlobs)
	v.MCP.Session.MaxPreimageBytes = int64(cfg.MCP.Session.MaxPreimageBytes)
	v.MCP.Session.PreimageFiles = cfg.MCP.Session.PreimageFiles
	v.Hooks.Context = cfg.Hooks.Context
	if v.Hooks.Context == "" {
		v.Hooks.Context = "minimal"
	}
	v.Hooks.ChangedMax = cfg.Hooks.ChangedMax
	if v.Hooks.ChangedMax <= 0 {
		v.Hooks.ChangedMax = config.DefaultHookChangedMax
	}
	v.Hooks.MemoryHeadLines = cfg.Hooks.MemoryHeadLines
	if v.Hooks.MemoryHeadLines == 0 {
		v.Hooks.MemoryHeadLines = config.DefaultHookMemoryHeadLines
	}
	v.Memory.Root = cfg.Memory.Root
	v.Memory.MaxFactBytes = int64(cfg.Memory.MaxFactBytes)
	v.Index.Enabled = cfg.Index.Enabled
	v.Index.Pinned = cfg.Index.Pinned
	v.Index.Rules = len(cfg.Index.Rules)
	v.Share.ConsoleLinks = cfg.Share.ConsoleLinksOn()
	v.Share.DefaultExpiry = durationText(cfg.Share.DefaultExpiry)
	v.Share.Render.Enabled = cfg.Share.Render.Enabled
	v.Share.Render.Listen = cfg.Share.Render.Listen
	v.Share.Render.TokenTTL = durationText(cfg.Share.Render.TokenTTL)
	v.Heat.Enabled = heat && cfg.MCP.Heat.On()
	v.Heat.RetentionDays = cfg.MCP.Heat.Retention()
	return v
}

// durationText renders a duration the way the YAML accepts it back:
// whole hours as "720h", whole minutes as "30m", anything else as Go
// prints it.
func durationText(d time.Duration) string {
	switch {
	case d == 0:
		return ""
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.String()
}
