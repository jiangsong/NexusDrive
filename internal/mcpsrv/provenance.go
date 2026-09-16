package mcpsrv

import (
	"context"
	"fmt"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Provenance (docs/agent-first-design.md §6.2, TODO.md T-51): stat,
// stat_many and a full list_directory say who last changed a path, and
// history lists what happened to it, from the changes table the owner
// daemon keeps. The session id there is the same one list_sessions shows,
// so an agent can tell its own earlier run from a teammate's device.

// Provenance is what the tools read; agent.Store satisfies it.
type Provenance interface {
	LastWriter(ctx context.Context, path string) (agent.Change, bool, error)
	History(ctx context.Context, path string, limit int) ([]agent.Change, error)
}

// lastWriter is the last_writer field: who changed the path most
// recently as far as this daemon saw.
type lastWriter struct {
	// Origin is kernel (a program using the mount), mcp (an agent, see
	// session_id), control, webdav, or remote (the provider: another
	// device, the web client, a share).
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id,omitempty"`
	Principal string `json:"principal,omitempty"`
	At        string `json:"at"`
}

// lastWriterOf looks a path up; nil when nothing is known, which is the
// common case for a file last changed before this daemon started
// recording.
func (s *Server) lastWriterOf(ctx context.Context, p string) *lastWriter {
	if s.provenance == nil {
		return nil
	}
	c, ok, err := s.provenance.LastWriter(ctx, p)
	if err != nil || !ok {
		return nil
	}
	return &lastWriter{Origin: c.Origin, Kind: c.Kind, SessionID: c.SessionID, Principal: c.Principal, At: c.TS.UTC().Format(time.RFC3339)}
}

type historyInput struct {
	Path  string `json:"path" jsonschema:"Mount-relative file or directory; a directory lists changes to what is under it"`
	Limit int    `json:"limit,omitempty" jsonschema:"Newest entries to return; default 50, at most 500"`
}

type historyEntry struct {
	At        string `json:"at"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	From      string `json:"from,omitempty"`
	Origin    string `json:"origin"`
	SessionID string `json:"session_id,omitempty"`
	Principal string `json:"principal,omitempty"`
	// Reliable is false on a rescan entry: the daemon lost track of
	// changes around that time, so gaps are possible.
	Reliable bool `json:"reliable"`
}

type historyOutput struct {
	Path    string         `json:"path"`
	Entries []historyEntry `json:"entries"`
	// Note explains an empty answer: the record starts when the daemon
	// did, and is kept for mcp.session.retain.
	Note string `json:"note,omitempty"`
}

func (s *Server) registerHistoryTool() {
	if s.provenance == nil {
		return
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "history",
		Description: "List what happened to a path as this daemon saw it, newest first: writes through the mount, agent sessions (with their id), " +
			"the console, WebDAV, and changes found on the remote. It is not the provider's version history; it starts when the daemon started recording and is kept for the session retention period.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.history)
}

func (s *Server) history(ctx context.Context, _ *mcp.CallToolRequest, in historyInput) (*mcp.CallToolResult, historyOutput, error) {
	p, err := s.checkPath(ctx, in.Path, false)
	if err != nil {
		r, _ := fail(err)
		return r, historyOutput{}, nil
	}
	rows, err := s.provenance.History(ctx, p, in.Limit)
	if err != nil {
		r, _ := fail(err)
		return r, historyOutput{}, nil
	}
	out := historyOutput{Path: p, Entries: []historyEntry{}}
	for _, c := range rows {
		if c.Path != "/" && !s.visible(ctx, c.Path) {
			continue
		}
		out.Entries = append(out.Entries, historyEntry{At: c.TS.UTC().Format(time.RFC3339), Path: c.Path, Kind: c.Kind, From: s.visibleFrom(ctx, c.From),
			Origin: c.Origin, SessionID: c.SessionID, Principal: c.Principal, Reliable: c.Reliable})
	}
	if keep, cut := cutItems(len(out.Entries), s.tokenBudget(), func(int) int { return 40 }); cut {
		out.Entries = out.Entries[:keep]
	}
	if len(out.Entries) == 0 {
		out.Note = "no recorded change: the record starts when this daemon began recording and is pruned after mcp.session.retain"
	}
	return text("%d changes to %s", len(out.Entries), p), out, nil
}

// String is for logs.
func (w *lastWriter) String() string {
	if w == nil {
		return "unknown"
	}
	return fmt.Sprintf("%s/%s at %s", w.Origin, w.Kind, w.At)
}
