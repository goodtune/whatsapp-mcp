package wa

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
)

// LinkDevice starts or polls a device-linking session for a user.
//
// If the user already has a linked device, it reports that immediately.
// Otherwise it starts a new pairing session (or returns the state of one
// already in progress): with no phone number, a QR code is generated that
// refreshes every ~20-60 seconds; with a phone number, an 8-character
// pairing code is generated for entry into WhatsApp's
// Settings > Linked Devices > Link with phone number.
//
// Callers should re-invoke this (e.g. as a repeated MCP tool call) until
// Linked is true, refreshing the QR code each time it has gone stale.
func (m *Manager) LinkDevice(ctx context.Context, userID int64, phoneNumber string) (*LinkState, error) {
	if jid, ok, err := m.linkedJID(ctx, userID); err != nil {
		return nil, err
	} else if ok {
		return &LinkState{Linked: true, JID: jid}, nil
	}

	m.mu.Lock()
	pending, exists := m.pending[userID]
	if !exists {
		pending = &pendingLink{}
		m.pending[userID] = pending
	}
	m.mu.Unlock()

	if exists {
		state := pending.snapshot()
		return &state, nil
	}

	device := m.container.NewDevice()
	client := whatsmeow.NewClient(device, m.logger)
	m.registerHandlers(client, userID)

	pending.mu.Lock()
	pending.client = client
	pending.mu.Unlock()

	bgCtx := context.Background()

	if phoneNumber == "" {
		qrChan, err := client.GetQRChannel(bgCtx)
		if err != nil {
			m.clearPending(userID)
			return nil, fmt.Errorf("failed to start QR pairing: %w", err)
		}
		if err := client.Connect(); err != nil {
			m.clearPending(userID)
			return nil, fmt.Errorf("failed to connect: %w", err)
		}

		go func() {
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					pending.setQR(evt.Code, time.Now().Add(evt.Timeout))
				case "timeout":
					pending.setErr(fmt.Errorf("QR code timed out, call link_whatsapp again to get a new one"))
					m.clearPending(userID)
					return
				case "success":
					return
				}
			}
		}()
	} else {
		if err := client.Connect(); err != nil {
			m.clearPending(userID)
			return nil, fmt.Errorf("failed to connect: %w", err)
		}

		code, err := client.PairPhone(bgCtx, phoneNumber, true, whatsmeow.PairClientChrome, "WhatsApp MCP")
		if err != nil {
			m.clearPending(userID)
			client.Disconnect()
			return nil, fmt.Errorf("failed to request pairing code: %w", err)
		}
		pending.setPairCode(code)
	}

	return waitForFirstUpdate(pending), nil
}

func (m *Manager) clearPending(userID int64) {
	m.mu.Lock()
	delete(m.pending, userID)
	m.mu.Unlock()
}

// onLinked is invoked from the client's event handler once pairing succeeds
// and the connection stabilizes. It persists the mapping from user to JID
// and promotes the client from "pending" to "active".
func (m *Manager) onLinked(ctx context.Context, userID int64, client *whatsmeow.Client) {
	if client.Store.ID == nil {
		return
	}
	jid := client.Store.ID.String()

	if err := m.saveLinkedJID(ctx, userID, jid); err != nil {
		m.logger.Errorf("failed to persist linked device for user %d: %v", userID, err)
		return
	}

	m.mu.Lock()
	m.clients[userID] = client
	delete(m.pending, userID)
	m.mu.Unlock()
}

// waitForFirstUpdate briefly waits for the first QR code or an error to
// become available, so the initial link_whatsapp call doesn't come back
// empty while the goroutine above is still starting up.
func waitForFirstUpdate(pending *pendingLink) *LinkState {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := pending.snapshot()
		if state.QRCode != "" || state.PairCode != "" || state.Error != nil {
			return &state
		}
		time.Sleep(200 * time.Millisecond)
	}
	state := pending.snapshot()
	return &state
}
