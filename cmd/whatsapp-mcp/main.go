// Command whatsapp-mcp is a single binary that serves a multi-tenant
// WhatsApp MCP server: users register/log in over a small REST API, link
// their own WhatsApp number via an MCP tool (QR code or phone-pairing
// code), and then use the WhatsApp MCP tools against their own account.
// All state (accounts, WhatsApp sessions, chat history) lives in Postgres.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
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
