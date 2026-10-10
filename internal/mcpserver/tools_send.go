package mcpserver

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

const recipientDescription = "The recipient - either a phone number with country code but no '+' or other symbols, " +
	"or a JID (e.g. \"123456789@s.whatsapp.net\" or a group JID like \"123456789@g.us\")"

func (s *Server) registerSendTools() {
	s.mcp.AddTool(
		mcp.NewTool("send_message",
			mcp.WithDescription("Send a WhatsApp text message to a person or group. For group chats use the JID."),
			mcp.WithString("recipient", mcp.Required(), mcp.Description(recipientDescription)),
			mcp.WithString("message", mcp.Required(), mcp.Description("The message text to send")),
		),
		s.handleSendMessage,
	)

	s.mcp.AddTool(
		mcp.NewTool("send_file",
			mcp.WithDescription("Send a file (image, raw audio, video, or document) via WhatsApp. For group chats use the JID."),
			mcp.WithString("recipient", mcp.Required(), mcp.Description(recipientDescription)),
			mcp.WithString("media_path", mcp.Required(), mcp.Description("Absolute path, accessible to this server, to the media file to send")),
		),
		s.handleSendFile,
	)

	s.mcp.AddTool(
		mcp.NewTool("send_audio_message",
			mcp.WithDescription(
				"Send an audio file as a playable WhatsApp voice message. For group chats use the JID. "+
					"Non-Ogg files are converted with ffmpeg; if that fails because ffmpeg isn't installed, use send_file instead.",
			),
			mcp.WithString("recipient", mcp.Required(), mcp.Description(recipientDescription)),
			mcp.WithString("media_path", mcp.Required(), mcp.Description("Absolute path, accessible to this server, to the audio file to send")),
		),
		s.handleSendAudioMessage,
	)

	s.mcp.AddTool(
		mcp.NewTool("download_media",
			mcp.WithDescription("Download media from a WhatsApp message and get the local file path."),
			mcp.WithString("message_id", mcp.Required(), mcp.Description("The ID of the message containing the media")),
			mcp.WithString("chat_jid", mcp.Required(), mcp.Description("The JID of the chat containing the message")),
		),
		s.handleDownloadMedia,
	)
}

func (s *Server) handleSendMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	recipient := req.GetString("recipient", "")
	if recipient == "" {
		return jsonResult(sendResultDTO{Success: false, Message: "recipient must be provided"})
	}

	success, message := s.wa.SendMessage(ctx, userID, recipient, req.GetString("message", ""))
	return jsonResult(sendResultDTO{Success: success, Message: message})
}

func (s *Server) handleSendFile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	success, message := s.wa.SendFile(ctx, userID, req.GetString("recipient", ""), req.GetString("media_path", ""))
	return jsonResult(sendResultDTO{Success: success, Message: message})
}

func (s *Server) handleSendAudioMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	success, message := s.wa.SendAudioMessage(ctx, userID, req.GetString("recipient", ""), req.GetString("media_path", ""))
	return jsonResult(sendResultDTO{Success: success, Message: message})
}

func (s *Server) handleDownloadMedia(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID, err := currentUserID(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	messageID := req.GetString("message_id", "")
	chatJID := req.GetString("chat_jid", "")

	_, _, filePath, err := s.wa.DownloadMedia(ctx, userID, messageID, chatJID)
	if err != nil {
		return jsonResult(downloadResultDTO{Success: false, Message: err.Error()})
	}
	return jsonResult(downloadResultDTO{Success: true, Message: "media downloaded successfully", FilePath: filePath})
}
