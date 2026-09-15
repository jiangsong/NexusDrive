package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TokenSpec is what a new access token grants. Read and Write are mount
// path prefixes; a nil Write means "the same as Read", an empty non-nil
// Write means no writes at all.
type TokenSpec struct {
	Name     string
	Read     []string
	Write    []string // nil = same as Read
	ReadOnly bool
	TTL      time.Duration // 0 = never expires
}

// The refusals token handling can hand back. VerifyToken wraps the three
// lookup outcomes so an HTTP layer can map them all to 401 while a CLI can
// still tell them apart.
var (
	ErrTokenUnknown = errors.New("agent: unknown token")
	ErrTokenExpired = errors.New("agent: token expired")
	ErrTokenRevoked = errors.New("agent: token revoked")
	ErrTokenName    = errors.New("agent: token name must match ^[a-z0-9][a-z0-9-]{0,63}$ and be unique among live tokens")
	ErrTokenScope   = errors.New("agent: every write prefix must be inside a read prefix")
)

// TokenPlainPrefix starts every plain token, so a token can be recognised in
// a log or a clipboard and so a fingerprint (the four characters after it)
// never overlaps the prefix.
const TokenPlainPrefix = "cfs_"

// tokenSecretBytes is how much randomness a token carries: 256 bits, which
// makes guessing hopeless even though only the hash is stored.
const tokenSecretBytes = 32

// lastUsedGranularity bounds how often a busy token rewrites last_used_at.
const lastUsedGranularity = time.Minute

var tokenNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// hashToken is the only transformation applied to a plain token before it
// touches the database: the plain text is returned to the creator once and
// never stored.
func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// fingerprintOf is the four characters that identify a token in listings
// and logs without revealing it.
func fingerprintOf(plain string) string {
	body := strings.TrimPrefix(plain, TokenPlainPrefix)
	if len(body) < 4 {
		return body
	}
	return body[:4]
}

// tokenScope validates a spec's prefixes and turns them into the scope the
// token's sessions will run under.
func tokenScope(spec TokenSpec, expires time.Time) (Scope, error) {
	sc := Scope{Read: clean(spec.Read), Write: clean(spec.Write), ReadOnly: spec.ReadOnly, ExpiresAt: expires}
	if sc.Read == nil {
		sc.Read = []string{}
	}
	if sc.Write != nil {
		if !allUnder(sc.Write, sc.EffectiveRead()) {
			return Scope{}, ErrTokenScope
		}
	}
	return sc, nil
}

