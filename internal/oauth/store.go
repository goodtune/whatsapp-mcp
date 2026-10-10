package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// RefreshTokenTTL is how long a refresh token stays valid. Each use rotates
// it, so an MCP client in regular use stays signed in indefinitely.
const RefreshTokenTTL = 30 * 24 * time.Hour

var errNotFound = errors.New("not found")

type client struct {
	ID           string
	Name         string
	RedirectURIs []string
}

func (s *Server) registerClient(ctx context.Context, name string, redirectURIs []string) (*client, error) {
	c := &client{ID: randomToken(), Name: name, RedirectURIs: redirectURIs}
	uris, err := json.Marshal(redirectURIs)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris) VALUES ($1, $2, $3)`,
		c.ID, nullableString(name), string(uris),
	); err != nil {
		return nil, fmt.Errorf("failed to register client: %w", err)
	}
	return c, nil
}

func (s *Server) getClient(ctx context.Context, id string) (*client, error) {
	var name sql.NullString
	var uris string
	err := s.db.QueryRowContext(ctx,
		`SELECT client_name, redirect_uris FROM oauth_clients WHERE client_id = $1`, id,
	).Scan(&name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, fmt.Errorf("failed to look up client: %w", err)
	}
	c := &client{ID: id, Name: name.String}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("corrupt redirect_uris for client %s: %w", id, err)
	}
	return c, nil
}

// issueRefreshToken stores a new refresh token (by hash) and returns it.
func (s *Server) issueRefreshToken(ctx context.Context, clientID string, userID int64) (string, error) {
	token := randomToken()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_refresh_tokens (token_hash, client_id, user_id, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), clientID, userID, time.Now().Add(RefreshTokenTTL).UTC(),
	); err != nil {
		return "", fmt.Errorf("failed to store refresh token: %w", err)
	}
	return token, nil
}

// consumeRefreshToken deletes a refresh token and returns its user, so each
// refresh token can be used exactly once.
func (s *Server) consumeRefreshToken(ctx context.Context, token, clientID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	hash := hashToken(token)
	var storedClientID string
	var userID int64
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT client_id, user_id, expires_at FROM oauth_refresh_tokens WHERE token_hash = $1`, hash,
	).Scan(&storedClientID, &userID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errNotFound
	} else if err != nil {
		return 0, fmt.Errorf("failed to look up refresh token: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM oauth_refresh_tokens WHERE token_hash = $1`, hash)
	if err != nil {
		return 0, fmt.Errorf("failed to revoke refresh token: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errNotFound // used concurrently
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	if storedClientID != clientID || time.Now().After(expiresAt) {
		return 0, errNotFound
	}
	return userID, nil
}

// pendingStore holds short-lived, single-use entries in memory: in-flight
// authorization requests, consent prompts and authorization codes. They
// only need to live for the few minutes a sign-in takes, so losing them on
// restart just means starting the sign-in again.
type pendingStore[T any] struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]pendingEntry[T]
}

type pendingEntry[T any] struct {
	value   T
	expires time.Time
}

func newPendingStore[T any](ttl time.Duration) *pendingStore[T] {
	return &pendingStore[T]{ttl: ttl, entries: make(map[string]pendingEntry[T])}
}

// put stores v under a new random key and returns the key.
func (p *pendingStore[T]) put(v T) string {
	key := randomToken()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, e := range p.entries {
		if now.After(e.expires) {
			delete(p.entries, k)
		}
	}
	p.entries[key] = pendingEntry[T]{value: v, expires: now.Add(p.ttl)}
	return key
}

// take removes and returns the entry for key, if present and unexpired.
func (p *pendingStore[T]) take(key string) (T, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	delete(p.entries, key)
	if !ok || time.Now().After(e.expires) {
		var zero T
		return zero, false
	}
	return e.value, true
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func nullableString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
