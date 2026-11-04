package websocket

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/fasthttp/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/logf"
)

type ownedFixture struct {
	hub       *Hub
	address   string
	callbacks chan struct{}
}

type ownedListener struct {
	net.Listener
	wrap func(net.Conn) net.Conn
}

func (l ownedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil && l.wrap != nil {
		c = l.wrap(c)
	}
	return c, err
}

func newOwnedFixture(t *testing.T, auth AuthenticateFn, wrap func(net.Conn) net.Conn, beforeServe func(*Upgrade)) *ownedFixture {
	t.Helper()
	h := NewHub(logf.New(logf.Opts{}))
	require.True(t, h.Start())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &ownedFixture{hub: h, address: listener.Addr().String(), callbacks: make(chan struct{}, 16)}
	up := wire.FastHTTPUpgrader{CheckOrigin: func(*fasthttp.RequestCtx) bool { return true }}
	server := &fasthttp.Server{ConnState: h.ConnState, Handler: func(ctx *fasthttp.RequestCtx) {
		u, ok := h.BeginUpgrade(ctx.Conn())
		if !ok {
			ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		if err := up.Upgrade(ctx, func(conn *wire.Conn) {
			if beforeServe != nil {
				beforeServe(u)
			}
			u.Serve(NewUnauthenticatedClient(h, conn, auth))
			f.callbacks <- struct{}{}
		}); err != nil {
			u.Cancel()
		}
	}}
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(ownedListener{listener, wrap}) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, h.Stop(ctx))
		require.NoError(t, server.ShutdownWithContext(ctx))
		_ = listener.Close()
		select {
		case <-served:
		case <-ctx.Done():
			t.Fatal("HTTP listener did not return")
		}
	})
	return f
}

func (f *ownedFixture) dial(t *testing.T) *wire.Conn {
	t.Helper()
	d := wire.Dialer{HandshakeTimeout: 2 * time.Second}
	c, _, err := d.Dial("ws://"+f.address+"/ws", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ownedAuth(user, org uuid.UUID) AuthenticateFn {
	return func(string) (uuid.UUID, uuid.UUID, error) { return user, org, nil }
}

func ownedAuthenticate(t *testing.T, c *wire.Conn) {
	t.Helper()
	require.NoError(t, c.WriteJSON(WSMessage{Type: TypeAuth, Payload: map[string]string{"token": "synthetic"}}))
}

func assertOwnedDoneOpen(t *testing.T, h *Hub) {
	t.Helper()
	select {
	case <-h.Done():
		t.Fatal("Done closed before all accepted work joined")
	default:
	}
}

func stopOwnedSoon(h *Hub) error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	return h.Stop(ctx)
}

func TestHubOwnedStopBeforeStartAndLateAdmission(t *testing.T) {
	h := NewHub(logf.New(logf.Opts{}))
	require.NoError(t, h.Stop(context.Background()))
	require.False(t, h.Start())
	h.Run()
	require.NoError(t, h.Stop(context.Background()))
	select {
	case <-h.Done():
	default:
		t.Fatal("unused hub did not finish")
	}
	c := NewClient(h, nil, uuid.New(), uuid.New())
	require.False(t, h.Register(c))
	h.Unregister(c)
	h.BroadcastToOrg(uuid.New(), WSMessage{Type: TypePong})
	require.Zero(t, h.GetClientCount())
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	_, ok := h.BeginUpgrade(left)
	require.False(t, ok)
}

func TestHubOwnedPendingHandoffNeedsTerminalConnState(t *testing.T) {
	for _, state := range []fasthttp.ConnState{fasthttp.StateClosed, fasthttp.StateIdle} {
		t.Run(state.String(), func(t *testing.T) {
			h := NewHub(logf.New(logf.Opts{}))
			require.True(t, h.Start())
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			u, ok := h.BeginUpgrade(left)
			require.True(t, ok)
			_, duplicate := h.BeginUpgrade(left)
			require.False(t, duplicate)
			require.ErrorIs(t, stopOwnedSoon(h), context.DeadlineExceeded)
			assertOwnedDoneOpen(t, h)
			h.ConnState(left, fasthttp.StateHijacked)
			assertOwnedDoneOpen(t, h)
			h.ConnState(left, state)
			u.Cancel()
			require.NoError(t, h.Stop(context.Background()))
		})
	}
}

func TestHubOwnedRealAuthenticatedAndUnauthenticatedSocketsJoin(t *testing.T) {
	user, org := uuid.New(), uuid.New()
	f := newOwnedFixture(t, ownedAuth(user, org), nil, nil)
	authenticated, unauthenticated := f.dial(t), f.dial(t)
	ownedAuthenticate(t, authenticated)
	require.Eventually(t, func() bool { return f.hub.GetClientCount() == 1 }, time.Second, time.Millisecond)
	f.hub.BroadcastToUser(org, user, WSMessage{Type: TypePong})
	require.NoError(t, authenticated.SetReadDeadline(time.Now().Add(time.Second)))
	_, body, err := authenticated.ReadMessage()
	require.NoError(t, err)
	require.Contains(t, string(body), string(TypePong))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
	for _, peer := range []*wire.Conn{authenticated, unauthenticated} {
		require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
		_, _, err := peer.ReadMessage()
		require.Error(t, err)
	}
	for range 2 {
		select {
		case <-f.callbacks:
		case <-ctx.Done():
			t.Fatal("upgrader callback did not return")
		}
	}
	require.Zero(t, f.hub.GetClientCount())
	require.Empty(t, f.hub.OnlineUserIDs(org))
}

func TestHubOwnedAuthenticationHeldAcrossStopCannotPublish(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	auth := func(string) (uuid.UUID, uuid.UUID, error) {
		once.Do(func() { close(entered) })
		<-release
		return uuid.New(), uuid.New(), nil
	}
	adopted := make(chan *Upgrade, 1)
	f := newOwnedFixture(t, auth, nil, func(u *Upgrade) { adopted <- u })
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	peer := f.dial(t)
	ownedAuthenticate(t, peer)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authentication not entered")
	}
	// A late failed-upgrade cleanup must not release an adopted callback.
	(<-adopted).Cancel()
	require.ErrorIs(t, stopOwnedSoon(f.hub), context.DeadlineExceeded)
	assertOwnedDoneOpen(t, f.hub)
	require.ErrorIs(t, f.hub.Context().Err(), context.Canceled)
	releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
	require.Zero(t, f.hub.GetClientCount())
}

