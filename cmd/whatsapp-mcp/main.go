// Command whatsapp-mcp is a single binary that serves a multi-tenant
// WhatsApp MCP server: users register/log in over a small REST API, link
// their own WhatsApp number via an MCP tool (QR code or phone-pairing
// code), and then use the WhatsApp MCP tools against their own account.
// All state (accounts, WhatsApp sessions, chat history) lives in Postgres.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

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
	httpMCPServer := server.NewStreamableHTTPServer(mcpSrv.MCPServer())

	mux := http.NewServeMux()
	httpapi.New(authService).Register(mux)
	mux.Handle("/mcp", authService.Middleware(httpMCPServer))

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("whatsapp-mcp listening on %s", cfg.HTTPAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
