package queue

import "time"

const (
	// WorkerHeartbeatKey is shared by embedded and standalone worker processes.
	// The value is an RFC3339Nano UTC timestamp and the TTL makes a stopped
	// worker fail closed without requiring cleanup.
	WorkerHeartbeatKey = "rereply:workers:heartbeat"
	WorkerHeartbeatTTL = 90 * time.Second

	workerHeartbeatInterval = 15 * time.Second
)

// WorkerHeartbeatInterval controls how often a live worker renews the shared
// readiness lease.
func WorkerHeartbeatInterval() time.Duration {
	return workerHeartbeatInterval
}