func TestHubOwnedCallbackNotYetAdoptedStaysReserved(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newOwnedFixture(t, ownedAuth(uuid.New(), uuid.New()), nil, func(*Upgrade) { close(entered); <-release })
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.dial(t)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback not entered")
	}
	require.ErrorIs(t, stopOwnedSoon(f.hub), context.DeadlineExceeded)
	assertOwnedDoneOpen(t, f.hub)
	once.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
}

type ownedWriteBlock struct {
	net.Conn
	block   atomic.Bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *ownedWriteBlock) Write(b []byte) (int, error) {
	if c.block.Load() {
		c.once.Do(func() { close(c.entered) })
		<-c.release
	}
	return c.Conn.Write(b)
}

func TestHubOwnedWriterMustJoinBeforeUpgraderReturns(t *testing.T) {
	accepted := make(chan *ownedWriteBlock, 1)
	org := uuid.New()
	f := newOwnedFixture(t, ownedAuth(uuid.New(), org), func(c net.Conn) net.Conn {
		b := &ownedWriteBlock{Conn: c, entered: make(chan struct{}), release: make(chan struct{})}
		accepted <- b
		return b
	}, nil)
	peer := f.dial(t)
	transport := <-accepted
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(transport.release) }) })
	ownedAuthenticate(t, peer)
	require.Eventually(t, func() bool { return f.hub.GetClientCount() == 1 }, time.Second, time.Millisecond)
	transport.block.Store(true)
	f.hub.BroadcastToOrg(org, WSMessage{Type: TypePong})
	select {
	case <-transport.entered:
	case <-time.After(time.Second):
		t.Fatal("writer not entered")
	}
	require.ErrorIs(t, stopOwnedSoon(f.hub), context.DeadlineExceeded)
	assertOwnedDoneOpen(t, f.hub)
	select {
	case <-f.callbacks:
		t.Fatal("callback returned while writer still used its buffer")
	default:
	}
	once.Do(func() { close(transport.release) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
	select {
	case <-f.callbacks:
	case <-ctx.Done():
		t.Fatal("writer/callback did not join")
	}
}

type ownedCloseHook struct {
	net.Conn
	closeFn func() error
}

func (c *ownedCloseHook) Close() error { return c.closeFn() }

func TestHubOwnedBlockedCloseDoesNotHideOrSerializeOtherCloses(t *testing.T) {
	h := NewHub(logf.New(logf.Opts{}))
	require.True(t, h.Start())
	a, ap := net.Pipe()
	b, bp := net.Pipe()
	defer a.Close()
	defer ap.Close()
	defer b.Close()
	defer bp.Close()
	entered, release, other := make(chan struct{}), make(chan struct{}), make(chan struct{})
	failure := errors.New("synthetic close failure")
	x := &ownedCloseHook{Conn: a, closeFn: func() error { close(entered); <-release; _ = a.Close(); return failure }}
	y := &ownedCloseHook{Conn: b, closeFn: func() error { close(other); return b.Close() }}
	_, ok := h.BeginUpgrade(x)
	require.True(t, ok)
	_, ok = h.BeginUpgrade(y)
	require.True(t, ok)
	var once sync.Once
	defer once.Do(func() { close(release) })
	require.ErrorIs(t, stopOwnedSoon(h), context.DeadlineExceeded)
	select {
	case <-entered:
	default:
		t.Fatal("blocked close not started")
	}
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("one close prevented another close")
	}
	h.ConnState(x, fasthttp.StateClosed)
	h.ConnState(y, fasthttp.StateClosed)
	assertOwnedDoneOpen(t, h)
	once.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorIs(t, h.Stop(ctx), failure)
	require.ErrorIs(t, h.Stop(ctx), failure)
}

