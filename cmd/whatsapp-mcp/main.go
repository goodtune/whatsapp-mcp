// Command whatsapp-mcp is a single binary that serves a multi-tenant
// WhatsApp MCP server: users register/log in over a small REST API, link
// their own WhatsApp number via an MCP tool (QR code or phone-pairing
// code), and then use the WhatsApp MCP tools against their own account.
// All state (accounts, WhatsApp sessions, chat history) lives in Postgres.
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
	fs.Parse(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if *ngrokDotvaultField != "" {
		token, err := ngrokAuthtokenFromDotvault(ctx, *ngrokDotvaultField)
		if err != nil {
			return err
		}
		if cfg.NgrokAuthtoken != "" {
			log.Printf("--ngrok-dotvault overrides NGROK_AUTHTOKEN")
		}
		cfg.NgrokAuthtoken = token
	}

	sqlDB, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return err
	}

	if err := db.Migrate(ctx, sqlDB); err != nil {
		return err
	}

	authService := auth.NewService(sqlDB, cfg.JWTSecret)
	msgStore := store.New(sqlDB)

	waManager, err := wa.NewManager(ctx, sqlDB, msgStore, cfg.MediaDir)
	if err != nil {
		return err
	}
	if err := waManager.ReconnectAll(ctx); err != nil {
		log.Printf("warning: failed to reconnect some linked WhatsApp devices: %v", err)
	}

	mcpSrv := mcpserver.New(waManager, msgStore)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv.MCPServer()
	}, nil)

	verifyJWT := func(_ context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		userID, expiresAt, err := authService.VerifyToken(token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", mcpauth.ErrInvalidToken, err)
		}
		return &mcpauth.TokenInfo{UserID: fmt.Sprintf("%d", userID), Expiration: expiresAt}, nil
	}
	requireAuth := mcpauth.RequireBearerToken(verifyJWT, nil)

	mux := http.NewServeMux()
	httpapi.New(authService).Register(mux)
	mux.Handle("/mcp", requireAuth(mcpHandler))

	httpServer := &http.Server{Handler: mux}

	listener, err := listen(ctx, cfg)
	if err != nil {
		return err
	}

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

// listen opens the address the HTTP/MCP server will be served on: a public
// ngrok tunnel when NGROK_AUTHTOKEN is configured (so the server can be
// self-hosted behind NAT or a firewall with no port-forwarding or reverse
// proxy setup), or a plain local TCP listener otherwise.
func listen(ctx context.Context, cfg config.Config) (net.Listener, error) {
	if cfg.NgrokAuthtoken == "" {
		listener, err := net.Listen("tcp", cfg.HTTPAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to listen on %s: %w", cfg.HTTPAddr, err)
		}
		log.Printf("whatsapp-mcp listening on %s", cfg.HTTPAddr)
		return listener, nil
	}

	endpointOpts := []ngrokconfig.HTTPEndpointOption{}
	if cfg.NgrokDomain != "" {
		endpointOpts = append(endpointOpts, ngrokconfig.WithDomain(cfg.NgrokDomain))
	}
	if cfg.NgrokBasicAuthUser != "" {
		endpointOpts = append(endpointOpts, ngrokconfig.WithBasicAuth(cfg.NgrokBasicAuthUser, cfg.NgrokBasicAuthPass))
	}

	tunnel, err := ngrok.Listen(ctx,
		ngrokconfig.HTTPEndpoint(endpointOpts...),
		ngrok.WithAuthtoken(cfg.NgrokAuthtoken),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start ngrok tunnel: %w", err)
	}
	log.Printf("whatsapp-mcp listening on public ngrok tunnel: %s", tunnel.URL())
	return tunnel, nil
}

// ngrokAuthtokenFromDotvault reads the ngrok authtoken from the caller's
// dotvault store (github.com/goodtune/dotvault), under the "ngrok" service
// and the given field, e.g. field "authtoken" reads kv/users/<you>/ngrok's
// "authtoken" field.
//
// This runs inside a long-lived daemon, so it only ever uses
// AuthenticateCached: dotvault's own client docs are explicit that the
// interactive Authenticate/Login entry points must not be called from a
// non-interactive service (they can open a browser or block on a terminal
// prompt). A missing cached token is therefore an operator error (run
// `dotvault login` first), not something this process should try to fix by
// prompting.
func ngrokAuthtokenFromDotvault(ctx context.Context, field string) (string, error) {
	cfg, err := dotvault.LoadConfig(dotvault.DefaultConfigPath())
	if err != nil {
		return "", fmt.Errorf("failed to load dotvault config: %w", err)
	}

	cli, err := dotvault.New(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to create dotvault client: %w", err)
	}

	if err := cli.AuthenticateCached(ctx); err != nil {
		switch {
		case errors.Is(err, dotvault.ErrLoginRequired):
			return "", fmt.Errorf("dotvault: no cached login; run `dotvault login` and retry: %w", err)
		case errors.Is(err, dotvault.ErrUnreachable):
			return "", fmt.Errorf("dotvault: vault unreachable: %w", err)
		default:
			return "", fmt.Errorf("dotvault: authentication failed: %w", err)
		}
	}

	token, found, err := cli.ReadUserSecret(ctx, "ngrok", field)
	if err != nil {
		return "", fmt.Errorf("dotvault: failed to read ngrok/%s: %w", field, err)
	}
	if !found {
		return "", fmt.Errorf("dotvault: no \"%s\" field under the \"ngrok\" secret for your identity; enroll it first", field)
	}

	return token, nil
}
