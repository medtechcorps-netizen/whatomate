package config

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// ReaformPilotConfig is deployment-owned. No tenant request can select a
// destination, credential or clinic. Nothing is enabled by default.
type ReaformPilotConfig struct {
	Enabled               bool   `koanf:"enabled"`
	OrganizationID        string `koanf:"organization_id"`
	APIBaseURL            string `koanf:"api_base_url"`
	APIToken              string `koanf:"api_token" json:"-"`
	RemoteOrganizationID  string `koanf:"remote_organization_id"`
	RemoteSiteID          string `koanf:"remote_site_id"`
	TerminalRetentionDays int    `koanf:"terminal_retention_days"`
}

func ValidateReaformPilotConfig(c ReaformPilotConfig) error {
	if !c.Enabled {
		return nil
	}
	if !canonicalNonNilUUID(c.OrganizationID) || !canonicalNonNilUUID(c.RemoteOrganizationID) || !canonicalNonNilUUID(c.RemoteSiteID) {
		return errors.New("Reaform pilot requires canonical organization and clinic UUID bindings")
	}
	u, err := url.Parse(c.APIBaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Port() != "" && u.Port() != "443") {
		return errors.New("Reaform pilot API base must be a public HTTPS origin")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || (ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast())) {
		return errors.New("Reaform pilot API base must be a public HTTPS origin")
	}
	if len(c.APIToken) != 64 || strings.Trim(c.APIToken, "0123456789abcdef") != "" {
		return errors.New("Reaform pilot requires a dedicated 64-character lowercase hexadecimal API token")
	}
	if c.TerminalRetentionDays < 0 || c.TerminalRetentionDays > 3650 {
		return errors.New("Reaform pilot terminal retention must be 1 to 3650 days, or 0 for the 90-day default")
	}
	return nil
}
