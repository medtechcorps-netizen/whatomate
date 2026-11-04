package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/websocket"
)

// StartOwnedWebSocketTestHub reserves the loop before launch and joins it during
// cleanup. It is exported only in the test build for handlers_test callers.
func StartOwnedWebSocketTestHub(t *testing.T, hub *websocket.Hub) {
	t.Helper()
	if !hub.Start() {
		t.Fatal("test WebSocket hub was already started or stopped")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := hub.Stop(ctx); err != nil {
			t.Errorf("test WebSocket hub did not join: %v", err)
		}
	})
}
