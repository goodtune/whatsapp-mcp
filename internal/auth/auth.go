// Package auth implements user accounts (one per GitHub identity) and the
// short-lived JWT access tokens the OAuth server issues for the MCP
// endpoint. The MCP endpoint verifies them via a
// github.com/modelcontextprotocol/go-sdk/auth TokenVerifier (wired up in
// cmd/whatsapp-mcp), rather than any bespoke middleware here.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL is how long an access token is valid. MCP clients use
// their refresh token to get a new one.
const AccessTokenTTL = time.Hour

type User struct {
	ID       int64
	Username string
}

type Service struct {
	db     *sql.DB
	secret []byte
}

func NewService(db *sql.DB, secret string) *Service {
	return &Service{db: db, secret: []byte(secret)}
}

// UpsertGitHubUser returns the account for a GitHub user, creating it on
// first sign-in and keeping its username in step with their GitHub login.
//
// An account created before OAuth (with a username but no GitHub id) is
// claimed by the GitHub user with the same login, so its linked WhatsApp
// device and history carry over.
func (s *Service) UpsertGitHubUser(ctx context.Context, githubID int64, login string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	user := User{Username: login}
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE github_id = $1`, githubID).Scan(&user.ID)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE users SET username = $1 WHERE id = $2`, login, user.ID); err != nil {
			return nil, fmt.Errorf("failed to update username: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx,
			`UPDATE users SET github_id = $1 WHERE username = $2 AND github_id IS NULL RETURNING id`,
			githubID, login,
		).Scan(&user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx,
				`INSERT INTO users (username, github_id) VALUES ($1, $2) RETURNING id`,
				login, githubID,
			).Scan(&user.ID)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to create user: %w", err)
		}
	default:
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit user: %w", err)
	}
	return &user, nil
}

func (s *Service) GetUser(ctx context.Context, id int64) (*User, error) {
	var user User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username FROM users WHERE id = $1`, id,
	).Scan(&user.ID, &user.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user %d not found", id)
	} else if err != nil {
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}
	return &user, nil
}

type claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// IssueAccessToken creates a signed JWT identifying the given user, valid
// only for the given audience (the MCP endpoint's resource URL).
func (s *Service) IssueAccessToken(user *User, audience string) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(AccessTokenTTL)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Username: user.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", user.ID),
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	})
	signed, err := token.SignedString(s.secret)
	return signed, expiresAt, err
}

// VerifyToken parses and validates a JWT issued for the given audience,
// returning the authenticated user id and the token's expiration time.
func (s *Service) VerifyToken(tokenString, audience string) (userID int64, expiresAt time.Time, err error) {
	var c claims
	token, err := jwt.ParseWithClaims(tokenString, &c, func(t *jwt.Token) (interface{}, error) {
		return s.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return 0, time.Time{}, fmt.Errorf("invalid token: %w", err)
	}

	if _, err := fmt.Sscanf(c.Subject, "%d", &userID); err != nil {
		return 0, time.Time{}, fmt.Errorf("invalid token subject: %w", err)
	}
	return userID, c.ExpiresAt.Time, nil
}
