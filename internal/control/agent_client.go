package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cloudfs/internal/agent"
)

// CallAudit asks the running daemon for a page of audit rows. online is
// false when no daemon answers, in which case the CLI reads agent.db itself.
func CallAudit(ctx context.Context, socket, tcp string, q agent.AuditQuery) (AuditResponse, bool, error) {
	params := url.Values{}
	setIf(params, "cursor", q.Cursor)
	setIf(params, "session", q.Session)
	setIf(params, "tool", q.Tool)
	setIf(params, "result", q.Result)
	if !q.Since.IsZero() {
		params.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var out AuditResponse
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/audit?"+params.Encode(), nil, &out)
	return out, online, err
}

// CallSessions asks the running daemon for a page of sessions.
func CallSessions(ctx context.Context, socket, tcp string, q agent.ListQuery) (SessionsResponse, bool, error) {
	params := url.Values{}
	setIf(params, "cursor", q.Cursor)
	setIf(params, "state", q.State)
	setIf(params, "path", q.Path)
	if q.Sandbox {
		params.Set("sandbox", "1")
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var out SessionsResponse
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/sessions?"+params.Encode(), nil, &out)
	return out, online, err
}

// CallSession asks the running daemon for one session with its audit tail.
func CallSession(ctx context.Context, socket, tcp, id string) (SessionDetail, bool, error) {
	var out SessionDetail
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/sessions/"+url.PathEscape(id), nil, &out)
	return out, online, err
}

// CallFinishSession ends a session through the running daemon. It never
// replays: a lost response leaves the session as the daemon left it.
func CallFinishSession(ctx context.Context, socket, tcp, id, summary string) (SessionView, bool, error) {
	body, err := json.Marshal(SessionFinishRequest{Summary: summary})
	if err != nil {
		return SessionView{}, false, err
	}
	var out SessionView
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/finish", body, &out)
	return out, online, err
}

func setIf(params url.Values, key, value string) {
	if value != "" {
		params.Set(key, value)
	}
}

// CallRollbackSession previews (dryRun) or executes a session rollback
// through the running daemon. The route itself refuses an execution that
// does not carry confirm, so the CLI's --confirm is what sets it.
func CallRollbackSession(ctx context.Context, socket, tcp, id string, dryRun bool) (agent.Plan, bool, error) {
	body, err := json.Marshal(SessionRollbackRequest{DryRun: dryRun, Confirm: !dryRun})
	if err != nil {
		return agent.Plan{}, false, err
	}
	var out agent.Plan
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/rollback", body, &out)
	return out, online, err
}
