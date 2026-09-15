package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func openTokenStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateTokenStoresOnlyTheHash(t *testing.T) {
	s := openTokenStore(t)
	plain, p, err := s.CreateToken(context.Background(), TokenSpec{Name: "codex", Read: []string{"/work"}, Write: []string{"/work/.agent"}, TTL: 720 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPlainPrefix) || len(plain) < 40 {
		t.Fatalf("plain %q", plain)
	}
	if p.TokenPrefix != plain[4:8] {
		t.Fatalf("fingerprint %q", p.TokenPrefix)
	}
	if p.Kind != "token" || p.Name != "codex" || p.ExpiresAt.IsZero() || !p.RevokedAt.IsZero() {
		t.Fatalf("principal %+v", p)
	}
	if r := p.Scope.EffectiveRead(); len(r) != 1 || r[0] != "/work" {
		t.Fatalf("read scope %v", r)
	}
	if w := p.Scope.EffectiveWrite(); len(w) != 1 || w[0] != "/work/.agent" {
		t.Fatalf("write scope %v", w)
	}
	if !p.Scope.ExpiresAt.Equal(p.ExpiresAt) {
		t.Fatalf("scope expiry %v differs from the principal's %v", p.Scope.ExpiresAt, p.ExpiresAt)
	}
	rows, err := s.db.Query(`SELECT id, kind, name, token_hash, token_prefix, scope FROM principals`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cols [6]string
		if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5]); err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			if strings.Contains(c, plain[4:]) {
				t.Fatalf("plain token material stored: %q", c)
			}
		}
	}
	sum := sha256.Sum256([]byte(plain))
	var hash string
	if err := s.db.QueryRow(`SELECT token_hash FROM principals WHERE id=?`, p.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("token_hash is not sha256(plain)")
	}
}

