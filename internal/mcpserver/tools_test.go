package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	appauth "github.com/goodtune/whatsapp-mcp/internal/auth"
	"github.com/goodtune/whatsapp-mcp/internal/db"
	"github.com/goodtune/whatsapp-mcp/internal/store"
	"github.com/goodtune/whatsapp-mcp/internal/wa"
)

// testFixture is a Server wired up exactly as cmd/whatsapp-mcp wires one,
// backed by a throwaway SQLite database, plus the handles tests need to
// seed data and simulate a linked device without touching the network.
type testFixture struct {
	server *Server
	store  *store.Store
	db     *sql.DB
	userID int64
}

func newTestServer(t *testing.T) *testFixture {
	t.Helper()
	ctx := context.Background()

	sqlDB, dialect, err := db.Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(ctx, sqlDB, dialect); err != nil {
		t.Fatal(err)
	}

	st := store.New(sqlDB)
	waManager, err := wa.NewManager(ctx, sqlDB, dialect, st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	user, err := appauth.NewService(sqlDB, "test-secret").UpsertGitHubUser(ctx, 1, "tester")
	if err != nil {
		t.Fatal(err)
	}

	return &testFixture{server: New(waManager, st), store: st, db: sqlDB, userID: user.ID}
}

// authContext returns a context carrying the auth.TokenInfo that the bearer
// token middleware wired up in cmd/whatsapp-mcp attaches to every
// authenticated request, so currentUserID resolves to f.userID. It runs the
// real middleware rather than poking at package internals, since
// auth.TokenInfo is only ever placed in a context that way.
func (f *testFixture) authContext(t *testing.T) context.Context {
	t.Helper()
	verifier := func(_ context.Context, _ string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		return &mcpauth.TokenInfo{
			UserID:     strconv.FormatInt(f.userID, 10),
			Expiration: time.Now().Add(time.Hour),
		}, nil
	}
	var captured context.Context
	handler := mcpauth.RequireBearerToken(verifier, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r.Context()
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

// markLinked simulates an already-linked device by writing the row
// LinkDevice would have written, without dialing WhatsApp's servers.
func (f *testFixture) markLinked(t *testing.T, jid string) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO whatsapp_links (user_id, jid) VALUES ($1, $2)`, f.userID, jid); err != nil {
		t.Fatal(err)
	}
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("expected text content, got %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected *mcp.TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

// TestToolsRequireAuthentication checks every tool's handler refuses to run
// without an authenticated user in context, the guard every handler opens
// with via currentUserID.
func TestToolsRequireAuthentication(t *testing.T) {
	f := newTestServer(t)
	ctx := context.Background() // no auth.TokenInfo attached

	calls := map[string]func() *mcp.CallToolResult{
		"search_contacts": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleSearchContacts(ctx, nil, searchContactsInput{})
			return res
		},
		"list_chats": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleListChats(ctx, nil, listChatsInput{})
			return res
		},
		"get_chat": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleGetChat(ctx, nil, getChatInput{ChatJID: "x"})
			return res
		},
		"get_direct_chat_by_contact": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleGetDirectChatByContact(ctx, nil, getDirectChatByContactInput{SenderPhoneNumber: "x"})
			return res
		},
		"get_contact_chats": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleGetContactChats(ctx, nil, getContactChatsInput{JID: "x"})
			return res
		},
		"get_last_interaction": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleGetLastInteraction(ctx, nil, getLastInteractionInput{JID: "x"})
			return res
		},
		"list_messages": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleListMessages(ctx, nil, listMessagesInput{})
			return res
		},
		"get_message_context": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleGetMessageContext(ctx, nil, getMessageContextInput{MessageID: "x"})
			return res
		},
		"whatsapp_status": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleWhatsAppStatus(ctx, nil, struct{}{})
			return res
		},
		"whatsapp_logout": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleWhatsAppLogout(ctx, nil, struct{}{})
			return res
		},
		"link_whatsapp": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleLinkWhatsApp(ctx, nil, linkWhatsAppInput{})
			return res
		},
		"send_message": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleSendMessage(ctx, nil, sendMessageInput{Recipient: "x", Message: "hi"})
			return res
		},
		"send_file": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleSendFile(ctx, nil, sendFileInput{Recipient: "x", MediaPath: "x"})
			return res
		},
		"send_audio_message": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleSendAudioMessage(ctx, nil, sendAudioMessageInput{Recipient: "x", MediaPath: "x"})
			return res
		},
		"download_media": func() *mcp.CallToolResult {
			res, _, _ := f.server.handleDownloadMedia(ctx, nil, downloadMediaInput{MessageID: "x", ChatJID: "x"})
			return res
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			res := call()
			if res == nil || !res.IsError {
				t.Fatalf("%s: expected an auth error result, got %+v", name, res)
			}
			if got := textOf(t, res); got != "no authenticated user in context" {
				t.Fatalf("%s: error = %q", name, got)
			}
		})
	}
}

// TestListToolOutputsAreJSONObjects is a regression test for the bug this
// package shipped: search_contacts, list_chats, and get_contact_chats
// returned a bare JSON array as structuredContent, which MCP clients that
// enforce the spec's object-only requirement reject outright.
func TestListToolOutputsAreJSONObjects(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}

	_, contacts, err := f.server.handleSearchContacts(ctx, nil, searchContactsInput{Query: "Peter"})
	if err != nil {
		t.Fatal(err)
	}
	_, chats, err := f.server.handleListChats(ctx, nil, listChatsInput{})
	if err != nil {
		t.Fatal(err)
	}
	_, contactChats, err := f.server.handleGetContactChats(ctx, nil, getContactChatsInput{JID: jid})
	if err != nil {
		t.Fatal(err)
	}

	for name, out := range map[string]any{
		"search_contacts":   contacts,
		"list_chats":        chats,
		"get_contact_chats": contactChats,
	} {
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(raw) == 0 || raw[0] != '{' {
			t.Fatalf("%s: structuredContent must be a JSON object, got %s", name, raw)
		}
	}
}

func TestHandleSearchContacts(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	if err := f.store.StoreChat(context.Background(), f.userID, "61400000001@s.whatsapp.net", "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StoreChat(context.Background(), f.userID, "61400000002@s.whatsapp.net", "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}

	res, out, err := f.server.handleSearchContacts(ctx, nil, searchContactsInput{Query: "Peter"})
	if err != nil || res != nil {
		t.Fatalf("handleSearchContacts() = %+v, %+v, %v", res, out, err)
	}
	if len(out.Contacts) != 1 || out.Contacts[0].Name != "Peter Topp" || out.Contacts[0].PhoneNumber != "61400000001" {
		t.Fatalf("Contacts = %+v", out.Contacts)
	}
}

func TestHandleListChats(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	base := time.Now()
	if err := f.store.StoreChat(context.Background(), f.userID, "61400000001@s.whatsapp.net", "Older", base); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StoreChat(context.Background(), f.userID, "61400000002@s.whatsapp.net", "Newer", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	res, out, err := f.server.handleListChats(ctx, nil, listChatsInput{})
	if err != nil || res != nil {
		t.Fatalf("handleListChats() = %+v, %+v, %v", res, out, err)
	}
	if len(out.Chats) != 2 || out.Chats[0].Name != "Newer" {
		t.Fatalf("Chats = %+v", out.Chats)
	}
}

func TestHandleGetChat(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}

	res, out, err := f.server.handleGetChat(ctx, nil, getChatInput{ChatJID: jid})
	if err != nil || res != nil || out == nil || out.Name != "Peter Topp" {
		t.Fatalf("handleGetChat() found = %+v, %+v, %v", res, out, err)
	}

	res, out, err = f.server.handleGetChat(ctx, nil, getChatInput{ChatJID: "missing@s.whatsapp.net"})
	if err != nil || out != nil {
		t.Fatalf("handleGetChat() missing = %+v, %+v, %v", res, out, err)
	}
	if got := textOf(t, res); got != "No chat found with that JID." {
		t.Fatalf("handleGetChat() missing text = %q", got)
	}
}

func TestHandleGetDirectChatByContact(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}

	res, out, err := f.server.handleGetDirectChatByContact(ctx, nil, getDirectChatByContactInput{SenderPhoneNumber: "61400000001"})
	if err != nil || res != nil || out == nil || out.JID != jid {
		t.Fatalf("handleGetDirectChatByContact() found = %+v, %+v, %v", res, out, err)
	}

	res, out, err = f.server.handleGetDirectChatByContact(ctx, nil, getDirectChatByContactInput{SenderPhoneNumber: "999999"})
	if err != nil || out != nil {
		t.Fatalf("handleGetDirectChatByContact() missing = %+v, %+v, %v", res, out, err)
	}
	if got := textOf(t, res); got != "No direct chat found with that contact." {
		t.Fatalf("handleGetDirectChatByContact() missing text = %q", got)
	}
}

func TestHandleGetContactChats(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StoreMessage(context.Background(), f.userID, "m1", jid, "61400000001", "hi", time.Now(), false,
		"", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	res, out, err := f.server.handleGetContactChats(ctx, nil, getContactChatsInput{JID: "61400000001"})
	if err != nil || res != nil {
		t.Fatalf("handleGetContactChats() = %+v, %+v, %v", res, out, err)
	}
	if len(out.Chats) != 1 || out.Chats[0].JID != jid {
		t.Fatalf("Chats = %+v", out.Chats)
	}
}

func TestHandleGetLastInteraction(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StoreMessage(context.Background(), f.userID, "m1", jid, "61400000001", "hello there", time.Now(), false,
		"", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	res, _, err := f.server.handleGetLastInteraction(ctx, nil, getLastInteractionInput{JID: "61400000001"})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, res); !strings.Contains(got, "hello there") {
		t.Fatalf("handleGetLastInteraction() text = %q", got)
	}

	res, _, err = f.server.handleGetLastInteraction(ctx, nil, getLastInteractionInput{JID: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, res); got != "No interactions found." {
		t.Fatalf("handleGetLastInteraction() missing text = %q", got)
	}
}

func TestHandleListMessages(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StoreMessage(context.Background(), f.userID, "m1", jid, "61400000001", "hello there", time.Now(), false,
		"", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}

	noContext := false
	res, _, err := f.server.handleListMessages(ctx, nil, listMessagesInput{ChatJID: jid, IncludeContext: &noContext})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, res); !strings.Contains(got, "hello there") {
		t.Fatalf("handleListMessages() text = %q", got)
	}

	res, _, err = f.server.handleListMessages(ctx, nil, listMessagesInput{ChatJID: "no-such-chat", IncludeContext: &noContext})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, res); got != "No messages to display." {
		t.Fatalf("handleListMessages() empty text = %q", got)
	}
}

func TestHandleGetMessageContext(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001@s.whatsapp.net"
	base := time.Now()
	if err := f.store.StoreChat(context.Background(), f.userID, jid, "Peter Topp", base); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"m1", "m2", "m3"} {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := f.store.StoreMessage(context.Background(), f.userID, id, jid, "61400000001", id, at, false,
			"", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	res, out, err := f.server.handleGetMessageContext(ctx, nil, getMessageContextInput{MessageID: "m2", Before: intPtr(1), After: intPtr(1)})
	if err != nil || res != nil || out == nil {
		t.Fatalf("handleGetMessageContext() = %+v, %+v, %v", res, out, err)
	}
	if out.Message.ID != "m2" || len(out.Before) != 1 || out.Before[0].ID != "m1" || len(out.After) != 1 || out.After[0].ID != "m3" {
		t.Fatalf("context = %+v", out)
	}
}

func TestHandleWhatsAppStatus(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)

	res, out, err := f.server.handleWhatsAppStatus(ctx, nil, struct{}{})
	if err != nil || res != nil || out.Linked {
		t.Fatalf("handleWhatsAppStatus() not linked = %+v, %+v, %v", res, out, err)
	}

	f.markLinked(t, "61400000001:1@s.whatsapp.net")
	res, out, err = f.server.handleWhatsAppStatus(ctx, nil, struct{}{})
	if err != nil || res != nil || !out.Linked || out.Connected {
		t.Fatalf("handleWhatsAppStatus() linked = %+v, %+v, %v", res, out, err)
	}
}

func TestHandleWhatsAppLogout(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)

	res, _, err := f.server.handleWhatsAppLogout(ctx, nil, struct{}{})
	if err != nil || res.IsError {
		t.Fatalf("handleWhatsAppLogout() not linked = %+v, %v", res, err)
	}
	if got := textOf(t, res); got != "WhatsApp device unlinked." {
		t.Fatalf("handleWhatsAppLogout() text = %q", got)
	}

	f.markLinked(t, "61400000001:1@s.whatsapp.net")
	res, _, err = f.server.handleWhatsAppLogout(ctx, nil, struct{}{})
	if err != nil || res.IsError {
		t.Fatalf("handleWhatsAppLogout() linked = %+v, %v", res, err)
	}

	var remaining int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM whatsapp_links WHERE user_id = $1`, f.userID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("whatsapp_links row still present after logout")
	}
}

func TestHandleLinkWhatsAppAlreadyLinked(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)
	jid := "61400000001:1@s.whatsapp.net"
	f.markLinked(t, jid)

	res, _, err := f.server.handleLinkWhatsApp(ctx, nil, linkWhatsAppInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, res); got != "Already linked as "+jid+"." {
		t.Fatalf("handleLinkWhatsApp() text = %q", got)
	}
}

