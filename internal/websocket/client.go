package websocket

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/google/uuid"
)

const (
	// Time allowed to write a message to the peer
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer
	maxMessageSize = 512

	// Time allowed to authenticate after connection
	authTimeout = 5 * time.Second
)

// AuthenticateFn validates a JWT token and returns user ID and organization ID.
type AuthenticateFn func(token string) (uuid.UUID, uuid.UUID, error)

// Client represents a WebSocket client connection
type Client struct {
	hub            *Hub
	mu             sync.RWMutex
	closed         bool
	runStarted     bool
	upgrade        *Upgrade
	closeTransport func() error
	closeOnce      sync.Once
	closeErr       error

	// The websocket connection
	conn *websocket.Conn

	// Buffered channel of outbound messages
	send chan []byte

	// User information (set after authentication)
	userID         uuid.UUID
	organizationID uuid.UUID

	// Whether the client has authenticated
	authenticated bool

	// Function to validate JWT tokens
	authFn AuthenticateFn

	// Current contact being viewed (nil if none)
	currentContact *uuid.UUID
}

// Run owns both pumps. Returning from an upgrader callback before the writer
// exits would let the upgrader recycle a buffer the writer can still use.
func (c *Client) Run() {
	c.mu.Lock()
	if c.runStarted {
		c.mu.Unlock()
		return
	}
	c.runStarted = true
	c.mu.Unlock()
	written := make(chan struct{})
	go func() { defer close(written); c.WritePump() }()
	c.ReadPump()
	<-written
}

func (c *Client) identity() (uuid.UUID, uuid.UUID) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.userID, c.organizationID
}

func (c *Client) isAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authenticated
}

func (c *Client) closeSend() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.send)
	}
}

func (c *Client) enqueue(data []byte) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return false
	}
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

func (c *Client) viewsContact(id uuid.UUID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return id == uuid.Nil || c.currentContact == nil || *c.currentContact == id
}

func (c *Client) closeNetwork() error {
	c.closeOnce.Do(func() {
		c.closeErr = closeWebSocketTransport(func() error {
			c.mu.RLock()
			closeTransport, conn := c.closeTransport, c.conn
			c.mu.RUnlock()
			if closeTransport != nil {
				return closeTransport()
			}
			if conn != nil {
				// Standalone clients can also use a fasthttp wrapper whose Close is a
				// no-op. Deadlines unblock both pumps before that callback returns.
				_ = conn.NetConn().SetDeadline(time.Now())
				return conn.Close()
			}
			return nil
		})
	})
	return c.closeErr
}

func closeWebSocketTransport(closeConn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("websocket transport close panicked")
		}
	}()
	return closeConn()
}

// NewClient creates a new unauthenticated Client instance.
// The client must authenticate via a message-based auth flow before it can
// send/receive application messages.
func NewClient(hub *Hub, conn *websocket.Conn, userID, orgID uuid.UUID) *Client {
	return &Client{
		hub:            hub,
		conn:           conn,
		send:           make(chan []byte, 256),
		userID:         userID,
		organizationID: orgID,
		authenticated:  userID != uuid.Nil, // pre-authenticated if userID is set (tests)
	}
}

// NewUnauthenticatedClient creates a client that requires message-based authentication.
func NewUnauthenticatedClient(hub *Hub, conn *websocket.Conn, authFn AuthenticateFn) *Client {
	return &Client{
		hub:    hub,
		conn:   conn,
		send:   make(chan []byte, 256),
		authFn: authFn,
	}
}

