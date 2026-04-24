package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Token is the public, safe-to-display row for a bearer token. The raw
// token value is NEVER stored; only the sha256. CreateToken returns the
// raw value exactly once at issuance time.
type Token struct {
	ID         uuid.UUID
	Name       string
	HashPrefix string // first 8 chars of token_hash, for identification
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// ErrTokenNotFound covers both "no such id" and "multiple matching
// prefixes" — the CLI uses a single message for either so a fat-finger
// doesn't reveal which tokens exist.
var ErrTokenNotFound = errors.New("store: token not found")

// ErrAmbiguousPrefix is returned when a prefix matches more than one row;
// exposed because the CLI shows a different message for "be more specific."
var ErrAmbiguousPrefix = errors.New("store: ambiguous token prefix")

// tokenPrefix prefixes raw tokens so a leaked secret is easy to grep for
// and distinguish from unrelated UUIDs.
const tokenPrefix = "rag_"

// CreateToken generates a new opaque bearer token, hashes it, and stores
// the hash under the given name. The returned rawToken must be displayed
// once and not persisted server-side — the caller's secret store is the
// only place it lives after this function returns.
func (s *Store) CreateToken(ctx context.Context, name string) (rawToken string, t Token, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", Token{}, errors.New("store: token name is required")
	}
	rawToken = tokenPrefix + uuid.NewString()
	sum := sha256.Sum256([]byte(rawToken))
	hash := hex.EncodeToString(sum[:])

	err = s.pool.QueryRow(ctx, `
INSERT INTO tokens (name, token_hash)
VALUES ($1, $2)
RETURNING id, name, created_at, last_used_at, revoked_at`,
		name, hash,
	).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if err != nil {
		return "", Token{}, fmt.Errorf("store: create token: %w", err)
	}
	t.HashPrefix = hash[:8]
	return rawToken, t, nil
}

// ListTokens returns every token row (active + revoked) newest first.
// Operators typically want to see revoked rows too so they can confirm
// the revoke worked without re-issuing.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, name, substr(token_hash, 1, 8), created_at, last_used_at, revoked_at
FROM tokens
ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.HashPrefix, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks a token revoked by id or unambiguous id-prefix.
// Already-revoked tokens are idempotently re-stamped.
func (s *Store) RevokeToken(ctx context.Context, idOrPrefix string) (Token, error) {
	id, err := s.resolveTokenID(ctx, idOrPrefix)
	if err != nil {
		return Token{}, err
	}
	var t Token
	err = s.pool.QueryRow(ctx, `
UPDATE tokens SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1
RETURNING id, name, substr(token_hash, 1, 8), created_at, last_used_at, revoked_at`,
		id,
	).Scan(&t.ID, &t.Name, &t.HashPrefix, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrTokenNotFound
	}
	if err != nil {
		return Token{}, err
	}
	return t, nil
}

// LookupActiveToken returns the token whose sha256(raw) == hash, only if
// it isn't revoked. Missing or revoked → ErrTokenNotFound so callers
// can't distinguish the two.
func (s *Store) LookupActiveToken(ctx context.Context, rawToken string) (Token, error) {
	sum := sha256.Sum256([]byte(rawToken))
	hash := hex.EncodeToString(sum[:])
	var t Token
	err := s.pool.QueryRow(ctx, `
SELECT id, name, substr(token_hash, 1, 8), created_at, last_used_at, revoked_at
FROM tokens
WHERE token_hash = $1 AND revoked_at IS NULL`,
		hash,
	).Scan(&t.ID, &t.Name, &t.HashPrefix, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrTokenNotFound
	}
	if err != nil {
		return Token{}, err
	}
	return t, nil
}

// TouchLastUsed updates tokens.last_used_at to now(). Fire-and-forget on
// every authenticated MCP request so operators can see dormant tokens.
// Errors are returned so the caller can log, but callers should not
// fail a request over it.
func (s *Store) TouchLastUsed(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE tokens SET last_used_at = now() WHERE id = $1`, id)
	return err
}

// resolveTokenID accepts a full UUID or an unambiguous id-prefix.
func (s *Store) resolveTokenID(ctx context.Context, s1 string) (uuid.UUID, error) {
	s1 = strings.TrimSpace(s1)
	if s1 == "" {
		return uuid.Nil, ErrTokenNotFound
	}
	if id, err := uuid.Parse(s1); err == nil {
		return id, nil
	}
	// Treat as prefix match against the id::text column.
	rows, err := s.pool.Query(ctx, `SELECT id FROM tokens WHERE id::text LIKE $1`, s1+"%")
	if err != nil {
		return uuid.Nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return uuid.Nil, err
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return uuid.Nil, ErrTokenNotFound
	case 1:
		return ids[0], nil
	default:
		return uuid.Nil, ErrAmbiguousPrefix
	}
}