// CreateToken issues a new access token. The plain text is returned exactly
// once; the database keeps only its SHA-256 and a four-character
// fingerprint. A name that is malformed or already carried by a live token
// is refused with ErrTokenName; a write prefix outside every read prefix
// with ErrTokenScope.
func (s *Store) CreateToken(ctx context.Context, spec TokenSpec) (string, Principal, error) {
	if s.readOnly {
		return "", Principal{}, errors.New("agent: the store is read-only")
	}
	if !tokenNameRE.MatchString(spec.Name) {
		return "", Principal{}, ErrTokenName
	}
	if spec.TTL < 0 {
		return "", Principal{}, errors.New("agent: token ttl must not be negative")
	}
	now := s.now()
	var expires time.Time
	if spec.TTL > 0 {
		expires = now.Add(spec.TTL)
	}
	sc, err := tokenScope(spec, expires)
	if err != nil {
		return "", Principal{}, err
	}
	scope, err := json.Marshal(sc)
	if err != nil {
		return "", Principal{}, fmt.Errorf("agent: %w", err)
	}
	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", Principal{}, fmt.Errorf("agent: %w", err)
	}
	plain := TokenPlainPrefix + base64.RawURLEncoding.EncodeToString(secret)
	id := uuid.NewString()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", Principal{}, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM principals WHERE kind = 'token' AND name = ? AND revoked_at = 0`, spec.Name).Scan(&n); err != nil {
		return "", Principal{}, fmt.Errorf("agent: %w", err)
	}
	if n > 0 {
		return "", Principal{}, ErrTokenName
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO principals(id, kind, name, token_hash, token_prefix, scope, created_at, expires_at)
		VALUES (?, 'token', ?, ?, ?, ?, ?, ?)`,
		id, spec.Name, hashToken(plain), fingerprintOf(plain), string(scope), now.UnixNano(), toNanos(expires)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", Principal{}, ErrTokenName
		}
		return "", Principal{}, fmt.Errorf("agent: %w", err)
	}
	p, err := scanPrincipal(tx.QueryRowContext(ctx, principalColumns+` WHERE id = ?`, id))
	if err != nil {
		return "", Principal{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", Principal{}, fmt.Errorf("agent: %w", err)
	}
	return plain, p, nil
}

// Tokens lists every issued token, live or not, newest first. Only the
// fingerprint of each is included; the hash never leaves the database.
func (s *Store) Tokens(ctx context.Context) ([]Principal, error) {
	rows, err := s.db.QueryContext(ctx, principalColumns+` WHERE kind = 'token' ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []Principal{}
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// Token returns one issued token by id or, failing that, by live name.
func (s *Store) Token(ctx context.Context, idOrName string) (Principal, error) {
	p, err := scanPrincipal(s.db.QueryRowContext(ctx, principalColumns+` WHERE kind = 'token' AND id = ?`, idOrName))
	if errors.Is(err, ErrPrincipalNotFound) {
		p, err = scanPrincipal(s.db.QueryRowContext(ctx, principalColumns+` WHERE kind = 'token' AND name = ? AND revoked_at = 0`, idOrName))
	}
	return p, err
}

// RevokeToken ends a token for good: VerifyToken refuses it from now on and
// its active sessions are marked expired, so the console stops listing them
// while the MCP server drops the connections that carried them. Revoking a
// token that is already revoked changes nothing and returns it as it is.
func (s *Store) RevokeToken(ctx context.Context, idOrName string) (Principal, error) {
	if s.readOnly {
		return Principal{}, errors.New("agent: the store is read-only")
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	defer tx.Rollback()
	p, err := scanPrincipal(tx.QueryRowContext(ctx, principalColumns+` WHERE kind = 'token' AND id = ?`, idOrName))
	if errors.Is(err, ErrPrincipalNotFound) {
		p, err = scanPrincipal(tx.QueryRowContext(ctx, principalColumns+` WHERE kind = 'token' AND name = ? AND revoked_at = 0`, idOrName))
	}
	if err != nil {
		return Principal{}, err
	}
	if !p.RevokedAt.IsZero() {
		return p, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principals SET revoked_at = ? WHERE id = ?`, now.UnixNano(), p.ID); err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	p.RevokedAt = now
	rows, err := tx.QueryContext(ctx, sessionColumns+` WHERE principal_id = ? AND state = 'active'`, p.ID)
	if err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	var ended []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return Principal{}, err
		}
		sess.State, sess.FinishedAt = "expired", now
		ended = append(ended, sess)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	if len(ended) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = 'expired', finished_at = ? WHERE principal_id = ? AND state = 'active'`, now.UnixNano(), p.ID); err != nil {
			return Principal{}, fmt.Errorf("agent: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Principal{}, fmt.Errorf("agent: %w", err)
	}
	for i := range ended {
		s.publish(Event{Kind: "session", Session: &ended[i]})
	}
	return p, nil
}

// VerifyToken resolves a plain token to its principal. The lookup is by
// hash, so the plain text is never compared or logged. A revoked token is
// reported as such even when it has also expired, because revocation is the
// deliberate act. A successful lookup bumps last_used_at at most once a
// minute.
func (s *Store) VerifyToken(ctx context.Context, plain string) (Principal, error) {
	if !strings.HasPrefix(plain, TokenPlainPrefix) {
		return Principal{}, ErrTokenUnknown
	}
	p, err := scanPrincipal(s.db.QueryRowContext(ctx, principalColumns+` WHERE kind = 'token' AND token_hash = ?`, hashToken(plain)))
	if errors.Is(err, ErrPrincipalNotFound) {
		return Principal{}, ErrTokenUnknown
	}
	if err != nil {
		return Principal{}, err
	}
	now := s.now()
	switch TokenState(p, now) {
	case "revoked":
		return Principal{}, ErrTokenRevoked
	case "expired":
		return Principal{}, ErrTokenExpired
	}
	if !s.readOnly && now.Sub(p.LastUsedAt) >= lastUsedGranularity {
		if _, err := s.db.ExecContext(ctx, `UPDATE principals SET last_used_at = ? WHERE id = ? AND last_used_at < ?`,
			now.UnixNano(), p.ID, now.Add(-lastUsedGranularity).UnixNano()); err != nil {
			return Principal{}, fmt.Errorf("agent: %w", err)
		}
		p.LastUsedAt = now
	}
	return p, nil
}

// HasLiveTokens reports whether any issued token is neither revoked nor
// expired. A loopback listener stays open to unauthenticated local callers
// only until the first token exists.
func (s *Store) HasLiveTokens(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM principals WHERE kind = 'token' AND revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)`,
		s.now().UnixNano()).Scan(&n); err != nil {
		return false, fmt.Errorf("agent: %w", err)
	}
	return n > 0, nil
}

// RevokedSince lists the ids of principals revoked at or after since. The
// MCP server polls it to drop the connections of a token revoked by another
// process, so the bound is inclusive: seeing a revocation twice is harmless,
// missing one is not.
func (s *Store) RevokedSince(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM principals WHERE revoked_at != 0 AND revoked_at >= ? ORDER BY revoked_at, id`, since.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	return out, nil
}

// TokenState is the one-word status a listing shows for a token at now:
// revoked wins over expired, and a token that is neither is active.
func TokenState(p Principal, now time.Time) string {
	switch {
	case !p.RevokedAt.IsZero():
		return "revoked"
	case !p.ExpiresAt.IsZero() && !now.Before(p.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}

// toNanos is the inverse of fromNanos: a zero time is stored as 0.
func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
