// Package config loads the server's runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
)

type Config struct {
	// DatabaseURL is a Postgres connection string, e.g.
	// postgres://user:pass@host:5432/whatsapp_mcp?sslmode=disable
	DatabaseURL string
	// JWTSecret signs and verifies session tokens issued at login.
	JWTSecret string
	// HTTPAddr is the address the HTTP/MCP server listens on.
	HTTPAddr string
	// MediaDir is the base directory downloaded media is stored under,
	// one subdirectory per user.
	MediaDir string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
		HTTPAddr:    getEnvDefault("HTTP_ADDR", ":8080"),
		MediaDir:    getEnvDefault("MEDIA_DIR", "store/media"),
	}

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL must be set, e.g. postgres://user:pass@localhost:5432/whatsapp_mcp?sslmode=disable")
	}
	if cfg.JWTSecret == "" {
		return cfg, fmt.Errorf("JWT_SECRET must be set to a long random string")
	}

	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