// ReadPump pumps messages from the websocket connection to the hub
func (c *Client) ReadPump() {
	defer func() {
		if r := recover(); r != nil {
			userID, _ := c.identity()
			c.hub.log.Error("Recovered from panic in ReadPump", "error", r, "user_id", userID)
		}
		c.hub.Unregister(c)
		_ = c.closeNetwork()
	}()

	c.conn.SetReadLimit(maxMessageSize)

	// If not yet authenticated, enforce auth timeout for the first message
	if !c.isAuthenticated() {
		_ = c.conn.SetReadDeadline(time.Now().Add(authTimeout))

		_, message, err := c.conn.ReadMessage()
		if err != nil {
			c.hub.log.Warn("WebSocket auth timeout or read error", "error", err)
			return
		}

		if !c.handleAuthMessage(message) {
			// Control writes are explicitly safe alongside the writer pump.
			_ = c.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "authentication failed"), time.Now().Add(writeWait))
			return
		}
	}

	// Normal read loop (authenticated)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				userID, _ := c.identity()
				c.hub.log.Error("WebSocket read error", "error", err, "user_id", userID)
			}
			break
		}

		c.handleMessage(message)
	}
}

// WritePump pumps messages from the hub to the websocket connection
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		if r := recover(); r != nil {
			userID, _ := c.identity()
			c.hub.log.Error("Recovered from panic in WritePump", "error", r, "user_id", userID)
		}
		ticker.Stop()
		_ = c.closeNetwork()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok || c.conn == nil {
				// Hub closed the channel or connection is gone — exit immediately.
				// Don't try to write a close frame; ReadPump may have already
				// closed the underlying connection.
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))

			// Only forward messages if authenticated
			if !c.isAuthenticated() {
				continue
			}

			// Send each message as a separate WebSocket frame
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

			// Send any queued messages as separate frames
			n := len(c.send)
			for i := 0; i < n; i++ {
				queued, open := <-c.send
				if !open {
					return
				}
				if err := c.conn.WriteMessage(websocket.TextMessage, queued); err != nil {
					return
				}
			}

		case <-ticker.C:
			if c.conn == nil {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleAuthMessage processes the first message which must be an auth message.
// Returns true if authentication succeeded.
func (c *Client) handleAuthMessage(data []byte) bool {
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.hub.log.Error("Failed to unmarshal auth message", "error", err)
		return false
	}

	if msg.Type != TypeAuth {
		c.hub.log.Warn("Expected auth message, got", "type", msg.Type)
		return false
	}

	payloadBytes, err := json.Marshal(msg.Payload)
	if err != nil {
		return false
	}

	var authPayload AuthPayload
	if err := json.Unmarshal(payloadBytes, &authPayload); err != nil {
		return false
	}

	if authPayload.Token == "" || c.authFn == nil {
		return false
	}

	userID, orgID, err := c.authFn(authPayload.Token)
	if err != nil {
		c.hub.log.Warn("WebSocket auth failed", "error", err)
		return false
	}

	// Identity publication and the shutdown admission freeze share a lock.
	if !c.hub.registerClient(c, userID, orgID) {
		return false
	}

	c.hub.log.Info("WebSocket client authenticated via message",
		"user_id", userID, "org_id", orgID)
	return true
}

// handleMessage processes incoming messages from the client
func (c *Client) handleMessage(data []byte) {
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.hub.log.Error("Failed to unmarshal client message", "error", err)
		return
	}

	switch msg.Type {
	case TypeSetContact:
		c.handleSetContact(msg.Payload)
	case TypePing:
		c.sendPong()
	}
}

// handleSetContact updates the client's current contact
func (c *Client) handleSetContact(payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	var setContact SetContactPayload
	if err := json.Unmarshal(data, &setContact); err != nil {
		return
	}

	if setContact.ContactID == "" {
		c.mu.Lock()
		c.currentContact = nil
		userID := c.userID
		c.mu.Unlock()
		c.hub.log.Debug("Client cleared current contact", "user_id", userID)
	} else {
		contactID, err := uuid.Parse(setContact.ContactID)
		if err != nil {
			return
		}
		c.mu.Lock()
		c.currentContact = &contactID
		userID := c.userID
		c.mu.Unlock()
		c.hub.log.Debug("Client set current contact",
			"user_id", userID,
			"contact_id", contactID)
	}
}

// SendChan returns the client's send channel for use in tests.
func (c *Client) SendChan() <-chan []byte {
	return c.send
}

// sendPong sends a pong response to the client
func (c *Client) sendPong() {
	msg := WSMessage{Type: TypePong}
	data, _ := json.Marshal(msg)
	c.enqueue(data)
}
