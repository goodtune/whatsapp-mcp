// Package auth implements user accounts, password hashing, and the JWT
// session tokens issued at login. The JWTs double as MCP bearer tokens: the
// MCP endpoint verifies them via a github.com/modelcontextprotocol/go-sdk/auth
// TokenVerifier (wired up in cmd/whatsapp-mcp), rather than any bespoke
// middleware here.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidCredentials = errors.New("invalid username or password")
var ErrUsernameTaken = errors.New("username is already taken")

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,64}$`)

type User struct {
	ID       int64
	Username string
}

type Service struct {
	db     *sql.DB
	secret []byte
	ttl    time.Duration
}

func NewService(db *sql.DB, secret string) *Service {
	return &Service{db: db, secret: []byte(secret), ttl: 30 * 24 * time.Hour}
}

// Register creates a new user account with a bcrypt-hashed password.
func (s *Service) Register(ctx context.Context, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if !usernamePattern.MatchString(username) {
		return nil, fmt.Errorf("username must be 3-64 characters of letters, digits, '.', '_' or '-'")
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	var id int64
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`,
		username, string(hash),
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrUsernameTaken
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return &User{ID: id, Username: username}, nil
}

// Authenticate verifies a username/password pair and returns the user on success.
func (s *Service) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var user User
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM users WHERE username = $1`,
		strings.TrimSpace(username),
	).Scan(&user.ID, &user.Username, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidCredentials
	} else if err != nil {
		return nil, fmt.Errorf("failed to look up user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
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

// IssueToken creates a signed JWT identifying the given user.
func (s *Service) IssueToken(user *User) (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Username: user.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", user.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		},
	})
	return token.SignedString(s.secret)
}

// VerifyToken parses and validates a JWT, returning the authenticated user
// id and the token's expiration time.
func (s *Service) VerifyToken(tokenString string) (userID int64, expiresAt time.Time, err error) {
	var c claims
	token, err := jwt.ParseWithClaims(tokenString, &c, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil || !token.Valid {
		return 0, time.Time{}, fmt.Errorf("invalid token: %w", err)
	}

	if _, err := fmt.Sscanf(c.Subject, "%d", &userID); err != nil {
		return 0, time.Time{}, fmt.Errorf("invalid token subject: %w", err)
	}
	if c.ExpiresAt != nil {
		expiresAt = c.ExpiresAt.Time
	}
	return userID, expiresAt, nil
}

func isUniqueViolation(err error) bool {
	// Postgres: "duplicate key value violates unique constraint";
	// SQLite: "UNIQUE constraint failed".
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key")
}
