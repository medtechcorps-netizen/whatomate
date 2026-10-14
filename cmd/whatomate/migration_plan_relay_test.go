package main

import (
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// migrationPlanRelay forwards unexamined PostgreSQL bytes. Only the first
// accepted connection after arm is held before upstream connection establishment.
// The already-held migration connection keeps forwarding throughout the test.
type migrationPlanRelay struct {
	listener    net.Listener
	upstream    string
	dsn         string
	mu          sync.Mutex
	connections map[net.Conn]bool
	failures    []string
	armed       bool
	closing     bool
	attempts    int
	accepted    chan struct{}
	release     chan struct{}
	stop        chan struct{}
	acceptDone  chan struct{}
	handlers    sync.WaitGroup
	releaseOnce sync.Once
}

func newMigrationPlanRelay(t *testing.T, dsn string) *migrationPlanRelay {
	t.Helper()
	u, err := url.Parse(dsn)
	planPGOK(t, err, "parse owned relay URL")
	require.Contains(t, []string{"127.0.0.1", "localhost", "::1"}, u.Hostname())
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(u.Hostname(), "0"))
	planPGOK(t, err, "listen on owned loopback relay")
	_, relayPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal("owned relay address unavailable")
	}
	r := &migrationPlanRelay{listener: listener, upstream: net.JoinHostPort(u.Hostname(), port), connections: map[net.Conn]bool{}, accepted: make(chan struct{}), release: make(chan struct{}), stop: make(chan struct{}), acceptDone: make(chan struct{})}
	// Preserve username/password, database, query settings and TLS hostname.
	u.Host = net.JoinHostPort(u.Hostname(), relayPort)
	r.dsn = u.String()
	t.Cleanup(func() { r.close(t) })
	go r.serve()
	return r
}

func (r *migrationPlanRelay) serve() {
	defer close(r.acceptDone)
	for {
		client, err := r.listener.Accept()
		if err != nil {
			r.failure("accept", err)
			return
		}
		r.mu.Lock()
		if r.closing {
			r.mu.Unlock()
			_ = client.Close()
			return
		}
		r.attempts++
		r.connections[client] = true
		held := r.armed
		if held {
			r.armed = false
			close(r.accepted)
		}
		r.handlers.Add(1)
		r.mu.Unlock()
		go r.forward(client, held)
	}
}

func (r *migrationPlanRelay) forward(client net.Conn, held bool) {
	defer r.handlers.Done()
	var server net.Conn
	defer func() {
		_ = client.Close()
		if server != nil {
			_ = server.Close()
		}
		r.mu.Lock()
		delete(r.connections, client)
		delete(r.connections, server)
		r.mu.Unlock()
	}()
	if held {
		select {
		case <-r.release:
		case <-r.stop:
			return
		}
	}
	var err error
	server, err = net.DialTimeout("tcp", r.upstream, 5*time.Second)
	if err != nil {
		r.failure("upstream-connect", err)
		return
	}
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.connections[server] = true
	r.mu.Unlock()
	finished := make(chan error, 2)
	go func() { _, err := io.Copy(server, client); finished <- err }()
	go func() { _, err := io.Copy(client, server); finished <- err }()
	first := <-finished
	// A terminal stream ends the whole proxied connection; closing both handles
	// unblocks the other copy. The handler waits for both goroutines before exit.
	_ = client.Close()
	_ = server.Close()
	second := <-finished
	r.failure("forward", first)
	r.failure("forward", second)
}

func (r *migrationPlanRelay) failure(code string, err error) {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closing {
		r.failures = append(r.failures, code)
	}
}

func (r *migrationPlanRelay) arm(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Equal(t, 1, r.attempts, "only the marked held connection exists before observation")
	require.False(t, r.armed)
	r.armed = true
}

func (r *migrationPlanRelay) proceed() { r.releaseOnce.Do(func() { close(r.release) }) }

func (r *migrationPlanRelay) close(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	r.closing = true
	close(r.stop)
	for conn := range r.connections {
		_ = conn.Close()
	}
	r.mu.Unlock()
	_ = r.listener.Close()
	finished := make(chan struct{})
	go func() { <-r.acceptDone; r.handlers.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("owned relay goroutines did not finish")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Empty(t, r.connections, "all relay handles disposed")
	require.Empty(t, r.failures, "relay only reports fixed transport stage codes")
}
