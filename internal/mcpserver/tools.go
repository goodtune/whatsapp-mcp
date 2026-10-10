// Package mcpserver defines the WhatsApp MCP tools and wires them to the
// per-user WhatsApp client manager and message store. Every tool call is
// scoped to the authenticated user found in the request context (see
// internal/auth), which the HTTP layer populates from the caller's JWT.
package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/goodtune/whatsapp-mcp/internal/auth"
	"github.com/goodtune/whatsapp-mcp/internal/store"
	"github.com/goodtune/whatsapp-mcp/internal/wa"
)

type Server struct {
	mcp   *server.MCPServer
	wa    *wa.Manager
	store *store.Store
}

func New(waManager *wa.Manager, st *store.Store) *Server {
	s := &Server{
		mcp:   server.NewMCPServer("whatsapp", "1.0.0", server.WithToolCapabilities(false)),
		wa:    waManager,
		store: st,
	}
	s.registerTools()
	return s
}

// MCPServer returns the underlying mcp-go server for mounting onto an HTTP transport.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcp
}

func currentUserID(ctx context.Context) (int64, error) {
	userID, ok := auth.UserIDFromContext(ctx)
	if !ok {
		return 0, fmt.Errorf("no authenticated user in context")
	}
	return userID, nil
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to encode result", err), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (s *Server) registerTools() {
	s.registerLinkingTools()
	s.registerContactAndChatTools()
	s.registerMessageTools()
	s.registerSendTools()
}

func (s *Server) registerLinkingTools() {
	s.mcp.AddTool(
		mcp.NewTool("link_whatsapp",
			mcp.WithDescription(
				"Link the caller's WhatsApp account to this server. Call this (and keep calling it, "+
					"every few seconds, until it reports 'linked') to retrieve a QR code image to scan with "+
					"WhatsApp > Linked Devices, or pass phone_number to get an 8-character pairing code to "+
					"enter under Linked Devices > Link with phone number instead. The QR code and pairing "+
					"code expire after a short time, so keep calling this tool to get a fresh one until linking succeeds.",
			),
			mcp.WithString("phone_number", mcp.Description(
				"Optional. The caller's WhatsApp phone number in international format with no '+' or "+
					"spaces (e.g. 15551234567). When provided, a pairing code is issued instead of a QR code.",
			)),
		),
		s.handleLinkWhatsApp,
	)

	s.mcp.AddTool(
		mcp.NewTool("whatsapp_status",
			mcp.WithDescription("Check whether the caller has a WhatsApp number linked to this server, and whether it's currently connected."),
		),
		s.handleWhatsAppStatus,
	)

	s.mcp.AddTool(
		mcp.NewTool("whatsapp_logout",
			mcp.WithDescription("Unlink the caller's WhatsApp device from this server. A new number can be linked afterwards with link_whatsapp."),
		),
		s.handleWhatsAppLogout,
	)
}

func (s *Server) handleLinkWhatsApp(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	phoneNumber := req.GetString("phone_number", "")

	state, err := s.wa.LinkDevice(ctx, userID, phoneNumber)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to start linking", err), nil
	}

	if state.Linked {
		return mcp.NewToolResultText(fmt.Sprintf("Already linked as %s.", state.JID)), nil
	}
	if state.Error != nil {
		return mcp.NewToolResultError(state.Error.Error()), nil
	}
	if state.PairCode != "" {
		return mcp.NewToolResultText(fmt.Sprintf(
			"Enter this pairing code in WhatsApp under Settings > Linked Devices > Link with phone number: %s\n"+
				"If it expires before you enter it, call link_whatsapp again for a new one.", state.PairCode,
		)), nil
	}
	if state.QRCode != "" {
		png, err := qrcode.Encode(state.QRCode, qrcode.Medium, 256)
		if err != nil {
			return mcp.NewToolResultText(fmt.Sprintf(
				"Scan this code with WhatsApp > Linked Devices > Link a Device: %s\n(failed to render it as an image: %v)",
				state.QRCode, err,
			)), nil
		}
		return mcp.NewToolResultImage(
			"Scan this QR code with WhatsApp > Linked Devices > Link a Device. "+
				"It refreshes periodically, so call link_whatsapp again if it expires before you scan it.",
			base64.StdEncoding.EncodeToString(png),
			"image/png",
		), nil
	}

	return mcp.NewToolResultText("Starting WhatsApp linking, call link_whatsapp again in a couple of seconds to get the QR code."), nil
}

func (s *Server) handleWhatsAppStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	status, err := s.wa.Status(ctx, userID)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to check status", err), nil
	}
	return jsonResult(status)
}

func (s *Server) handleWhatsAppLogout(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := s.wa.Logout(ctx, userID); err != nil {
		return mcp.NewToolResultErrorFromErr("failed to log out", err), nil
	}
	return mcp.NewToolResultText("WhatsApp device unlinked."), nil
}
