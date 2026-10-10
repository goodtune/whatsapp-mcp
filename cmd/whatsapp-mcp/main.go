// Command whatsapp-mcp is a single binary that serves a multi-tenant
// WhatsApp MCP server: users sign in with GitHub through the MCP client's
// own OAuth flow, link their own WhatsApp number via an MCP tool (QR code
// or phone-pairing code), and then use the WhatsApp MCP tools against
// their own account.
// All state (accounts, WhatsApp sessions, chat history) lives in Postgres
// or SQLite.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	dotvault "github.com/goodtune/dotvault/client"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.ngrok.com/ngrok"
	ngrokconfig "golang.ngrok.com/ngrok/config"

	"github.com/goodtune/whatsapp-mcp/internal/auth"
	"github.com/goodtune/whatsapp-mcp/internal/config"
	"github.com/goodtune/whatsapp-mcp/internal/db"
	"github.com/goodtune/whatsapp-mcp/internal/httpapi"
	"github.com/goodtune/whatsapp-mcp/internal/mcpserver"
	"github.com/goodtune/whatsapp-mcp/internal/oauth"
	"github.com/goodtune/whatsapp-mcp/internal/store"
	"github.com/goodtune/whatsapp-mcp/internal/wa"
)

// version is injected at build time via -ldflags "-X main.version=...". It is
// the v-stripped semantic version (0.2.0): GoReleaser's {{.Version}} strips
// the tag's leading "v".
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// A private FlagSet, not the flag.CommandLine default: the dotvault
	// client pulls in Google's TPM/confidential-computing attestation
	// libraries several layers down, and those register their own global
	// flags (-sev_guest_device_path and the like) on flag.CommandLine via
	// init(). Keeping our own flag(s) on a separate FlagSet means our
	// --help stays limited to flags we actually support, and avoids
	// colliding with whatever that dependency graph registers.
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	ngrokDotvault := fs.String("ngrok-dotvault", "",
		"Read the ngrok authtoken from dotvault instead of NGROK_AUTHTOKEN, given as "+
			"\"<secret>/<field>\" (e.g. \"whatsapp-mcp/ngrok/authtoken\" for the authtoken field "+
			"of kv/users/<you>/whatsapp-mcp/ngrok — there is no default secret, so the full path "+
			"is always required). Requires a cached dotvault login (run \"dotvault login\" first); "+
			"overrides NGROK_AUTHTOKEN when both are set.")
	githubDotvaultClientID := fs.String("github-dotvault-client-id", "",
		"Read the GitHub OAuth app's client ID from dotvault instead of GITHUB_CLIENT_ID, given "+
			"as \"<secret>/<field>\" (e.g. \"whatsapp-mcp/github/client_id\"). Requires a cached "+
			"dotvault login; overrides GITHUB_CLIENT_ID when both are set.")
	githubDotvaultClientSecret := fs.String("github-dotvault-client-secret", "",
		"Read the GitHub OAuth app's client secret from dotvault instead of GITHUB_CLIENT_SECRET, "+
			"given as \"<secret>/<field>\" (e.g. \"whatsapp-mcp/github/client_secret\"). Requires a "+
			"cached dotvault login; overrides GITHUB_CLIENT_SECRET when both are set.")
	dotvaultConfig := fs.String("dotvault-config", "",
		"Path to dotvault's config file, for a dotvault daemon that was started with its own "+
			"--config pointing somewhere other than the OS default (e.g. \"~/.config/dotvault/config.yaml\"). "+
			"Only read when a --*-dotvault* flag is set; defaults to dotvault's standard system path "+
			"(dotvault.DefaultConfigPath()) when unset.")
	showVersion := fs.Bool("version", false, "Print the version and exit.")
	fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	log.Printf("whatsapp-mcp %s", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if *ngrokDotvault != "" || *githubDotvaultClientID != "" || *githubDotvaultClientSecret != "" {
		vault, err := dotvaultClient(ctx, *dotvaultConfig)
		if err != nil {
			return err
		}
		if *ngrokDotvault != "" {
			token, err := readDotvaultRef(ctx, vault, *ngrokDotvault)
			if err != nil {
				return fmt.Errorf("--ngrok-dotvault: %w", err)
			}
			if cfg.NgrokAuthtoken != "" {
				log.Printf("--ngrok-dotvault overrides NGROK_AUTHTOKEN")
			}
			cfg.NgrokAuthtoken = token
		}
		if *githubDotvaultClientID != "" {
			if cfg.GitHubClientID, err = readDotvaultRef(ctx, vault, *githubDotvaultClientID); err != nil {
				return fmt.Errorf("--github-dotvault-client-id: %w", err)
			}
		}
		if *githubDotvaultClientSecret != "" {
			if cfg.GitHubClientSecret, err = readDotvaultRef(ctx, vault, *githubDotvaultClientSecret); err != nil {
				return fmt.Errorf("--github-dotvault-client-secret: %w", err)
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	sqlDB, dialect, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	log.Printf("using %s database", dialect)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return err
	}

	if err := db.Migrate(ctx, sqlDB, dialect); err != nil {
		return err
	}

	authService := auth.NewService(sqlDB, cfg.JWTSecret)
	msgStore := store.New(sqlDB)

	waManager, err := wa.NewManager(ctx, sqlDB, dialect, msgStore, cfg.MediaDir)
	if err != nil {
		return err
	}
	if err := waManager.ReconnectAll(ctx); err != nil {
		log.Printf("warning: failed to reconnect some linked WhatsApp devices: %v", err)
	}

	// Listen first: with ngrok, the public URL (which the OAuth endpoints
	// are built from) is only known once the tunnel is up.
	listener, publicURL, err := listen(ctx, cfg)
	if err != nil {
		return err
	}
	resourceURL := publicURL + "/mcp"

	oauthServer := oauth.New(oauth.Config{
		BaseURL:            publicURL,
		ResourceURL:        resourceURL,
		GitHubClientID:     cfg.GitHubClientID,
		GitHubClientSecret: cfg.GitHubClientSecret,
		AllowedUsers:       cfg.GitHubAllowedUsers,
	}, sqlDB, authService)

	mcpSrv := mcpserver.New(waManager, msgStore)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv.MCPServer()
	}, nil)

	verifyJWT := func(_ context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		userID, expiresAt, err := authService.VerifyToken(token, resourceURL)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", mcpauth.ErrInvalidToken, err)
		}
		return &mcpauth.TokenInfo{UserID: fmt.Sprintf("%d", userID), Expiration: expiresAt}, nil
	}
	requireAuth := mcpauth.RequireBearerToken(verifyJWT, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: oauthServer.ResourceMetadataURL(),
	})

	mux := http.NewServeMux()
	httpapi.Register(mux)
	oauthServer.Register(mux)
	mux.Handle("/mcp", requireAuth(mcpHandler))

	httpServer := &http.Server{Handler: mux}

	log.Printf("MCP endpoint: %s", resourceURL)
	log.Printf("GitHub OAuth app callback URL must be: %s", oauthServer.GitHubCallbackURL())

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// listen opens the address the HTTP/MCP server will be served on, and
// returns the public URL it is reachable at: a public ngrok tunnel when
// NGROK_AUTHTOKEN is configured (so the server can be self-hosted behind
// NAT or a firewall with no port-forwarding or reverse proxy setup), or a
// plain local TCP listener otherwise.
func listen(ctx context.Context, cfg config.Config) (net.Listener, string, error) {
	if cfg.NgrokAuthtoken == "" {
		listener, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return nil, "", fmt.Errorf("failed to listen on %s: %w", cfg.HTTPAddr, err)
		}
		log.Printf("whatsapp-mcp listening on %s", cfg.HTTPAddr)
		publicURL := cfg.PublicURL
		if publicURL == "" {
			publicURL = fmt.Sprintf("http://localhost:%d", listener.Addr().(*net.TCPAddr).Port)
		}
		return listener, publicURL, nil
	}

	endpointOpts := []ngrokconfig.HTTPEndpointOption{}
	if cfg.NgrokDomain != "" {
		endpointOpts = append(endpointOpts, ngrokconfig.WithDomain(cfg.NgrokDomain))
	}

	tunnel, err := ngrok.Listen(ctx,
		ngrokconfig.HTTPEndpoint(endpointOpts...),
		ngrok.WithAuthtoken(cfg.NgrokAuthtoken),
	)
	if err != nil {
		return nil, "", fmt.Errorf("failed to start ngrok tunnel: %w", err)
	}
	log.Printf("whatsapp-mcp listening on public ngrok tunnel: %s", tunnel.URL())
	return tunnel, strings.TrimSuffix(tunnel.URL(), "/"), nil
}

