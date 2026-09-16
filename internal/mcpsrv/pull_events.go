package mcpsrv

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pull_events (docs/agent-first-design.md §6.4, TODO.md T-52) is the pull
// side of the change record: what changed since a cursor, from the
// changes table, so a long-running agent (or the hook that runs at the
// start of every turn) can ask "what moved while I was not looking"
// without a subscription stream. The cursor is the row id; a session
// that passes none continues from where its last pull ended, and on its
// first pull from when it started. An agent's own changes are left out
// unless asked for: the question is what others did.

// EventSource is what pull_events reads; agent.Store satisfies it.
type EventSource interface {
	Changes(ctx context.Context, q agent.ChangesQuery) ([]agent.Change, bool, error)
	ChangeIDBefore(ctx context.Context, t time.Time) (int64, error)
	LastChangeID(ctx context.Context) (int64, error)
	LastChangeSeen(ctx context.Context, sessionID string) (int64, error)
	SetLastChangeSeen(ctx context.Context, sessionID string, id int64) error
}

type pullEventsInput struct {
	Cursor     string   `json:"cursor,omitempty" jsonschema:"Cursor from a previous call; omit to continue from this session's last pull (or its start)"`
	Path       string   `json:"path,omitempty" jsonschema:"Only changes at or under this path; default everything you may read"`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"Only these kinds: write, create, mkdir, remove, rename, remote; default all"`
	IncludeOwn bool     `json:"include_own,omitempty" jsonschema:"Also return changes this session made itself; default false"`
	Limit      int      `json:"limit,omitempty" jsonschema:"Maximum events; default 100, the server caps this"`
}

type pullEvent struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	From      string `json:"from,omitempty"`
	Origin    string `json:"origin"`
	SessionID string `json:"session_id,omitempty"`
}

type pullEventsOutput struct {
	Events []pullEvent `json:"events"`
	// Cursor is where the next call continues; it is stored for the
	// session as well, so omitting it next time is the same thing.
	Cursor string `json:"cursor"`
	More   bool   `json:"more"`
	// Rescan says the daemon lost track of changes in this range (the
	// feed overflowed): re-list what you rely on rather than trust the
	// events alone.
	Rescan bool   `json:"rescan"`
	Note   string `json:"note,omitempty"`
}

func (s *Server) registerPullEvents() {
	if s.events == nil {
		return
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "pull_events",
		Description: "Return what changed in the mount since a cursor: writes through the mount, other agents' sessions, the console, WebDAV and changes found on the remote. " +
			"Your own session's changes are left out unless include_own is true. Omit the cursor to continue from your last call (or from when your session started). " +
			"A rescan flag means the daemon lost changes in that range: re-list what you rely on.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.pullEvents)
}

func (s *Server) pullEvents(ctx context.Context, _ *mcp.CallToolRequest, in pullEventsInput) (*mcp.CallToolResult, pullEventsOutput, error) {
	root := "/"
	if in.Path != "" {
		var err error
		if root, err = s.checkPath(ctx, in.Path, false); err != nil {
			r, _ := fail(err)
			return r, pullEventsOutput{}, nil
		}
	}
	sess, hasSession := agent.FromContext(ctx)
	after, err := s.pullCursor(ctx, in.Cursor, sess, hasSession)
	if err != nil {
		r, _ := fail(err)
		return r, pullEventsOutput{}, nil
	}
	limit := in.Limit
	if limit <= 0 || limit > s.opt.Limits.MaxResults {
		limit = s.opt.Limits.MaxResults
	}
	out := pullEventsOutput{Events: []pullEvent{}}
	cursor := after
	// Rows the scope or the own-session filter drop still advance the
	// cursor; the page is refilled until limit events or the end.
	for len(out.Events) < limit {
		rows, more, err := s.events.Changes(ctx, agent.ChangesQuery{After: cursor, Prefix: root, Kinds: in.Kinds, Limit: limit})
		if err != nil {
			r, _ := fail(err)
			return r, pullEventsOutput{}, nil
		}
		for _, c := range rows {
			cursor = c.ID
			if c.Kind == "rescan" {
				out.Rescan = true
				continue
			}
			if !in.IncludeOwn && hasSession && c.SessionID == sess.ID {
				continue
			}
			if !s.visible(ctx, c.Path) {
				continue
			}
			out.Events = append(out.Events, pullEvent{ID: c.ID, At: c.TS.UTC().Format(time.RFC3339), Path: c.Path, Kind: c.Kind, From: s.visibleFrom(ctx, c.From), Origin: c.Origin, SessionID: c.SessionID})
			if len(out.Events) >= limit {
				out.More = more || c.ID < rows[len(rows)-1].ID
				break
			}
		}
		if !more || len(out.Events) >= limit {
			if len(out.Events) < limit {
				out.More = false
			}
			break
		}
	}
	if keep, cut := cutItems(len(out.Events), s.tokenBudget(), func(int) int { return 40 }); cut {
		out.Events, out.More, cursor = out.Events[:keep], true, out.Events[keep-1].ID
	}
	out.Cursor = strconv.FormatInt(cursor, 10)
	if hasSession {
		_ = s.events.SetLastChangeSeen(ctx, sess.ID, cursor)
	}
	if out.Rescan {
		out.Note = "the daemon lost track of some changes in this range; re-list the directories you rely on"
	}
	msg := fmt.Sprintf("%d events since cursor %d", len(out.Events), after)
	if out.More {
		msg += fmt.Sprintf(" (more; continue at %s)", out.Cursor)
	}
	if out.Rescan {
		msg += ". " + out.Note
	}
	return text("%s", msg), out, nil
}

// pullCursor resolves where a pull starts: the cursor given, else the
// session's stored one, else the row before the session started (a
// caller without a session starts from now).
func (s *Server) pullCursor(ctx context.Context, cursor string, sess agent.Session, hasSession bool) (int64, error) {
	if cursor != "" {
		id, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || id < 0 {
			return 0, fmt.Errorf("invalid cursor %q; pass the cursor a previous pull_events returned", cursor)
		}
		return id, nil
	}
	if hasSession {
		if id, err := s.events.LastChangeSeen(ctx, sess.ID); err == nil && id > 0 {
			return id, nil
		}
		return s.events.ChangeIDBefore(ctx, sess.StartedAt)
	}
	return s.events.LastChangeID(ctx)
}
