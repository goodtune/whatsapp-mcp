package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"

	"github.com/goodtune/whatsapp-mcp/internal/auth"
	"github.com/goodtune/whatsapp-mcp/internal/db"
	"github.com/goodtune/whatsapp-mcp/internal/store"
)

// openTestDB opens a fresh SQLite database by default. Set
// TEST_DATABASE_URL to an empty Postgres database to run the same tests
// against Postgres.
func openTestDB(t *testing.T) (*store.Store, *auth.Service) {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "sqlite:" + filepath.Join(t.TempDir(), "sub", "test.db")
	}
	sqlDB, dialect, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	if err := db.Migrate(ctx, sqlDB, dialect); err != nil {
		t.Fatal(err)
	}
	// Running again must be a no-op.
	if err := db.Migrate(ctx, sqlDB, dialect); err != nil {
		t.Fatal(err)
	}
	// whatsmeow's own schema must install on the same connection; on SQLite
	// this also checks that foreign keys are enabled.
	if err := sqlstore.NewWithDB(sqlDB, dialect.DriverName(), nil).Upgrade(ctx); err != nil {
		t.Fatalf("whatsmeow upgrade: %v", err)
	}

	return store.New(sqlDB), auth.NewService(sqlDB, "test-secret")
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	st, authSvc := openTestDB(t)

	user, err := authSvc.Register(ctx, "alice", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authSvc.Register(ctx, "alice", "password123"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("duplicate register: got %v, want ErrUsernameTaken", err)
	}
	if _, err := authSvc.Authenticate(ctx, "alice", "password123"); err != nil {
		t.Fatal(err)
	}

	// Non-UTC input times must still order and filter correctly.
	sydney := time.FixedZone("AEST", 10*3600)
	base := time.Date(2026, 1, 2, 9, 0, 0, 0, sydney)
	direct := "61400000000@s.whatsapp.net"
	group := "123-456@g.us"

	if err := st.StoreChat(ctx, user.ID, direct, "Bob", base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.StoreChat(ctx, user.ID, group, "Team", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	msgs := []struct {
		id, chat, sender, content string
		at                        time.Duration
		fromMe                    bool
	}{
		{"m1", direct, "61400000000", "hello bob", 0, false},
		{"m2", group, "61400000000", "Team meeting", time.Minute, false},
		{"m3", direct, "me", "hi again", 2 * time.Minute, true},
	}
	for _, m := range msgs {
		if err := st.StoreMessage(ctx, user.ID, m.id, m.chat, m.sender, m.content, base.Add(m.at), m.fromMe,
			"", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	// Upsert path.
	if err := st.StoreMessage(ctx, user.ID, "m1", direct, "61400000000", "hello bob!", base, false,
		"image", "a.jpg", "https://example/a", []byte{1}, []byte{2}, []byte{3}, 42); err != nil {
		t.Fatal(err)
	}
	mt, fn, _, key, _, _, length, err := st.GetMediaInfo(ctx, user.ID, "m1", direct)
	if err != nil || mt != "image" || fn != "a.jpg" || len(key) != 1 || length != 42 {
		t.Fatalf("GetMediaInfo = %q %q %v %d, %v", mt, fn, key, length, err)
	}
	if err := st.StoreMediaInfo(ctx, user.ID, "m1", direct, "https://example/b", []byte{9}, nil, nil, 7); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListMessages(ctx, user.ID, store.ListMessagesFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); got != "m3,m2,m1" {
		t.Fatalf("ListMessages order = %s", got)
	}
	if !all[0].IsFromMe || all[0].ChatName != "Bob" {
		t.Fatalf("unexpected first message: %+v", all[0])
	}

	after := "2026-01-01T23:00:30Z" // 09:00:30 AEST
	before := "2026-01-02T09:01:30+10:00"
	ranged, err := st.ListMessages(ctx, user.ID, store.ListMessagesFilter{After: &after, Before: &before})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(ranged); got != "m2" {
		t.Fatalf("ranged ListMessages = %s", got)
	}

	q := "TEAM"
	searched, err := st.ListMessages(ctx, user.ID, store.ListMessagesFilter{Query: &q})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(searched); got != "m2" {
		t.Fatalf("query ListMessages = %s", got)
	}

	bad := "yesterday"
	if _, err := st.ListMessages(ctx, user.ID, store.ListMessagesFilter{After: &bad}); err == nil {
		t.Fatal("expected error for invalid timestamp")
	}

	mc, err := st.GetMessageContext(ctx, user.ID, "m3", 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(mc.Before); got != "m1" || len(mc.After) != 0 {
		t.Fatalf("context before=%s after=%d", got, len(mc.After))
	}

	chats, err := st.ListChats(ctx, user.ID, "", 10, 0, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 || chats[0].JID != direct || chats[0].LastMessage == nil || *chats[0].LastMessage != "hi again" {
		t.Fatalf("ListChats = %+v", chats)
	}
	if chats[0].LastMessageTime == nil || !chats[0].LastMessageTime.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("LastMessageTime = %v", chats[0].LastMessageTime)
	}

	contacts, err := st.SearchContacts(ctx, user.ID, "bob")
	if err != nil || len(contacts) != 1 || contacts[0].PhoneNumber != "61400000000" {
		t.Fatalf("SearchContacts = %+v, %v", contacts, err)
	}

	if _, err := st.GetContactChats(ctx, user.ID, "61400000000", 10, 0); err != nil {
		t.Fatal(err)
	}
	last, err := st.GetLastInteraction(ctx, user.ID, direct)
	if err != nil || last == nil || last.ID != "m3" {
		t.Fatalf("GetLastInteraction = %+v, %v", last, err)
	}
	chat, err := st.GetChat(ctx, user.ID, group, true)
	if err != nil || chat == nil || !chat.IsGroup() || chat.LastMessage == nil {
		t.Fatalf("GetChat = %+v, %v", chat, err)
	}
	dc, err := st.GetDirectChatByContact(ctx, user.ID, "61400000000")
	if err != nil || dc == nil || dc.JID != direct {
		t.Fatalf("GetDirectChatByContact = %+v, %v", dc, err)
	}
	if name := st.GetSenderName(ctx, user.ID, direct); name != "Bob" {
		t.Fatalf("GetSenderName = %q", name)
	}
	if name, ok, err := st.ExistingChatName(ctx, user.ID, group); err != nil || !ok || name != "Team" {
		t.Fatalf("ExistingChatName = %q %v %v", name, ok, err)
	}
}

func ids(ms []store.Message) string {
	s := ""
	for i, m := range ms {
		if i > 0 {
			s += ","
		}
		s += m.ID
	}
	return s
}
