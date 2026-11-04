package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/logf"
)

// Hub maintains the set of active clients and broadcasts messages to them
type Hub struct {
	// clients maps organization ID -> user ID -> set of clients (supports multiple tabs)
	clients map[uuid.UUID]map[uuid.UUID]map[*Client]struct{}

	// broadcast channel for messages
	broadcast chan BroadcastMessage

	// mutex for thread-safe access to clients map
	mu       sync.RWMutex
	upgrades map[net.Conn]*Upgrade
	ctx      context.Context
	cancel   context.CancelFunc
	stop     chan struct{}
	done     chan struct{}
	started  bool
	running  bool
	stopping bool
	finished bool
	closing  int
	closeErr error

	// logger
	log logf.Logger
}

// NewHub creates a new Hub instance
func NewHub(log logf.Logger) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		clients:   make(map[uuid.UUID]map[uuid.UUID]map[*Client]struct{}),
		broadcast: make(chan BroadcastMessage, 256),
		upgrades:  make(map[net.Conn]*Upgrade),
		ctx:       ctx,
		cancel:    cancel,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		log:       log,
	}
}

// Start reserves the loop before launching it, so Stop also joins a loop that
// has not yet been scheduled. A Hub is single-use.
func (h *Hub) Start() bool {
	if !h.beginRun() {
		return false
	}
	go h.run()
	return true
}

// Run runs the loop in the caller. Production owners should use Start so the
// launch itself is included in the hub's shutdown accounting.
func (h *Hub) Run() {
	if h.beginRun() {
		h.run()
	}
}

func (h *Hub) beginRun() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started || h.stopping {
		return false
	}
	h.started, h.running = true, true
	return true
}

func (h *Hub) run() {
	defer func() {
		h.mu.Lock()
		h.running = false
		h.finishLocked()
		h.mu.Unlock()
	}()
	for {
		select {
		case <-h.stop:
			return
		case message := <-h.broadcast:
			h.broadcastMessage(message)
		}
	}
}

// registerClient publishes identity and membership under the admission lock.
// Client state always follows the hub -> client lock order.
func (h *Hub) registerClient(client *Client, userID, orgID uuid.UUID) bool {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return false
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		h.mu.Unlock()
		return false
	}
	client.userID, client.organizationID, client.authenticated = userID, orgID, true
	client.mu.Unlock()

	orgClients, ok := h.clients[orgID]
	if !ok {
		orgClients = make(map[uuid.UUID]map[*Client]struct{})
		h.clients[orgID] = orgClients
	}

	userClients, ok := orgClients[userID]
	if !ok {
		userClients = make(map[*Client]struct{})
		orgClients[userID] = userClients
	}

	// Add this client to the set (allows multiple tabs)
	userClients[client] = struct{}{}
	connections, total := len(userClients), h.countClients()
	h.mu.Unlock()
	h.log.Info("WebSocket client registered",
		"user_id", userID,
		"org_id", orgID,
		"user_connections", connections,
		"total_clients", total)
	return true
}

// unregisterClient removes a client from the hub
func (h *Hub) unregisterClient(client *Client) {
	h.mu.Lock()
	userID, orgID := client.identity()

	if orgClients, ok := h.clients[orgID]; ok {
		if userClients, ok := orgClients[userID]; ok {
			if _, exists := userClients[client]; exists {
				delete(userClients, client)

				// Clean up empty user map
				if len(userClients) == 0 {
					delete(orgClients, userID)
				}

				// Clean up empty org map
				if len(orgClients) == 0 {
					delete(h.clients, orgID)
				}
			}
		}
	}
	client.closeSend()
	total, stopping := h.countClients(), h.stopping
	h.mu.Unlock()
	if stopping {
		return
	}
	h.log.Info("WebSocket client unregistered",
		"user_id", userID,
		"org_id", orgID,
		"total_clients", total)
}

// broadcastMessage sends a message to all relevant clients
func (h *Hub) broadcastMessage(msg BroadcastMessage) {
	// Payload marshalling and logging may call external code. Neither may hold
	// the admission lock and prevent Stop from freezing new work.
	data, err := json.Marshal(msg.Message)
	if err != nil {
		h.log.Error("Failed to marshal broadcast message", "error", err)
		return
	}
	h.mu.RLock()
	if h.stopping {
		h.mu.RUnlock()
		return
	}
	orgClients, ok := h.clients[msg.OrgID]
	if !ok {
		h.mu.RUnlock()
		return
	}
	dropped := 0

	// If UserID is specified, only send to that user's clients
	if msg.UserID != uuid.Nil {
		userClients, ok := orgClients[msg.UserID]
		if !ok {
			h.mu.RUnlock()
			return
		}
		for client := range userClients {
			if !client.enqueue(data) {
				dropped++
			}
		}
		h.mu.RUnlock()
		if dropped > 0 {
			h.log.Warn("Client send buffers full, skipping", "clients", dropped)
		}
		return
	}

	// Iterate through all users in the organization
	for _, userClients := range orgClients {
		// Iterate through all clients (tabs) for each user
		for client := range userClients {
			// If ContactID is specified, only send to clients viewing that contact
			if !client.viewsContact(msg.ContactID) {
				continue
			}

			if !client.enqueue(data) {
				dropped++
			}
		}
	}
	h.mu.RUnlock()
	if dropped > 0 {
		h.log.Warn("Client send buffers full, skipping", "clients", dropped)
	}
}

