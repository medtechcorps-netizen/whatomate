package handlers

import (
	"net/netip"
	"testing"
)

func TestForbiddenOutboundAddressRejectsZonedIPv6(t *testing.T) {
	t.Parallel()

	for _, rawAddress := range []string{
		"fe80::1%eth0",
		"2001:4860:4860::8888%eth0",
	} {
		address, err := netip.ParseAddr(rawAddress)
		if err != nil {
			t.Fatalf("parse %q: %v", rawAddress, err)
		}
		if !forbiddenOutboundAddress(address) {
			t.Fatalf("expected zoned IPv6 address %q to be forbidden", rawAddress)
		}
	}
}

func TestValidateWebhookRuntimeURLRejectsZonedIPv6(t *testing.T) {
	t.Parallel()

	if err := validateWebhookRuntimeURL("https://[fe80::1%25eth0]/header.png"); err == nil {
		t.Fatal("expected a zoned link-local IPv6 URL to be rejected")
	}
}

func TestForbiddenOutboundAddressAllowsPublicUnzonedIP(t *testing.T) {
	t.Parallel()

	address := netip.MustParseAddr("2606:4700:4700::1111")
	if forbiddenOutboundAddress(address) {
		t.Fatalf("expected public unzoned address %q to be allowed", address)
	}
}
