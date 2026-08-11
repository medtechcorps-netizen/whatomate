package metarelay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

type sequenceReviewResolver struct {
	mu        sync.Mutex
	answers   [][]netip.Addr
	err       error
	calls     int
	lastHost  string
	lastProto string
}

func (r *sequenceReviewResolver) LookupNetIP(
	_ context.Context,
	network, host string,
) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastHost = host
	r.lastProto = network
	if r.err != nil {
		return nil, r.err
	}
	if len(r.answers) == 0 {
		return nil, nil
	}
	index := r.calls - 1
	if index >= len(r.answers) {
		index = len(r.answers) - 1
	}
	return append([]netip.Addr(nil), r.answers[index]...), nil
}

func TestReviewPublicDialRejectsAnyNonPublicDNSAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		addresses []netip.Addr
	}{
		{name: "private", addresses: []netip.Addr{netip.MustParseAddr("10.0.0.7")}},
		{name: "cgnat", addresses: []netip.Addr{netip.MustParseAddr("100.64.0.7")}},
		{name: "link local", addresses: []netip.Addr{netip.MustParseAddr("169.254.1.7")}},
		{name: "documentation", addresses: []netip.Addr{netip.MustParseAddr("203.0.113.7")}},
		{name: "multicast", addresses: []netip.Addr{netip.MustParseAddr("239.1.1.7")}},
		{name: "future use", addresses: []netip.Addr{netip.MustParseAddr("240.0.0.7")}},
		{name: "ipv6 private", addresses: []netip.Addr{netip.MustParseAddr("fd00::7")}},
		{name: "ipv6 link local", addresses: []netip.Addr{netip.MustParseAddr("fe80::7")}},
		{name: "ipv6 deprecated site local", addresses: []netip.Addr{netip.MustParseAddr("fec0::7")}},
		{name: "ipv6 translation", addresses: []netip.Addr{netip.MustParseAddr("64:ff9b::7f00:1")}},
		{name: "ipv6 documentation", addresses: []netip.Addr{netip.MustParseAddr("2001:db8::7")}},
		{
			name: "mixed public private",
			addresses: []netip.Addr{
				netip.MustParseAddr("8.8.8.8"),
				netip.MustParseAddr("10.0.0.8"),
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolver := &sequenceReviewResolver{answers: [][]netip.Addr{testCase.addresses}}
			dialCalls := 0
			dial := reviewPublicDialContext(
				resolver,
				func(context.Context, string, string) (net.Conn, error) {
					dialCalls++
					return nil, errors.New("unexpected dial")
				},
				false,
			)
			if _, err := dial(context.Background(), "tcp", "review.example:443"); err == nil {
				t.Fatal("non-public DNS answer was accepted")
			}
			if resolver.calls != 1 || resolver.lastHost != "review.example" ||
				resolver.lastProto != "ip" {
				t.Fatalf("resolver state = calls:%d host:%q network:%q", resolver.calls, resolver.lastHost, resolver.lastProto)
			}
			if dialCalls != 0 {
				t.Fatalf("network dial calls=%d, want zero", dialCalls)
			}
		})
	}
}

func TestReviewPublicDialResolvesEveryDialAndStopsRebinding(t *testing.T) {
	resolver := &sequenceReviewResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("8.8.8.8")},
		{netip.MustParseAddr("127.0.0.1")},
	}}
	errReachedDial := errors.New("reached pinned dial")
	dialCalls := 0
	var dialAddress string
	dial := reviewPublicDialContext(
		resolver,
		func(_ context.Context, _, address string) (net.Conn, error) {
			dialCalls++
			dialAddress = address
			return nil, errReachedDial
		},
		false,
	)
	if _, err := dial(context.Background(), "tcp", "review.example:443"); !errors.Is(err, errReachedDial) {
		t.Fatalf("first dial error=%v, want pinned dial error", err)
	}
	if dialAddress != "8.8.8.8:443" {
		t.Fatalf("dial address=%q, want DNS-pinned public address", dialAddress)
	}
	if _, err := dial(context.Background(), "tcp", "review.example:443"); err == nil ||
		errors.Is(err, errReachedDial) {
		t.Fatalf("rebound private address reached network dial: %v", err)
	}
	if resolver.calls != 2 || dialCalls != 1 {
		t.Fatalf("resolver calls=%d dial calls=%d, want 2/1", resolver.calls, dialCalls)
	}
}