// Broadcast sends a message to the broadcast channel
func (h *Hub) Broadcast(msg BroadcastMessage) {
	h.mu.RLock()
	if h.stopping {
		h.mu.RUnlock()
		return
	}
	dropped := false
	select {
	case h.broadcast <- msg:
	default:
		dropped = true
	}
	h.mu.RUnlock()
	if dropped {
		h.log.Warn("Broadcast channel full, dropping message")
	}
}

// BroadcastToOrg sends a message to all clients in an organization
func (h *Hub) BroadcastToOrg(orgID uuid.UUID, msg WSMessage) {
	h.Broadcast(BroadcastMessage{
		OrgID:   orgID,
		Message: msg,
	})
}

// BroadcastToContact sends a message to clients viewing a specific contact
func (h *Hub) BroadcastToContact(orgID, contactID uuid.UUID, msg WSMessage) {
	h.Broadcast(BroadcastMessage{
		OrgID:     orgID,
		ContactID: contactID,
		Message:   msg,
	})
}

// BroadcastToUser sends a message to a specific user
func (h *Hub) BroadcastToUser(orgID, userID uuid.UUID, msg WSMessage) {
	h.Broadcast(BroadcastMessage{
		OrgID:   orgID,
		UserID:  userID,
		Message: msg,
	})
}

// BroadcastToUsers sends a message to multiple users
func (h *Hub) BroadcastToUsers(orgID uuid.UUID, userIDs []uuid.UUID, msg WSMessage) {
	for _, userID := range userIDs {
		h.BroadcastToUser(orgID, userID, msg)
	}
}

// countClients returns the total number of connected clients
func (h *Hub) countClients() int {
	count := 0
	for _, orgClients := range h.clients {
		for _, userClients := range orgClients {
			count += len(userClients)
		}
	}
	return count
}

// GetClientCount returns the number of connected clients (thread-safe)
func (h *Hub) GetClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.countClients()
}

// IsUserOnline returns true if the user has at least one active WebSocket connection.
func (h *Hub) IsUserOnline(orgID, userID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if orgClients, ok := h.clients[orgID]; ok {
		if userClients, ok := orgClients[userID]; ok {
			return len(userClients) > 0
		}
	}
	return false
}

// OnlineUserIDs returns every user ID in the org that has at least one
// active WebSocket connection. Used by ListUsers for the online-only
// filter and the online-count badge.
func (h *Hub) OnlineUserIDs(orgID uuid.UUID) []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()

	orgClients, ok := h.clients[orgID]
	if !ok {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(orgClients))
	for uid, clients := range orgClients {
		if len(clients) > 0 {
			ids = append(ids, uid)
		}
	}
	return ids
}

// OnlineUserIDsWithinLimit copies a complete connected-user set or refuses it.
// It never returns a truncated audience that a caller could mistake for complete.
func (h *Hub) OnlineUserIDsWithinLimit(orgID uuid.UUID, limit int) ([]uuid.UUID, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if limit <= 0 {
		return nil, false
	}
	var ids []uuid.UUID
	for userID, clients := range h.clients[orgID] {
		if len(clients) == 0 {
			continue
		}
		if len(ids) == limit {
			return nil, false
		}
		ids = append(ids, userID)
	}
	return ids, true
}

// FilterOnlineUsers returns only the user IDs that have active WebSocket connections.
func (h *Hub) FilterOnlineUsers(orgID uuid.UUID, userIDs []uuid.UUID) []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()

	orgClients, ok := h.clients[orgID]
	if !ok {
		return nil
	}

	online := make([]uuid.UUID, 0, len(userIDs))
	for _, uid := range userIDs {
		if userClients, ok := orgClients[uid]; ok && len(userClients) > 0 {
			online = append(online, uid)
		}
	}
	return online
}

// Register publishes a pre-authenticated client unless admission has stopped.
func (h *Hub) Register(client *Client) bool {
	userID, orgID := client.identity()
	return h.registerClient(client, userID, orgID)
}