func TestHandleSendMessage(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)

	_, out, err := f.server.handleSendMessage(ctx, nil, sendMessageInput{Message: "hi"})
	if err != nil || out.Success || out.Message != "recipient must be provided" {
		t.Fatalf("handleSendMessage() empty recipient = %+v, %v", out, err)
	}

	_, out, err = f.server.handleSendMessage(ctx, nil, sendMessageInput{Recipient: "61400000001", Message: "hi"})
	if err != nil || out.Success || !strings.Contains(out.Message, "link_whatsapp") {
		t.Fatalf("handleSendMessage() not linked = %+v, %v", out, err)
	}
}

func TestHandleSendFile(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)

	_, out, err := f.server.handleSendFile(ctx, nil, sendFileInput{Recipient: "61400000001", MediaPath: "/does/not/matter"})
	if err != nil || out.Success || !strings.Contains(out.Message, "link_whatsapp") {
		t.Fatalf("handleSendFile() not linked = %+v, %v", out, err)
	}
}

func TestHandleSendAudioMessage(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)

	_, out, err := f.server.handleSendAudioMessage(ctx, nil, sendAudioMessageInput{Recipient: "61400000001", MediaPath: "/does/not/matter"})
	if err != nil || out.Success || !strings.Contains(out.Message, "link_whatsapp") {
		t.Fatalf("handleSendAudioMessage() not linked = %+v, %v", out, err)
	}
}