func TestReviewTestLoopbackExceptionRequiresLiteralTarget(t *testing.T) {
	resolver := &sequenceReviewResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("127.0.0.1")},
	}}
	dialCalls := 0
	dial := reviewPublicDialContext(
		resolver,
		func(context.Context, string, string) (net.Conn, error) {
			dialCalls++
			return nil, errors.New("unexpected dial")
		},
		true,
	)
	if _, err := dial(context.Background(), "tcp", "localhost:8080"); err == nil {
		t.Fatal("DNS-resolved loopback was accepted by the test exception")
	}
	if dialCalls != 0 {
		t.Fatalf("network dial calls=%d, want zero", dialCalls)
	}
}

func TestReviewTransportAllowsOnlyExplicitTestHTTPLoopback(t *testing.T) {
	baseCalls := 0
	transport := &reviewEndpointTransport{
		allowTestLoopback: true,
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			baseCalls++
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
	}

	for _, testCase := range []struct {
		url      string
		wantCall bool
	}{
		{url: "http://127.0.0.1:8080/review", wantCall: true},
		{url: "http://[::1]:8080/review", wantCall: true},
		{url: "http://8.8.8.8/review", wantCall: false},
		{url: "http://localhost:8080/review", wantCall: false},
		{url: "ftp://127.0.0.1/review", wantCall: false},
		{url: "https://review.example/review", wantCall: true},
	} {
		before := baseCalls
		request, err := http.NewRequest(http.MethodGet, testCase.url, nil)
		if err != nil {
			t.Fatalf("new request %s: %v", testCase.url, err)
		}
		_, err = transport.RoundTrip(request)
		called := baseCalls == before+1
		if called != testCase.wantCall {
			t.Errorf("%s base called=%v, want %v (error=%v)", testCase.url, called, testCase.wantCall, err)
		}
		if !testCase.wantCall && err == nil {
			t.Errorf("%s was accepted", testCase.url)
		}
	}

	strict := &reviewEndpointTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("strict transport was reached")
		}),
	}
	request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/review", nil)
	if _, err := strict.RoundTrip(request); err == nil || strings.Contains(err.Error(), "was reached") {
		t.Fatalf("strict transport accepted insecure loopback: %v", err)
	}
}

func TestReviewTransportRetainsTLSVerificationAndDisablesProxy(t *testing.T) {
	roundTripper := newReviewEndpointTransport(
		&sequenceReviewResolver{},
		func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("not dialed")
		},
		false,
	)
	wrapper, ok := roundTripper.(*reviewEndpointTransport)
	if !ok {
		t.Fatalf("transport type=%T", roundTripper)
	}
	base, ok := wrapper.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport type=%T", wrapper.base)
	}
	if base.Proxy != nil {
		t.Fatal("review transport inherited environment proxy behavior")
	}
	if base.TLSClientConfig != nil && base.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("review transport disabled TLS certificate verification")
	}
}

func TestReviewBrokerAndWorkerUseSharedGuardedClient(t *testing.T) {
	config := newReviewTestConfig(t)
	broker, err := NewReviewBrokerClient(config, config.ReviewBindingCacheTTL)
	if err != nil {
		t.Fatalf("new review broker: %v", err)
	}
	if _, ok := broker.client.Transport.(*reviewEndpointTransport); !ok {
		t.Fatalf("broker transport type=%T, want guarded review transport", broker.client.Transport)
	}
	if err := broker.client.CheckRedirect(
		&http.Request{},
		[]*http.Request{{}},
	); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("broker redirect policy error=%v", err)
	}

	unsafeClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unsafe test client reached")
	})}
	worker, err := NewWorker(
		config,
		&fakeQueueStore{},
		WithWorkerHTTPClient(unsafeClient),
		WithWorkerReviewBindingResolver(&fakeReviewResolver{}),
	)
	if err != nil {
		t.Fatalf("new review worker: %v", err)
	}
	if _, ok := worker.client.Transport.(*reviewEndpointTransport); !ok {
		t.Fatalf("worker transport type=%T, want guarded review transport", worker.client.Transport)
	}
	if err := worker.client.CheckRedirect(
		&http.Request{},
		[]*http.Request{{}},
	); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("worker redirect policy error=%v", err)
	}
}

func TestForbiddenReviewEndpointAddressAllowsOrdinaryPublicAddresses(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if forbiddenReviewEndpointAddress(netip.MustParseAddr(raw), false) {
			t.Errorf("ordinary public address %s was rejected", raw)
		}
	}
	if !forbiddenReviewEndpointAddress(netip.MustParseAddr("127.0.0.1"), false) {
		t.Fatal("strict policy accepted loopback")
	}
	if forbiddenReviewEndpointAddress(netip.MustParseAddr("127.0.0.1"), true) {
		t.Fatal("explicit package-test policy rejected loopback")
	}
}