func TestVerifyTokenRejectsExpiredAndRevoked(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	short, _, err := s.CreateToken(ctx, TokenSpec{Name: "short", TTL: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	live, p, err := s.CreateToken(ctx, TokenSpec{Name: "live"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := s.VerifyToken(ctx, short); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	got, err := s.VerifyToken(ctx, live)
	if err != nil || got.ID != p.ID {
		t.Fatalf("live: %v", err)
	}
	if got.LastUsedAt.IsZero() {
		t.Fatal("a verified token must record last_used_at")
	}
	if _, err := s.RevokeToken(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyToken(ctx, live); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	if _, err := s.VerifyToken(ctx, "cfs_nope"); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.VerifyToken(ctx, ""); !errors.Is(err, ErrTokenUnknown) {
		t.Fatalf("empty: %v", err)
	}
}

func TestVerifyTokenTouchesLastUsedAtMostOnceAMinute(t *testing.T) {
	s := openTokenStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	plain, _, err := s.CreateToken(ctx, TokenSpec{Name: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.VerifyToken(ctx, plain)
	if err != nil || !first.LastUsedAt.Equal(now) {
		t.Fatalf("first use: %v %v", first.LastUsedAt, err)
	}
	now = now.Add(30 * time.Second)
	again, err := s.VerifyToken(ctx, plain)
	if err != nil || !again.LastUsedAt.Equal(first.LastUsedAt) {
		t.Fatalf("a use within the minute rewrote last_used_at: %v %v", again.LastUsedAt, err)
	}
	now = now.Add(31 * time.Second)
	later, err := s.VerifyToken(ctx, plain)
	if err != nil || !later.LastUsedAt.Equal(now) {
		t.Fatalf("a use after the minute did not touch last_used_at: %v %v", later.LastUsedAt, err)
	}
}

func TestTokenWriteMustBeInsideRead(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "bad", Read: []string{"/work"}, Write: []string{"/gd"}}); !errors.Is(err, ErrTokenScope) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "sibling", Read: []string{"/work"}, Write: []string{"/workshop"}}); !errors.Is(err, ErrTokenScope) {
		t.Fatalf("prefix lookalike: %v", err)
	}
	_, p, err := s.CreateToken(ctx, TokenSpec{Name: "none", Read: []string{"/work"}, Write: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if w := p.Scope.EffectiveWrite(); len(w) != 0 {
		t.Fatalf("an empty write list must mean no writes, got %v", w)
	}
	_, p, err = s.CreateToken(ctx, TokenSpec{Name: "whole", Write: []string{"/anything"}})
	if err != nil {
		t.Fatalf("whole-mount read covers any write prefix: %v", err)
	}
	if r := p.Scope.EffectiveRead(); len(r) != 1 || r[0] != "/" {
		t.Fatalf("read %v", r)
	}
}

func TestLiveTokenNamesAreUnique(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "codex"}); !errors.Is(err, ErrTokenName) {
		t.Fatalf("dup: %v", err)
	}
	if _, err := s.RevokeToken(ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "codex"}); err != nil {
		t.Fatalf("reuse after revoke: %v", err)
	}
	for _, bad := range []string{"Bad Name", "", "-lead", "UPPER", strings.Repeat("a", 65)} {
		if _, _, err := s.CreateToken(ctx, TokenSpec{Name: bad}); !errors.Is(err, ErrTokenName) {
			t.Fatalf("name %q: %v", bad, err)
		}
	}
	tokens, err := s.Tokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("listing must include the revoked token: %d", len(tokens))
	}
	states := map[string]int{}
	for _, p := range tokens {
		if p.TokenPrefix == "" || p.Name != "codex" {
			t.Fatalf("%+v", p)
		}
		states[TokenState(p, time.Now())]++
	}
	if states["active"] != 1 || states["revoked"] != 1 {
		t.Fatalf("states %v", states)
	}
}

func TestRevokedSinceListsRevokedPrincipals(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	_, p, err := s.CreateToken(ctx, TokenSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, err := s.RevokeToken(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := s.RevokedSince(ctx, before.Add(-time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != p.ID {
		t.Fatalf("%v", ids)
	}
	if ids, _ := s.RevokedSince(ctx, time.Now().Add(time.Hour)); len(ids) != 0 {
		t.Fatalf("future bound returned %v", ids)
	}
	if _, err := s.RevokeToken(ctx, "nobody"); !errors.Is(err, ErrPrincipalNotFound) {
		t.Fatalf("unknown revoke: %v", err)
	}
	first, err := s.Token(ctx, p.ID)
	if err != nil || first.RevokedAt.IsZero() {
		t.Fatalf("lookup after revoke: %+v %v", first, err)
	}
	again, err := s.RevokeToken(ctx, p.ID)
	if err != nil || !again.RevokedAt.Equal(first.RevokedAt) {
		t.Fatalf("revoking twice must keep the first instant: %+v %v", again, err)
	}
}

func TestRevokeTokenExpiresItsActiveSessions(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	_, p, err := s.CreateToken(ctx, TokenSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	m := NewSessions(s, SessionOptions{})
	sess, err := m.Resolve(ctx, ConnInfo{Key: "token:" + p.ID, Transport: "http-token", PrincipalID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	events, stop := s.Watch()
	defer stop()
	if _, err := s.RevokeToken(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "expired" || got.FinishedAt.IsZero() {
		t.Fatalf("session after revoke: %+v", got)
	}
	select {
	case e := <-events:
		if e.Kind != "session" || e.Session == nil || e.Session.ID != sess.ID || e.Session.State != "expired" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no session event was published")
	}
}

func TestHasLiveTokensIgnoresExpiredAndRevoked(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()
	if live, err := s.HasLiveTokens(ctx); err != nil || live {
		t.Fatalf("empty store: live=%v err=%v", live, err)
	}
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "short", TTL: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if live, _ := s.HasLiveTokens(ctx); live {
		t.Fatal("an expired token counted as live")
	}
	if _, _, err := s.CreateToken(ctx, TokenSpec{Name: "gone"}); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.HasLiveTokens(ctx); !live {
		t.Fatal("a fresh token must count as live")
	}
	if _, err := s.RevokeToken(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.HasLiveTokens(ctx); live {
		t.Fatal("a revoked token counted as live")
	}
}

func TestTokenStateOrdersRevokedOverExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	for _, tc := range []struct {
		p    Principal
		want string
	}{
		{Principal{}, "active"},
		{Principal{ExpiresAt: future}, "active"},
		{Principal{ExpiresAt: now}, "expired"},
		{Principal{ExpiresAt: past}, "expired"},
		{Principal{ExpiresAt: past, RevokedAt: past}, "revoked"},
		{Principal{RevokedAt: now}, "revoked"},
	} {
		if got := TokenState(tc.p, now); got != tc.want {
			t.Errorf("%+v: got %s want %s", tc.p, got, tc.want)
		}
	}
}
