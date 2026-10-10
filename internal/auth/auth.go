// Package auth implements user accounts, password hashing, JWT session
// tokens, and the HTTP middleware that authenticates both the REST endpoints
// and the MCP endpoint.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
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

// VerifyToken parses and validates a JWT, returning the authenticated user id.
func (s *Service) VerifyToken(tokenString string) (int64, error) {
	var c claims
	token, err := jwt.ParseWithClaims(tokenString, &c, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil || !token.Valid {
		return 0, fmt.Errorf("invalid token: %w", err)
	}

	var userID int64
	if _, err := fmt.Sscanf(c.Subject, "%d", &userID); err != nil {
		return 0, fmt.Errorf("invalid token subject: %w", err)
	}
	return userID, nil
}

type contextKey string

const userIDContextKey contextKey = "whatsapp-mcp.user_id"

func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDContextKey, userID)
}

// UserIDFromContext returns the authenticated user id injected by Middleware.
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userIDContextKey).(int64)
	return id, ok
}

// Middleware authenticates requests carrying "Authorization: Bearer <jwt>"
// and rejects everything else with 401. On success the user id is injected
// into the request context, where it is visible to downstream HTTP handlers
// and, for the MCP endpoint, to tool handlers (mcp-go threads the request
// context through to each tool call).
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		tokenString, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || tokenString == "" {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}

		userID, err := s.VerifyToken(strings.TrimSpace(tokenString))
		if err != nil {
			http.Error(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}

		r = r.WithContext(WithUserID(r.Context(), userID))
		next.ServeHTTP(w, r)
	})
}

func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate key")
}
