// Package db manages the shared database connection (Postgres or SQLite)
// and schema migrations for the application's own tables (users,
// whatsapp_links, chats, messages).
//
// whatsmeow's own session tables (whatsmeow_device, whatsmeow_*) are managed
// separately by sqlstore.Container.Upgrade against the same connection.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationFiles embed.FS

// Dialect identifies which database engine a connection talks to.
type Dialect string

const (
	Postgres Dialect = "postgres"
	SQLite   Dialect = "sqlite"
)

// DriverName is the database/sql driver name registered for the dialect,
// which is also what whatsmeow's sqlstore expects as its dialect argument.
func (d Dialect) DriverName() string {
	if d == SQLite {
		return "sqlite"
	}
	return "pgx"
}

// Open connects to the database described by dsn. The returned *sql.DB is
// shared between the application's own repositories and whatsmeow's
// sqlstore.Container.
//
// A dsn starting with postgres:// or postgresql:// selects Postgres (via the
// pgx stdlib driver). A dsn starting with sqlite:, sqlite3: or file:, or a
// bare path ending in .db, .sqlite or .sqlite3, selects SQLite (via the
// pure-Go modernc.org/sqlite driver, so no cgo is needed).
func Open(dsn string) (*sql.DB, Dialect, error) {
	dialect, err := detectDialect(dsn)
	if err != nil {
		return nil, "", err
	}

	if dialect == Postgres {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, "", fmt.Errorf("failed to open database: %w", err)
		}
		return db, dialect, nil
	}

	sqliteDSN, path, err := sqliteDSN(dsn)
	if err != nil {
		return nil, "", err
	}
	if dir := filepath.Dir(path); path != "" && path != ":memory:" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN)
	if err != nil {
		return nil, "", fmt.Errorf("failed to open database: %w", err)
	}
	return db, dialect, nil
}

func detectDialect(dsn string) (Dialect, error) {
	lower := strings.ToLower(dsn)
	switch {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return Postgres, nil
	case strings.HasPrefix(lower, "sqlite:"), strings.HasPrefix(lower, "sqlite3:"), strings.HasPrefix(lower, "file:"):
		return SQLite, nil
	}
	path, _, _ := strings.Cut(lower, "?")
	for _, ext := range []string{".db", ".sqlite", ".sqlite3"} {
		if strings.HasSuffix(path, ext) {
			return SQLite, nil
		}
	}
	return "", fmt.Errorf("unrecognised DATABASE_URL %q: use postgres://... for Postgres, or sqlite:path/to/file.db for SQLite", dsn)
}

// sqliteDSN turns a user-supplied SQLite DATABASE_URL into a
// modernc.org/sqlite DSN, adding the settings this application relies on
// unless the user has set them explicitly:
//
//   - foreign_keys: whatsmeow refuses to run without it, and the app's own
//     ON DELETE CASCADE relationships need it too.
//   - busy_timeout + WAL + immediate transactions: many goroutines (one
//     whatsmeow client per user plus MCP requests) share one database file,
//     so writers must wait for each other rather than fail with SQLITE_BUSY.
//   - _time_format/_timezone: timestamps are written as UTC text in one
//     consistent, lexically sortable format so range filters and ORDER BY
//     behave the same as on Postgres.
//
// It also returns the filesystem path, so the caller can create its
// directory.
func sqliteDSN(dsn string) (string, string, error) {
	rest := dsn
	for _, prefix := range []string{"sqlite3://", "sqlite://", "sqlite3:", "sqlite:", "file:"} {
		if len(rest) >= len(prefix) && strings.EqualFold(rest[:len(prefix)], prefix) {
			rest = rest[len(prefix):]
			break
		}
	}
	path, rawQuery, _ := strings.Cut(rest, "?")
	if path == "" {
		return "", "", fmt.Errorf("SQLite DATABASE_URL %q has no file path", dsn)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse SQLite DATABASE_URL options: %w", err)
	}

	hasPragma := func(name string) bool {
		for _, v := range q["_pragma"] {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), name) {
				return true
			}
		}
		return false
	}
	for _, p := range []struct{ name, value string }{
		{"foreign_keys", "foreign_keys(1)"},
		{"busy_timeout", "busy_timeout(10000)"},
		{"journal_mode", "journal_mode(WAL)"},
	} {
		if !hasPragma(p.name) {
			q.Add("_pragma", p.value)
		}
	}
	for k, v := range map[string]string{"_txlock": "immediate", "_time_format": "sqlite", "_timezone": "UTC"} {
		if !q.Has(k) {
			q.Set(k, v)
		}
	}

	return "file:" + path + "?" + q.Encode(), path, nil
}

// Migrate applies the embedded SQL migrations for the dialect in filename
// order, tracking which have already run in a schema_migrations table.
func Migrate(ctx context.Context, sqlDB *sql.DB, dialect Dialect) error {
	if _, err := sqlDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename   TEXT PRIMARY KEY,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	dir := "migrations/" + string(dialect)
	entries, err := fs.ReadDir(migrationFiles, dir)
	if err != nil {
		return fmt.Errorf("failed to read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("failed to check migration state for %s: %w", name, err)
		}
		if applied {
			continue
		}

		contents, err := migrationFiles.ReadFile(dir + "/" + name)
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", name, err)
		}

		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction for migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %s: %w", name, err)
		}
	}

	return nil
}
