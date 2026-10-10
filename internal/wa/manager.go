// Package wa manages one whatsmeow WhatsApp client per logged-in user,
// backed by a shared Postgres or SQLite connection, and exposes the operations the MCP
// tools need: linking a device (QR or phone-pairing code), sending messages
// and media, and downloading media.
package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	appdb "github.com/goodtune/whatsapp-mcp/internal/db"
	"github.com/goodtune/whatsapp-mcp/internal/store"
)

var ErrNotLinked = errors.New("no WhatsApp number linked yet; call link_whatsapp first")

// LinkState is a snapshot of a user's device-linking progress, returned by
// the link_whatsapp MCP tool so the caller can render a QR code or relay a
// pairing code.
type LinkState struct {
	Linked   bool
	JID      string
	QRCode   string // raw QR payload; render this as a QR code, or show qr_png_base64
	QRExpiry time.Time
	PairCode string // phone-number linking code, if a phone number was supplied
	Error    error
}

type pendingLink struct {
	mu       sync.Mutex
	client   *whatsmeow.Client
	qrCode   string
	qrExpiry time.Time
	pairCode string
	err      error
}

func (p *pendingLink) snapshot() LinkState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return LinkState{QRCode: p.qrCode, QRExpiry: p.qrExpiry, PairCode: p.pairCode, Error: p.err}
}

func (p *pendingLink) setQR(code string, expiry time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.qrCode, p.qrExpiry = code, expiry
}

func (p *pendingLink) setPairCode(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pairCode = code
}

func (p *pendingLink) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

type Manager struct {
	db        *sql.DB
	container *sqlstore.Container
	store     *store.Store
	mediaDir  string
	logger    waLog.Logger

	mu      sync.Mutex
	clients map[int64]*whatsmeow.Client
	pending map[int64]*pendingLink
}

func NewManager(ctx context.Context, db *sql.DB, dialect appdb.Dialect, st *store.Store, mediaDir string) (*Manager, error) {
	logger := waLog.Stdout("WhatsApp", "INFO", true)
	container := sqlstore.NewWithDB(db, dialect.DriverName(), logger)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("failed to prepare whatsmeow schema: %w", err)
	}

	return &Manager{
		db:        db,
		container: container,
		store:     st,
		mediaDir:  mediaDir,
		logger:    logger,
		clients:   make(map[int64]*whatsmeow.Client),
		pending:   make(map[int64]*pendingLink),
	}, nil
}

// ReconnectAll reconnects every user who already has a linked WhatsApp
// device. Call this once at startup so incoming messages are captured even
// when no MCP tool call is in flight.
func (m *Manager) ReconnectAll(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `SELECT user_id, jid FROM whatsapp_links`)
	if err != nil {
		return fmt.Errorf("failed to list linked users: %w", err)
	}
	type link struct {
		userID int64
		jid    string
	}
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.userID, &l.jid); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan linked user: %w", err)
		}
		links = append(links, l)
	}
	rows.Close()

	for _, l := range links {
		if _, err := m.connectExisting(ctx, l.userID, l.jid); err != nil {
			m.logger.Errorf("failed to reconnect user %d (%s): %v", l.userID, l.jid, err)
		}
	}
	return nil
}

func (m *Manager) linkedJID(ctx context.Context, userID int64) (string, bool, error) {
	var jid string
	err := m.db.QueryRowContext(ctx, `SELECT jid FROM whatsapp_links WHERE user_id = $1`, userID).Scan(&jid)
	if err == sql.ErrNoRows {
		return "", false, nil
	} else if err != nil {
		return "", false, fmt.Errorf("failed to look up linked device: %w", err)
	}
	return jid, true, nil
}

func (m *Manager) saveLinkedJID(ctx context.Context, userID int64, jid string) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO whatsapp_links (user_id, jid) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET jid = EXCLUDED.jid
	`, userID, jid)
	if err != nil {
		return fmt.Errorf("failed to save linked device: %w", err)
	}
	return nil
}

// Status reports whether a user has a linked WhatsApp device and whether it
// is currently connected.
type Status struct {
	Linked    bool   `json:"linked"`
	JID       string `json:"jid,omitempty"`
	Connected bool   `json:"connected"`
}

func (m *Manager) Status(ctx context.Context, userID int64) (*Status, error) {
	jid, ok, err := m.linkedJID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Status{Linked: false}, nil
	}

	m.mu.Lock()
	client, connected := m.clients[userID]
	m.mu.Unlock()

	return &Status{Linked: true, JID: jid, Connected: connected && client.IsConnected()}, nil
}

// Logout disconnects and unlinks a user's WhatsApp device.
func (m *Manager) Logout(ctx context.Context, userID int64) error {
	m.mu.Lock()
	client, hasClient := m.clients[userID]
	delete(m.clients, userID)
	delete(m.pending, userID)
	m.mu.Unlock()

	jid, ok, err := m.linkedJID(ctx, userID)
	if err != nil {
		return err
	}

	if hasClient {
		client.Logout(ctx)
		client.Disconnect()
	} else if ok {
		parsed, err := types.ParseJID(jid)
		if err == nil {
			if device, err := m.container.GetDevice(ctx, parsed); err == nil && device != nil {
				_ = m.container.DeleteDevice(ctx, device)
			}
		}
	}

	if ok {
		if _, err := m.db.ExecContext(ctx, `DELETE FROM whatsapp_links WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("failed to remove link record: %w", err)
		}
	}

	return nil
}

// connectExisting reconnects an already-linked device and registers it in
// the active clients map.
func (m *Manager) connectExisting(ctx context.Context, userID int64, jid string) (*whatsmeow.Client, error) {
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return nil, fmt.Errorf("stored JID %q is invalid: %w", jid, err)
	}

	device, err := m.container.GetDevice(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("failed to load device: %w", err)
	}
	if device == nil {
		return nil, fmt.Errorf("linked device not found, please re-link with link_whatsapp")
	}

	client := whatsmeow.NewClient(device, m.logger)
	m.registerHandlers(client, userID)

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	m.mu.Lock()
	m.clients[userID] = client
	m.mu.Unlock()

	return client, nil
}

// getClient returns the connected client for a user, reconnecting on demand
// if necessary.
func (m *Manager) getClient(ctx context.Context, userID int64) (*whatsmeow.Client, error) {
	m.mu.Lock()
	client, ok := m.clients[userID]
	m.mu.Unlock()
	if ok {
		return client, nil
	}

	jid, ok, err := m.linkedJID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotLinked
	}

	return m.connectExisting(ctx, userID, jid)
}
