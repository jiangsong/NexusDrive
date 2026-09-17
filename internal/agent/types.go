package agent

import (
	"encoding/json"
	"time"
)

// Scope is what a principal or session may touch inside the mount. The
// struct lives here because the rows below embed it; the path checks and
// narrowing rules that give it meaning are in scope.go.
type Scope struct {
	Read      []string  `json:"read,omitempty"` // empty = whole mount
	Write     []string  `json:"write"`          // nil = same as Read; empty non-nil = none
	ReadOnly  bool      `json:"read_only,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Sandbox   string    `json:"sandbox,omitempty"` // non-empty: write narrowed to this directory
}

// Principal is who is talking to the MCP server: the stdio child an agent
// spawned, an environment token, a loopback caller, a named access token, or
// the console. Only tokens carry a hash; it is never part of the JSON view.
type Principal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // stdio | env | loopback | token | console | hook
	Name string `json:"name"`
	// Owner is the person this principal acts for — the memory layout v2
	// keys memories by it; "" means this machine's user (DefaultOwner).
	Owner       string    `json:"owner,omitempty"`
	Scope       Scope     `json:"scope"`
	TokenPrefix string    `json:"fingerprint,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitempty"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
}

// Session is one MCP connection as seen over time: which principal opened
// it, over which transport, how far its scope was narrowed, and what it left
// behind.
type Session struct {
	ID            string     `json:"id"`
	PrincipalID   string     `json:"principal_id"`
	ConnKey       string     `json:"-"`
	ClientName    string     `json:"client"`
	ClientVersion string     `json:"client_version,omitempty"`
	Transport     string     `json:"transport"` // stdio | http-legacy | http-token | http-loopback | console (a rollback run from the control plane)
	Scope         Scope      `json:"scope"`
	Workspace     string     `json:"workspace,omitempty"`
	Sandbox       bool       `json:"sandbox"`
	State         string     `json:"state"` // active | finished | expired | rolled_back
	StartedAt     time.Time  `json:"started_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	FinishedAt    time.Time  `json:"finished_at,omitempty"`
	Summary       string     `json:"summary,omitempty"`
	Writes        int        `json:"writes"`
	Artifacts     []Artifact `json:"artifacts,omitempty"`
	// OpsCount is how many writes the session recorded in session_ops,
	// which is what a rollback would look at.
	OpsCount int `json:"ops_count"`
	// RolledBackAt is when the session was rolled back; zero otherwise.
	RolledBackAt time.Time `json:"rolled_back_at,omitzero"`
	// LastChangeSeen is the changes-table cursor the session's pull_events
	// (or its client's turn-start hook) last returned; 0 when it never
	// pulled.
	LastChangeSeen int64 `json:"last_change_seen,omitempty"`
}

// Artifact is a file a session declared as one of its deliverables.
type Artifact struct {
	Path        string    `json:"path"`
	URI         string    `json:"uri"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256,omitempty"`
	State       string    `json:"state"` // synced | local
	DownloadURL string    `json:"download_url,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
}

// AuditRow is one recorded tool call. Args is the redacted argument object;
// Paths are the mount paths the call named, so a row can be found by
// directory without parsing Args.
type AuditRow struct {
	ID          int64           `json:"id"`
	TS          time.Time       `json:"ts"`
	PrincipalID string          `json:"principal_id"`
	SessionID   string          `json:"session_id"`
	Transport   string          `json:"transport"`
	Tool        string          `json:"tool"`
	Paths       []string        `json:"paths"`
	Args        json.RawMessage `json:"args"`
	BytesIn     int64           `json:"bytes_in"`
	BytesOut    int64           `json:"bytes_out"`
	// TokensOut is the estimated tokens of the result (EstimateTokens),
	// what the agent's context actually paid.
	TokensOut  int64  `json:"tokens_out"`
	Result     string `json:"result"` // ok | denied | error | oversize
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Event is what Watch delivers: a new audit row, a session that changed,
// or a trigger delivery that changed state.
type Event struct {
	Kind     string    `json:"kind"` // audit | session | trigger
	Audit    *AuditRow `json:"audit,omitempty"`
	Session  *Session  `json:"session,omitempty"`
	Delivery *Delivery `json:"delivery,omitempty"`
}

// Summary is the headline the console and the status endpoint show.
type Summary struct {
	Active      int `json:"active"`
	WritesToday int `json:"writes_today"`
	DeniedToday int `json:"denied_today"`
}