func TestHandleDownloadMedia(t *testing.T) {
	f := newTestServer(t)
	ctx := f.authContext(t)

	_, out, err := f.server.handleDownloadMedia(ctx, nil, downloadMediaInput{MessageID: "missing", ChatJID: "61400000001@s.whatsapp.net"})
	if err != nil || out.Success || !strings.Contains(out.Message, "failed to find message") {
		t.Fatalf("handleDownloadMedia() missing message = %+v, %v", out, err)
	}
}

// TestToolAnnotations checks the annotations published in tools/list,
// mirroring how the vitriolic admin MCP server's test_tools_list verifies
// its own readOnlyHint/destructiveHint/openWorldHint scheme: it's these
// annotations, not the tool descriptions, that let a client such as the
// Claude connector directory auto-approve safe tools and warn before
// destructive ones.
func TestToolAnnotations(t *testing.T) {
	f := newTestServer(t)
	ctx := context.Background()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := f.server.MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	byName := make(map[string]*mcp.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		byName[tool.Name] = tool
	}

	readOnly := []string{
		"search_contacts", "list_chats", "get_chat", "get_direct_chat_by_contact",
		"get_contact_chats", "get_last_interaction", "list_messages",
		"get_message_context", "whatsapp_status", "download_media",
	}
	for _, name := range readOnly {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s: expected readOnlyHint true, got %+v", name, tool.Annotations)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatalf("%s: expected destructiveHint false, got %+v", name, tool.Annotations)
		}
		if tool.Annotations.Title != tool.Title {
			t.Fatalf("%s: annotations.title %q != title %q", name, tool.Annotations.Title, tool.Title)
		}
	}

	write := []string{"link_whatsapp", "whatsapp_logout", "send_message", "send_file", "send_audio_message"}
	for _, name := range write {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s: expected readOnlyHint false, got %+v", name, tool.Annotations)
		}
		if tool.Annotations.Title != tool.Title {
			t.Fatalf("%s: annotations.title %q != title %q", name, tool.Annotations.Title, tool.Title)
		}
	}

	if got := byName["whatsapp_logout"].Annotations.DestructiveHint; got == nil || !*got {
		t.Fatalf("whatsapp_logout: expected destructiveHint true, got %v", got)
	}
	if got := byName["send_message"].Annotations.DestructiveHint; got == nil || *got {
		t.Fatalf("send_message: expected destructiveHint false, got %v", got)
	}
	if got := byName["link_whatsapp"].Annotations.OpenWorldHint; got == nil || !*got {
		t.Fatalf("link_whatsapp: expected openWorldHint true, got %v", got)
	}
}

func intPtr(v int) *int { return &v }