// dotvaultClient returns a client for the caller's dotvault store
// (github.com/goodtune/dotvault), for reading secrets such as the ngrok
// authtoken or the GitHub OAuth app credentials.
//
// configPath, if non-empty, overrides dotvault's own config discovery: a
// dotvault daemon started with its own --config pointing somewhere other
// than the OS default (as the "run as a per-user agent" setups in the
// README do, rather than installing a system-wide config) reads its secret
// paths from that file, not the default one, and this process has no way to
// discover that override except being told. An empty configPath falls back
// to dotvault.DefaultConfigPath(), the OS-standard location.
//
// This runs inside a long-lived daemon, so it only ever uses
// AuthenticateCached: dotvault's own client docs are explicit that the
// interactive Authenticate/Login entry points must not be called from a
// non-interactive service (they can open a browser or block on a terminal
// prompt). A missing cached token is therefore an operator error (run
// `dotvault login` first), not something this process should try to fix by
// prompting.
func dotvaultClient(ctx context.Context, configPath string) (*dotvault.Client, error) {
	if configPath == "" {
		configPath = dotvault.DefaultConfigPath()
	} else if expanded, err := expandHome(configPath); err != nil {
		return nil, fmt.Errorf("--dotvault-config: %w", err)
	} else {
		configPath = expanded
	}

	cfg, err := dotvault.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load dotvault config: %w", err)
	}

	cli, err := dotvault.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dotvault client: %w", err)
	}

	if err := cli.AuthenticateCached(ctx); err != nil {
		switch {
		case errors.Is(err, dotvault.ErrLoginRequired):
			return nil, fmt.Errorf("dotvault: no cached login; run `dotvault login` and retry: %w", err)
		case errors.Is(err, dotvault.ErrUnreachable):
			return nil, fmt.Errorf("dotvault: vault unreachable: %w", err)
		default:
			return nil, fmt.Errorf("dotvault: authentication failed: %w", err)
		}
	}
	return cli, nil
}