// Unregister is safe after the loop has stopped and never waits for that loop.
func (h *Hub) Unregister(client *Client) {
	h.unregisterClient(client)
}

// Upgrade owns the HTTP-to-WebSocket handoff and the original transport. The
// default fasthttp hijack wrapper deliberately makes Close a no-op, so closing
// that wrapper alone cannot unblock the read pump.
type Upgrade struct {
	hub       *Hub
	conn      net.Conn
	adopted   bool // protected by hub.mu
	finished  bool // protected by hub.mu
	closeOnce sync.Once
	closeErr  error
}

func (h *Hub) BeginUpgrade(conn net.Conn) (*Upgrade, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping || conn == nil || h.upgrades[conn] != nil {
		return nil, false
	}
	u := &Upgrade{hub: h, conn: conn}
	h.upgrades[conn] = u
	return u, true
}

// ConnState settles reservations for upgrades whose callback never starts
// because response writing, flushing, or setting the transport deadline failed.
// A hijacked connection stays owned until Serve has joined both client pumps.
func (h *Hub) ConnState(conn net.Conn, state fasthttp.ConnState) {
	if state != fasthttp.StateClosed && state != fasthttp.StateIdle {
		return
	}
	h.mu.Lock()
	if u := h.upgrades[conn]; u != nil && !u.adopted {
		h.releaseUpgradeLocked(u)
	}
	h.mu.Unlock()
}

// Cancel is only for a failed synchronous Upgrade call. It cannot release an
// already adopted callback while that callback is still running.
func (u *Upgrade) Cancel() {
	u.hub.mu.Lock()
	if !u.adopted {
		u.hub.releaseUpgradeLocked(u)
	}
	u.hub.mu.Unlock()
}

// Serve runs inside the actual upgrader callback and joins the writer before
// returning; the upgrader may recycle its write buffer as soon as we return.
func (u *Upgrade) Serve(client *Client) {
	h := u.hub
	h.mu.Lock()
	if u.finished || u.adopted {
		h.mu.Unlock()
		return
	}
	u.adopted = true
	stopping := h.stopping
	client.mu.Lock()
	client.closeTransport = u.close
	client.upgrade = u
	client.mu.Unlock()
	h.mu.Unlock()
	defer func() {
		err := u.close()
		h.mu.Lock()
		h.recordCloseErrorLocked(err)
		h.releaseUpgradeLocked(u)
		h.mu.Unlock()
	}()
	if stopping {
		return
	}
	client.Run()
}

func (u *Upgrade) close() error {
	u.closeOnce.Do(func() { u.closeErr = closeWebSocketTransport(u.conn.Close) })
	return u.closeErr
}

func (h *Hub) releaseUpgradeLocked(u *Upgrade) {
	if u.finished {
		return
	}
	u.finished = true
	if h.upgrades[u.conn] == u {
		delete(h.upgrades, u.conn)
	}
	h.finishLocked()
}

func (h *Hub) recordCloseErrorLocked(err error) {
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(h.closeErr, err) {
		h.closeErr = errors.Join(h.closeErr, err)
	}
}

func (h *Hub) finishLocked() {
	if h.stopping && !h.finished && !h.running && len(h.upgrades) == 0 && h.closing == 0 {
		h.finished = true
		close(h.done)
	}
}

func (h *Hub) requestStop() {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return
	}
	h.stopping = true
	h.cancel()
	close(h.stop)
	var closes []func() error
	for _, u := range h.upgrades {
		closes = append(closes, u.close)
	}
	for _, org := range h.clients {
		for _, users := range org {
			for client := range users {
				client.closeSend()
				client.mu.RLock()
				legacy := client.upgrade == nil && client.conn != nil
				client.mu.RUnlock()
				if legacy {
					closes = append(closes, client.closeNetwork)
				}
			}
		}
	}
	h.clients = make(map[uuid.UUID]map[uuid.UUID]map[*Client]struct{})
	h.closing += len(closes)
	h.finishLocked()
	h.mu.Unlock()
	// Every close has ownership before launch. One blocked transport cannot
	// prevent closing the other transports or returning the caller's deadline.
	for _, closeConn := range closes {
		go func() {
			err := closeConn()
			h.mu.Lock()
			h.recordCloseErrorLocked(err)
			h.closing--
			h.finishLocked()
			h.mu.Unlock()
		}()
	}
}

func (h *Hub) Stop(ctx context.Context) error {
	h.requestStop()
	select {
	case <-h.done:
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.closeErr
	case <-ctx.Done():
		h.mu.RLock()
		defer h.mu.RUnlock()
		return errors.Join(ctx.Err(), h.closeErr)
	}
}

func (h *Hub) Done() <-chan struct{}    { return h.done }
func (h *Hub) Context() context.Context { return h.ctx }
