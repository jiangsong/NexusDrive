package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func newSessions(t *testing.T, now *time.Time) *Sessions {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewSessions(st, SessionOptions{Idle: 30 * time.Minute, Now: func() time.Time { return *now }})
}

func TestResolveReusesTheActiveSessionForAConnection(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, err := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	c := ConnInfo{Key: "stdio:0x1", Transport: "stdio", PrincipalID: p.ID, ClientName: "claude-code"}
	a, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.State != "active" || a.ClientName != "claude-code" {
		t.Fatalf("%+v %+v", a, b)
	}
	if a.PrincipalID != p.ID || a.Transport != "stdio" || a.StartedAt != now {
		t.Fatalf("session did not copy its connection: %+v", a)
	}
	if got := a.Scope.EffectiveRead(); len(got) != 1 || got[0] != "/work" {
		t.Fatalf("scope %+v", a.Scope)
	}
}

func TestEnsurePrincipalIsIdempotentAndUpdatesTheScope(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	a, err := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{Read: []string{"/one"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{Read: []string{"/two"}, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("a second EnsurePrincipal made a new row: %q %q", a.ID, b.ID)
	}
	got, err := m.Principal(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r := got.Scope.EffectiveRead(); len(r) != 1 || r[0] != "/two" || !got.Scope.ReadOnly {
		t.Fatalf("scope was not updated: %+v", got.Scope)
	}
	if _, err := m.Principal(context.Background(), "nope"); !errors.Is(err, ErrPrincipalNotFound) {
		t.Fatalf("missing principal: %v", err)
	}
}

func TestTokenSessionRotatesAfterIdle(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, err := m.EnsurePrincipal(context.Background(), "env", "env", Scope{})
	if err != nil {
		t.Fatal(err)
	}
	c := ConnInfo{Key: "token:" + p.ID, Transport: "http-token", PrincipalID: p.ID}
	a, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Minute)
	same, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if same.ID != a.ID || !same.LastSeenAt.Equal(now) {
		t.Fatalf("a busy token session rotated or did not record last_seen: %+v", same)
	}
	now = now.Add(31 * time.Minute)
	b, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("an idle token session must rotate")
	}
	old, err := m.Get(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != "expired" || old.FinishedAt.IsZero() {
		t.Fatalf("old state %q finished %v", old.State, old.FinishedAt)
	}
}

func TestStdioSessionDoesNotRotateOnIdle(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	c := ConnInfo{Key: "stdio:0x9", Transport: "stdio", PrincipalID: p.ID}
	a, _ := m.Resolve(context.Background(), c)
	now = now.Add(3 * time.Hour)
	b, _ := m.Resolve(context.Background(), c)
	if a.ID != b.ID {
		t.Fatal("a stdio session lives as long as its process, whatever the idle gap")
	}
}

func TestFinishIsTerminalAndTheNextCallStartsANewSession(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	c := ConnInfo{Key: "stdio:0x2", Transport: "stdio", PrincipalID: p.ID}
	a, _ := m.Resolve(context.Background(), c)
	done, err := m.Finish(context.Background(), a.ID, "done")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "finished" || done.Summary != "done" || done.FinishedAt.IsZero() {
		t.Fatalf("finished row: %+v", done)
	}
	b, _ := m.Resolve(context.Background(), c)
	if b.ID == a.ID {
		t.Fatal("a finished session was reused")
	}
	if _, err := m.Finish(context.Background(), "missing", ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("finishing an unknown session: %v", err)
	}
	if _, err := m.Get(context.Background(), "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("getting an unknown session: %v", err)
	}
}

func TestListSessionsPagesByCursor(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		if _, err := m.Resolve(context.Background(), ConnInfo{Key: fmt.Sprintf("stdio:%d", i), Transport: "stdio", PrincipalID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	page1, cur, err := m.List(context.Background(), ListQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	page2, cur2, err := m.List(context.Background(), ListQuery{Limit: 2, Cursor: cur})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || len(page2) != 2 || cur == "" || page1[0].ID == page2[0].ID {
		t.Fatalf("%v %v %q", page1, page2, cur)
	}
	if !page1[0].StartedAt.After(page1[1].StartedAt) || !page1[1].StartedAt.After(page2[0].StartedAt) {
		t.Fatal("sessions must list newest first")
	}
	page3, cur3, err := m.List(context.Background(), ListQuery{Limit: 2, Cursor: cur2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 1 || cur3 != "" {
		t.Fatalf("last page: %d rows, cursor %q", len(page3), cur3)
	}
	if _, _, err := m.List(context.Background(), ListQuery{Cursor: "not a cursor"}); err == nil {
		t.Fatal("a malformed cursor was accepted")
	}
}

func TestListSessionsFiltersByStateSandboxAndPath(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	a, _ := m.Resolve(context.Background(), ConnInfo{Key: "stdio:a", Transport: "stdio", PrincipalID: p.ID})
	now = now.Add(time.Second)
	b, _ := m.Resolve(context.Background(), ConnInfo{Key: "stdio:b", Transport: "stdio", PrincipalID: p.ID})
	if _, err := m.Finish(context.Background(), a.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.db.Exec(`UPDATE sessions SET workspace = '/work/ws-b', sandbox = 1 WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	active, _, _ := m.List(context.Background(), ListQuery{State: "active"})
	finished, _, _ := m.List(context.Background(), ListQuery{State: "finished"})
	if len(active) != 1 || active[0].ID != b.ID || len(finished) != 1 || finished[0].ID != a.ID {
		t.Fatalf("state filter: active=%v finished=%v", active, finished)
	}
	sandboxed, _, _ := m.List(context.Background(), ListQuery{Sandbox: true})
	if len(sandboxed) != 1 || sandboxed[0].ID != b.ID || !sandboxed[0].Sandbox {
		t.Fatalf("sandbox filter: %v", sandboxed)
	}
	inside, _, _ := m.List(context.Background(), ListQuery{Path: "/work/ws-b/out.txt"})
	outside, _, _ := m.List(context.Background(), ListQuery{Path: "/work/ws-bb"})
	if len(inside) != 1 || inside[0].ID != b.ID || len(outside) != 0 {
		t.Fatalf("path filter: inside=%v outside=%v", inside, outside)
	}
}

func TestSummaryCountsActiveWritesAndDenials(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	if _, err := m.Resolve(context.Background(), ConnInfo{Key: "stdio:a", Transport: "stdio", PrincipalID: p.ID}); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-time.Hour)
	rows := []struct {
		ts     time.Time
		tool   string
		result string
	}{
		{now, "write_file", "ok"},
		{now, "delete", "ok"},
		{now, "read_text", "ok"},
		{now, "write_file", "error"},
		{now, "read_text", "denied"},
		{since.Add(-time.Minute), "write_file", "ok"},
		{since.Add(-time.Minute), "read_text", "denied"},
	}
	for _, r := range rows {
		if _, err := m.store.db.Exec(`INSERT INTO audit(ts, tool, paths, args, result) VALUES (?, ?, '[]', '{}', ?)`, r.ts.UnixNano(), r.tool, r.result); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.Summary(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if got.Active != 1 || got.WritesToday != 2 || got.DeniedToday != 1 {
		t.Fatalf("summary %+v", got)
	}
}

func TestResolvePublishesASessionEvent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	events, stop := m.Store().Watch()
	defer stop()
	s, err := m.Resolve(context.Background(), ConnInfo{Key: "stdio:a", Transport: "stdio", PrincipalID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-events:
		if e.Kind != "session" || e.Session == nil || e.Session.ID != s.ID {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no session event")
	}
}

func TestSessionContextRoundTrip(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("an empty context holds no session")
	}
	ctx := WithSession(context.Background(), Session{ID: "s1"})
	s, ok := FromContext(ctx)
	if !ok || s.ID != "s1" {
		t.Fatalf("%v %+v", ok, s)
	}
}
