package agent

import (
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"time"
)

// ErrWorkspaceUnset is returned when no delivery directory can be derived:
// mcp.workspace is not configured and the scope reads the whole mount, whose
// root is a synthesised layout directory nobody can write into.
var ErrWorkspaceUnset = errors.New("mcp.workspace is not configured and there is no --allow prefix to derive it from")

// ManifestName is the one file a session directory holds that the server
// manages; everything else in it belongs to the agent.
const ManifestName = "manifest.json"

// DefaultWorkspace resolves the workspace root sessions are created under.
// A configured value wins; otherwise the first read prefix of the scope,
// joined with ".agent", so a mount allowed at /work delivers into
// /work/.agent. A scope over the whole mount has no such prefix.
func DefaultWorkspace(configured string, sc Scope) (string, error) {
	if configured != "" {
		return Normalise(configured), nil
	}
	for _, p := range sc.EffectiveRead() {
		if p != "/" {
			return path.Join(p, ".agent"), nil
		}
	}
	return "", ErrWorkspaceUnset
}

// SessionDirName is the directory a session delivers into, relative to the
// workspace root: "<client>-<YYYYMMDD>-<id[:8]>". The client name is
// lowered and reduced to [a-z0-9-] so it is safe on every provider; an
// empty one becomes "agent". The id fragment keeps two sessions of one
// client on one day apart, so a directory is never reused.
func SessionDirName(client string, started time.Time, id string) string {
	name := safeName(client)
	if name == "" {
		name = "agent"
	}
	if len(id) > 8 {
		id = id[:8]
	}
	return name + "-" + started.UTC().Format("20060102") + "-" + id
}

// safeName lowers s and replaces every run of characters outside [a-z0-9]
// with one dash, trimming dashes at both ends.
func safeName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// ArtifactPaths derives a session's deliverables from its audit rows: the
// destination of every successful write-tool call except delete, that is
// the last path the call checked. Paths are unique and sorted; the
// session's own manifest is never one of them.
func ArtifactPaths(rows []AuditRow, sessionDir string) []string {
	manifest := ""
	if sessionDir != "" {
		manifest = path.Join(Normalise(sessionDir), ManifestName)
	}
	seen := map[string]struct{}{}
	out := []string{}
	for _, r := range rows {
		if r.Result != "ok" || !WriteTools[r.Tool] || r.Tool == "delete" || len(r.Paths) == 0 {
			continue
		}
		p := r.Paths[len(r.Paths)-1]
		if p == "" || p == manifest {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Manifest is manifest.json, the one server-managed file in a session
// directory. It is written as a skeleton when the session begins and in
// full when it finishes.
type Manifest struct {
	SessionID  string     `json:"session_id"`
	Client     string     `json:"client"`
	Principal  string     `json:"principal"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Scope      Scope      `json:"scope"`
	Summary    string     `json:"summary,omitempty"`
	Artifacts  []Artifact `json:"artifacts"`
}

// ManifestOf builds the manifest of a session. principal is the
// "<kind>:<name>" label of the principal that opened it.
func ManifestOf(s Session, principal string) Manifest {
	m := Manifest{
		SessionID: s.ID, Client: s.ClientName, Principal: principal,
		StartedAt: s.StartedAt.UTC(), Scope: s.Scope, Summary: s.Summary,
		Artifacts: s.Artifacts,
	}
	if m.Artifacts == nil {
		m.Artifacts = []Artifact{}
	}
	if !s.FinishedAt.IsZero() {
		finished := s.FinishedAt.UTC()
		m.FinishedAt = &finished
	}
	return m
}

// JSON encodes the manifest the way it is stored: indented, trailing
// newline, so `cat manifest.json` in a terminal reads well.
func (m Manifest) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ManifestPath is where a session directory keeps its manifest.
func ManifestPath(sessionDir string) string {
	return path.Join(Normalise(sessionDir), ManifestName)
}
