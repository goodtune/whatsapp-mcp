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
	ngrokDotvaultField := fs.String("ngrok-dotvault", "",
		"Read the ngrok authtoken from this field of the caller's dotvault \"ngrok\" secret "+
			"(service \"ngrok\"), instead of NGROK_AUTHTOKEN. Requires a cached dotvault login "+
			"(run `dotvault login` first); overrides NGROK_AUTHTOKEN when both are set.")
	githubDotvaultService := fs.String("github-dotvault", "",
		"Read the GitHub OAuth app's credentials from the \"client_id\" and \"client_secret\" fields of "+
			"this dotvault secret, instead of GITHUB_CLIENT_ID/GITHUB_CLIENT_SECRET. Requires a cached "+
			"dotvault login; overrides those variables when both are set.")
	fs.Parse(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if *ngrokDotvaultField != "" || *githubDotvaultService != "" {
		vault, err := dotvaultClient(ctx)
		if err != nil {
			return err
		}
		if *ngrokDotvaultField != "" {
			token, err := readDotvault(ctx, vault, "ngrok", *ngrokDotvaultField)
			if err != nil {
				return err
			}
			if cfg.NgrokAuthtoken != "" {
				log.Printf("--ngrok-dotvault overrides NGROK_AUTHTOKEN")
			}
			cfg.NgrokAuthtoken = token
		}
		if *githubDotvaultService != "" {
			if cfg.GitHubClientID, err = readDotvault(ctx, vault, *githubDotvaultService, "client_id"); err != nil {
				return err
			}
			if cfg.GitHubClientSecret, err = readDotvault(ctx, vault, *githubDotvaultService, "client_secret"); err != nil {
				return err
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
// This runs inside a long-lived daemon, so it only ever uses
// AuthenticateCached: dotvault's own client docs are explicit that the
// interactive Authenticate/Login entry points must not be called from a
// non-interactive service (they can open a browser or block on a terminal
// prompt). A missing cached token is therefore an operator error (run
// `dotvault login` first), not something this process should try to fix by
// prompting.
func dotvaultClient(ctx context.Context) (*dotvault.Client, error) {
	cfg, err := dotvault.LoadConfig(dotvault.DefaultConfigPath())
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
