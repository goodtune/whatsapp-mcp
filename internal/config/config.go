// Package config loads the server's runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	// DatabaseURL selects the database: a Postgres connection string, e.g.
	// postgres://user:pass@host:5432/whatsapp_mcp?sslmode=disable, or a
	// SQLite file, e.g. sqlite:store/whatsapp-mcp.db. Defaults to SQLite at
	// store/whatsapp-mcp.db when unset.
	DatabaseURL string
	// JWTSecret signs and verifies the OAuth access tokens issued to MCP
	// clients.
	JWTSecret string
	// HTTPAddr is the address the HTTP/MCP server listens on.
	HTTPAddr string
	// PublicURL is the URL MCP clients and browsers reach the server at,
	// e.g. https://whatsapp.example.com. OAuth endpoints and the GitHub
	// callback URL are derived from it. Ignored when serving on ngrok (the
	// tunnel's URL is used); otherwise defaults to http://localhost:<port>.
	PublicURL string
	// MediaDir is the base directory downloaded media is stored under,
	// one subdirectory per user.
	MediaDir string

	// GitHubClientID/GitHubClientSecret are the credentials of the GitHub
	// OAuth app users sign in with. Its callback URL must be
	// <public URL>/oauth/github/callback.
	GitHubClientID     string
	GitHubClientSecret string
	// GitHubAllowedUsers lists the GitHub logins allowed to sign in, from
	// the comma-separated GITHUB_ALLOWED_USERS.
	GitHubAllowedUsers []string

	// NgrokAuthtoken, if set, makes the server listen on a public ngrok
	// tunnel (via golang.ngrok.com/ngrok) instead of a local TCP address —
	// useful for self-hosting without port-forwarding or a reverse proxy.
	// HTTPAddr is ignored when this is set. Get a token at
	// https://dashboard.ngrok.com/get-started/your-authtoken.
	NgrokAuthtoken string
	// NgrokDomain optionally pins the tunnel to a reserved ngrok domain, so
	// the public URL stays the same across restarts — which the GitHub
	// OAuth app's fixed callback URL needs. Only used when NgrokAuthtoken
	// is set.
	NgrokDomain string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:        getEnvDefault("DATABASE_URL", "sqlite:store/whatsapp-mcp.db"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		HTTPAddr:           getEnvDefault("HTTP_ADDR", ":8080"),
		PublicURL:          strings.TrimSuffix(os.Getenv("PUBLIC_URL"), "/"),
		MediaDir:           getEnvDefault("MEDIA_DIR", "store/media"),
		GitHubClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		NgrokAuthtoken:     os.Getenv("NGROK_AUTHTOKEN"),
		NgrokDomain:        os.Getenv("NGROK_DOMAIN"),
	}
	for _, u := range strings.Split(os.Getenv("GITHUB_ALLOWED_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			cfg.GitHubAllowedUsers = append(cfg.GitHubAllowedUsers, u)
		}
	}

	if cfg.JWTSecret == "" {
		return cfg, fmt.Errorf("JWT_SECRET must be set to a long random string")
	}

	return cfg, nil
}

// Validate checks the settings that may also be supplied by command-line
// flags, so it runs after those are applied.
func (cfg Config) Validate() error {
	if cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "" {
		return fmt.Errorf("GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET must be set to a GitHub OAuth app's credentials")
	}
	if len(cfg.GitHubAllowedUsers) == 0 {
		return fmt.Errorf("GITHUB_ALLOWED_USERS must list the GitHub logins allowed to sign in, comma-separated")
	}
	return nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
