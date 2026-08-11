package metarelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// reviewIPResolver is deliberately smaller than net.Resolver so package tests
// can prove the dial-time DNS and rebinding policy without using external DNS.
type reviewIPResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type reviewContextDialer func(
	ctx context.Context,
	network, address string,
) (net.Conn, error)

// newReviewEndpointHTTPClient is shared by the review broker and the review
// worker. It intentionally bypasses environment proxy settings: otherwise the
// relay would validate the proxy address while allowing that proxy to resolve
// and fetch an unvalidated target.
func newReviewEndpointHTTPClient(timeout time.Duration, allowTestLoopback bool) *http.Client {
	transport := newReviewEndpointTransport(
		net.DefaultResolver,
		(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		allowTestLoopback,
	)
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newReviewEndpointTransport(
	resolver reviewIPResolver,
	dial reviewContextDialer,
	allowTestLoopback bool,
) http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = reviewPublicDialContext(resolver, dial, allowTestLoopback)
	return &reviewEndpointTransport{
		base:              base,
		allowTestLoopback: allowTestLoopback,
	}
}

type reviewEndpointTransport struct {
	base              http.RoundTripper
	allowTestLoopback bool
}

func (t *reviewEndpointTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("review endpoint request is invalid")
	}
	switch strings.ToLower(request.URL.Scheme) {
	case "https":
		// Keep Go's normal certificate and hostname verification. The cloned
		// default transport has no custom TLSClientConfig.
	case "http":
		// This branch is unreachable from environment configuration. It exists
		// solely for package tests that explicitly set the unexported test flag
		// and use httptest's literal loopback origin.
		address, err := netip.ParseAddr(request.URL.Hostname())
		if !t.allowTestLoopback || err != nil || !address.Unmap().IsLoopback() {
			return nil, errors.New("review endpoints require public HTTPS")
		}
	default:
		return nil, errors.New("review endpoints require public HTTPS")
	}
	return t.base.RoundTrip(request)
}

func reviewPublicDialContext(
	resolver reviewIPResolver,
	dial reviewContextDialer,
	allowTestLoopback bool,
) reviewContextDialer {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if resolver == nil || dial == nil {
			return nil, errors.New("review endpoint network policy is unavailable")
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil || strings.TrimSpace(port) == "" {
			return nil, errors.New("review endpoint address is invalid")
		}

		addresses, err := resolveReviewEndpoint(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		allowLiteralTestLoopback := false
		if literal, parseErr := netip.ParseAddr(strings.TrimSpace(host)); parseErr == nil {
			allowLiteralTestLoopback = allowTestLoopback && literal.Unmap().IsLoopback()
		}
		for _, candidate := range addresses {
			if forbiddenReviewEndpointAddress(candidate, allowLiteralTestLoopback) {
				// Reject a mixed public/private answer as a whole. Selecting only a
				// public member could otherwise produce resolver-order-dependent
				// behavior during DNS rebinding.
				return nil, errors.New("review endpoint resolved to a non-public address")
			}
		}

		var lastErr error
		attempted := false
		for _, candidate := range addresses {
			if network == "tcp4" && !candidate.Is4() {
				continue
			}
			if network == "tcp6" && candidate.Is4() {
				continue
			}
			attempted = true
			connection, dialErr := dial(
				ctx,
				network,
				net.JoinHostPort(candidate.String(), port),
			)
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if !attempted {
			return nil, errors.New("review endpoint has no address for the requested network")
		}
		return nil, fmt.Errorf("review endpoint dial failed: %w", lastErr)
	}
}

func resolveReviewEndpoint(
	ctx context.Context,
	resolver reviewIPResolver,
	host string,
) ([]netip.Addr, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("review endpoint hostname is empty")
	}
	if literal, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{literal}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, errors.New("review endpoint DNS resolution failed")
	}
	if len(addresses) == 0 {
		return nil, errors.New("review endpoint hostname did not resolve")
	}
	return addresses, nil
}

var reservedReviewEndpointPrefixes = []netip.Prefix{
	// IPv4 special-purpose, documentation, benchmarking, multicast, and
	// future-use ranges. Private, loopback, and link-local are also rejected
	// through netip predicates below, but are listed where broader reservations
	// matter (for example 0/8 and carrier-grade NAT).
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),

	// IPv6 discard, protocol-assignment, local-use translation,
	// documentation, deprecated transition, and multicast ranges.
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func forbiddenReviewEndpointAddress(address netip.Addr, allowTestLoopback bool) bool {
	if !address.IsValid() || address.Zone() != "" {
		return true
	}
	address = address.Unmap()
	if allowTestLoopback && address.IsLoopback() {
		return false
	}
	if !address.IsGlobalUnicast() || address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsUnspecified() || address.IsMulticast() {
		return true
	}
	for _, prefix := range reservedReviewEndpointPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