func TestHubOwnedClosePanicRemainsAReportedFailure(t *testing.T) {
	h := NewHub(logf.New(logf.Opts{}))
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	x := &ownedCloseHook{Conn: a, closeFn: func() error { panic("synthetic") }}
	_, ok := h.BeginUpgrade(x)
	require.True(t, ok)
	require.ErrorIs(t, stopOwnedSoon(h), context.DeadlineExceeded)
	h.ConnState(x, fasthttp.StateClosed)
	require.ErrorContains(t, h.Stop(context.Background()), "close panicked")
}

type ownedWriteFailure struct{ net.Conn }

func (c ownedWriteFailure) Write([]byte) (int, error) {
	return 0, errors.New("synthetic handshake write failure")
}

func TestHubOwnedFailedHandshakeNeverLeaksReservation(t *testing.T) {
	f := newOwnedFixture(t, ownedAuth(uuid.New(), uuid.New()), func(c net.Conn) net.Conn { return ownedWriteFailure{c} }, nil)
	d := wire.Dialer{HandshakeTimeout: time.Second}
	_, _, err := d.Dial("ws://"+f.address+"/ws", nil)
	require.Error(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
	select {
	case <-f.callbacks:
		t.Fatal("failed handshake invoked callback")
	default:
	}
}

func TestHubOwnedSynchronousUpgradeFailureAndLater503(t *testing.T) {
	f := newOwnedFixture(t, ownedAuth(uuid.New(), uuid.New()), nil, nil)
	client := http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + f.address + "/ws")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
	d := wire.Dialer{HandshakeTimeout: time.Second}
	_, response, err = d.Dial("ws://"+f.address+"/ws", nil)
	require.Error(t, err)
	require.NotNil(t, response)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	if response.Body != nil {
		_ = response.Body.Close()
	}
}

func TestHubOwnedConcurrentContactBroadcastAndUnregister(t *testing.T) {
	h := NewHub(logf.New(logf.Opts{}))
	require.True(t, h.Start())
	org, contact := uuid.New(), uuid.New()
	c := NewClient(h, nil, uuid.New(), org)
	require.True(t, h.Register(c))
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 300 {
			c.handleSetContact(map[string]string{"contact_id": contact.String()})
			c.handleSetContact(map[string]string{"contact_id": ""})
		}
	}()
	go func() {
		defer wg.Done()
		for range 300 {
			h.BroadcastToContact(org, contact, WSMessage{Type: TypePong})
			c.sendPong()
		}
	}()
	go func() {
		defer wg.Done()
		for range c.SendChan() {
		}
	}()
	h.Unregister(c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, h.Stop(ctx))
	wg.Wait()
	require.False(t, h.IsUserOnline(org, c.userID))
}

func TestHubOwnedConcurrentStopClosesTransportOnce(t *testing.T) {
	h := NewHub(logf.New(logf.Opts{}))
	require.True(t, h.Start())
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	var calls atomic.Int32
	x := &ownedCloseHook{Conn: a, closeFn: func() error { calls.Add(1); return a.Close() }}
	_, ok := h.BeginUpgrade(x)
	require.True(t, ok)
	var wg sync.WaitGroup
	wg.Add(8)
	results := make(chan error, 8)
	for range 8 {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- h.Stop(ctx)
		}()
	}
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	h.ConnState(x, fasthttp.StateClosed)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestHubOwnedAuthenticationFailureUsesSafeControlWrite(t *testing.T) {
	f := newOwnedFixture(t, func(string) (uuid.UUID, uuid.UUID, error) { return uuid.Nil, uuid.Nil, errors.New("synthetic refusal") }, nil, nil)
	peer := f.dial(t)
	ownedAuthenticate(t, peer)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := peer.ReadMessage()
	require.Error(t, err)
	var closed *wire.CloseError
	require.ErrorAs(t, err, &closed)
	require.Equal(t, wire.ClosePolicyViolation, closed.Code)
	require.True(t, strings.Contains(closed.Text, "authentication failed"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, f.hub.Stop(ctx))
}
