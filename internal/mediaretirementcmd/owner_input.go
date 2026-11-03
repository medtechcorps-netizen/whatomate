package mediaretirementcmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

const mediaOwnerInputByteLimit = 32768

type mediaOwnerCredentials struct {
	Version  int    `json:"version"`
	Username string `json:"username"`
	Password string `json:"password"`
	RootCA   string `json:"ca_pem"`
}

// decodeMediaOwnerCredentials accepts one closed envelope, including a
// duplicate-key check. Callers own input capture and must never print raw input
// or decoder/driver errors. The envelope has no host, database, SQL or options.
func decodeMediaOwnerCredentials(raw []byte) (mediaOwnerCredentials, error) {
	var result mediaOwnerCredentials
	if len(raw) == 0 || len(raw) > mediaOwnerInputByteLimit || !utf8.Valid(raw) {
		return result, mediaretirement.ErrIncomplete
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return result, mediaretirement.ErrIncomplete
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return mediaOwnerCredentials{}, mediaretirement.ErrIncomplete
		}
		seen[name] = true
		switch name {
		case "version":
			err = d.Decode(&result.Version)
		case "username":
			err = d.Decode(&result.Username)
		case "password":
			err = d.Decode(&result.Password)
		case "ca_pem":
			err = d.Decode(&result.RootCA)
		default:
			return mediaOwnerCredentials{}, mediaretirement.ErrIncomplete
		}
		if err != nil {
			return mediaOwnerCredentials{}, mediaretirement.ErrIncomplete
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || len(seen) != 4 {
		return mediaOwnerCredentials{}, mediaretirement.ErrIncomplete
	}
	if _, err = d.Token(); err != io.EOF {
		return mediaOwnerCredentials{}, mediaretirement.ErrIncomplete
	}
	if result.Version != 1 || result.Username == "" || len(result.Username) > 63 ||
		strings.ContainsAny(result.Username, "\x00\r\n\t ") || result.Password == "" ||
		len(result.Password) > 4096 || strings.ContainsRune(result.Password, 0) ||
		result.RootCA == "" || len(result.RootCA) > 24576 {
		return mediaOwnerCredentials{}, mediaretirement.ErrIncomplete
	}
	return result, nil
}

func mediaOwnerRootPool(raw string, at time.Time) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	remaining := []byte(raw)
	count := 0
	for len(bytes.TrimSpace(remaining)) != 0 {
		remaining = bytes.TrimSpace(remaining)
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, mediaretirement.ErrIncomplete
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || count == 8 {
			return nil, mediaretirement.ErrIncomplete
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid ||
			cert.KeyUsage&x509.KeyUsageCertSign == 0 || at.Before(cert.NotBefore) || !at.Before(cert.NotAfter) {
			return nil, mediaretirement.ErrIncomplete
		}
		pool.AddCert(cert)
		count++
		remaining = rest
	}
	if count == 0 {
		return nil, mediaretirement.ErrIncomplete
	}
	return pool, nil
}

// mediaExplicitOwnerConfig constructs a separate maintenance connection. The
// server, port and database come only from the existing runtime configuration;
// the supplied envelope can change only authentication and the trust root.
// MigrationURL is never consulted and cfg is never modified. Native owner/RLS
// admission must still pass before any customer rows are read.
func mediaExplicitOwnerConfig(ctx context.Context, cfg config.DatabaseConfig, raw []byte) (*pgx.ConnConfig, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, mediaretirement.ErrIncomplete
	}
	credentials, err := decodeMediaOwnerCredentials(raw)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	dsn, err := mediaInventoryDSN(cfg)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil || parsed.TLSConfig == nil || len(parsed.Fallbacks) != 0 ||
		parsed.Host == "" || strings.ContainsAny(parsed.Host, "/, \t\r\n") || parsed.Port == 0 || parsed.Database == "" {
		return nil, mediaretirement.ErrIncomplete
	}
	for _, key := range []string{"role", "options", "session_authorization"} {
		if _, exists := parsed.RuntimeParams[key]; exists {
			return nil, mediaretirement.ErrIncomplete
		}
	}
	pool, err := mediaOwnerRootPool(credentials.RootCA, time.Now())
	if err != nil || ctx.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	owner := parsed.Copy()
	owner.User = credentials.Username
	owner.Password = credentials.Password
	owner.TLSConfig = parsed.TLSConfig.Clone()
	owner.TLSConfig.RootCAs = pool
	owner.TLSConfig.ServerName = parsed.Host
	owner.TLSConfig.InsecureSkipVerify = false
	if owner.TLSConfig.MinVersion < tls.VersionTLS12 {
		owner.TLSConfig.MinVersion = tls.VersionTLS12
	}
	return owner, nil
}
