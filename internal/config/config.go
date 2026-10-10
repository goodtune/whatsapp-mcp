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

	// NgrokAuthtoken, if set, makes the server listen on a public ngrok
	// tunnel (via golang.ngrok.com/ngrok) instead of a local TCP address —
	// useful for self-hosting without port-forwarding or a reverse proxy.
	// HTTPAddr is ignored when this is set. Get a token at
	// https://dashboard.ngrok.com/get-started/your-authtoken.
	NgrokAuthtoken string
	// NgrokDomain optionally pins the tunnel to a reserved ngrok domain, so
	// the public URL stays the same across restarts. Only used when
	// NgrokAuthtoken is set.
	NgrokDomain string
	// NgrokBasicAuthUser/NgrokBasicAuthPass, if both set, require HTTP basic
	// auth at the ngrok edge for every request reaching the tunnel —
	// including /api/register, which is otherwise open to anyone who can
	// reach the server. Only used when NgrokAuthtoken is set.
	NgrokBasicAuthUser string
	NgrokBasicAuthPass string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		HTTPAddr:           getEnvDefault("HTTP_ADDR", ":8080"),
		MediaDir:           getEnvDefault("MEDIA_DIR", "store/media"),
		NgrokAuthtoken:     os.Getenv("NGROK_AUTHTOKEN"),
		NgrokDomain:        os.Getenv("NGROK_DOMAIN"),
		NgrokBasicAuthUser: os.Getenv("NGROK_BASIC_AUTH_USER"),
		NgrokBasicAuthPass: os.Getenv("NGROK_BASIC_AUTH_PASS"),
	}

	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL must be set, e.g. postgres://user:pass@localhost:5432/whatsapp_mcp?sslmode=disable")
	}
	if cfg.JWTSecret == "" {
		return cfg, fmt.Errorf("JWT_SECRET must be set to a long random string")
	}
	if (cfg.NgrokBasicAuthUser == "") != (cfg.NgrokBasicAuthPass == "") {
		return cfg, fmt.Errorf("NGROK_BASIC_AUTH_USER and NGROK_BASIC_AUTH_PASS must both be set, or neither")
	}

	return cfg, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