// readDotvault reads one field of one of the caller's dotvault secrets, e.g.
// service "ngrok", field "authtoken" reads kv/users/<you>/ngrok's
// "authtoken" field.
func readDotvault(ctx context.Context, cli *dotvault.Client, service, field string) (string, error) {
	value, found, err := cli.ReadUserSecret(ctx, service, field)
	if err != nil {
		return "", fmt.Errorf("dotvault: failed to read %s/%s: %w", service, field, err)
	}
	if !found {
		return "", fmt.Errorf("dotvault: no \"%s\" field under the \"%s\" secret for your identity; enroll it first", field, service)
	}
	return value, nil
}

// readDotvaultRef reads the dotvault field named by ref, "<secret>/<field>"
// (e.g. "whatsapp-mcp/ngrok/authtoken"). Everything up to the last slash is
// the secret path — which can itself contain slashes, as in that example —
// and the final segment is the field, which never does.
//
// Every --*-dotvault* flag takes a ref rather than a field name with a
// secret name defaulted elsewhere: a default secret (e.g. always "ngrok")
// is a silent assumption about where operators keep that credential, and
// it's wrong as soon as a secret holds more than one app's worth of
// tokens. Spelling out the full path on every flag means a typo'd or
// unenrolled secret fails loudly, instead of this process quietly reading
// whatever happens to live at the assumed default.
func readDotvaultRef(ctx context.Context, cli *dotvault.Client, ref string) (string, error) {
	i := strings.LastIndex(ref, "/")
	if i <= 0 || i == len(ref)-1 {
		return "", fmt.Errorf("%q must be \"<secret>/<field>\", e.g. \"whatsapp-mcp/ngrok/authtoken\"", ref)
	}
	return readDotvault(ctx, cli, ref[:i], ref[i+1:])
}

// expandHome expands a leading "~" or "~/..." in path to the caller's home
// directory, as a shell would. --dotvault-config is read by this process
// directly, with no shell in between to do that expansion (e.g. when set
// from a launchd/systemd unit's literal argument list), so a path copied
// from a shell prompt with a leading "~" would otherwise be passed to
// dotvault.LoadConfig verbatim and fail with a confusing "no such file".
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home directory for %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
