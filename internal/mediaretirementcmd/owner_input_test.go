package mediaretirementcmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
	"github.com/stretchr/testify/require"
)

func mediaOwnerTestCA(t *testing.T, ca bool, from, until time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-owner-input-ca"},
		NotBefore: from, NotAfter: until, IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func mediaOwnerTestEnvelope(t *testing.T) []byte {
	t.Helper()
	at := time.Now()
	raw, err := json.Marshal(mediaOwnerCredentials{Version: 1, Username: "inventory_owner", Password: "PRIVATE_OWNER_PASSWORD",
		RootCA: mediaOwnerTestCA(t, true, at.Add(-time.Hour), at.Add(time.Hour))})
	require.NoError(t, err)
	return raw
}

func TestMediaOwnerInputClosedEnvelope(t *testing.T) {
	raw := mediaOwnerTestEnvelope(t)
	got, err := decodeMediaOwnerCredentials(raw)
	require.NoError(t, err)
	require.Equal(t, "inventory_owner", got.Username)
	for name, input := range map[string][]byte{
		"empty": nil, "array": []byte(`[]`), "null": []byte(`null`),
		"oversize":      []byte(strings.Repeat(" ", mediaOwnerInputByteLimit+1)),
		"invalid-utf8":  append(append([]byte{}, raw...), 0xff),
		"duplicate":     append([]byte(`{"version":1,`), raw[1:]...),
		"unknown-host":  append([]byte(`{"host":"another-server",`), raw[1:]...),
		"unknown-sql":   append([]byte(`{"sql":"SELECT private",`), raw[1:]...),
		"second-object": append(append([]byte{}, raw...), []byte(` {}`)...),
		"truncated":     raw[:len(raw)-1],
	} {
		t.Run(name, func(t *testing.T) {
			value, err := decodeMediaOwnerCredentials(input)
			require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
			require.Equal(t, mediaOwnerCredentials{}, value)
			require.NotContains(t, err.Error(), "PRIVATE_")
		})
	}
	for _, field := range []string{"version", "username", "password", "ca_pem"} {
		t.Run("missing-"+field, func(t *testing.T) {
			var v map[string]any
			require.NoError(t, json.Unmarshal(raw, &v))
			delete(v, field)
			bad, err := json.Marshal(v)
			require.NoError(t, err)
			_, err = decodeMediaOwnerCredentials(bad)
			require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
		})
	}
	for name, change := range map[string]func(*mediaOwnerCredentials){
		"version":        func(v *mediaOwnerCredentials) { v.Version = 2 },
		"empty-user":     func(v *mediaOwnerCredentials) { v.Username = "" },
		"long-user":      func(v *mediaOwnerCredentials) { v.Username = strings.Repeat("x", 64) },
		"control-user":   func(v *mediaOwnerCredentials) { v.Username = "private\nowner" },
		"empty-password": func(v *mediaOwnerCredentials) { v.Password = "" },
		"long-password":  func(v *mediaOwnerCredentials) { v.Password = strings.Repeat("x", 4097) },
		"nul-password":   func(v *mediaOwnerCredentials) { v.Password = "private\x00password" },
		"empty-ca":       func(v *mediaOwnerCredentials) { v.RootCA = "" },
		"long-ca":        func(v *mediaOwnerCredentials) { v.RootCA = strings.Repeat("x", 24577) },
	} {
		t.Run(name, func(t *testing.T) {
			v := got
			change(&v)
			bad, err := json.Marshal(v)
			require.NoError(t, err)
			_, err = decodeMediaOwnerCredentials(bad)
			require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
		})
	}
}

func TestMediaOwnerInputRequiresCertificateAuthority(t *testing.T) {
	at := time.Now()
	valid := mediaOwnerTestCA(t, true, at.Add(-time.Hour), at.Add(time.Hour))
	pool, err := mediaOwnerRootPool(valid, at)
	require.NoError(t, err)
	require.NotNil(t, pool)
	for name, text := range map[string]string{
		"empty": "", "junk": "PRIVATE_CA", "prefix-junk": "PRIVATE_PREFIX" + valid,
		"suffix-junk": valid + "PRIVATE_SUFFIX", "too-many": strings.Repeat(valid, 9),
		"private-key-block": strings.ReplaceAll(valid, "CERTIFICATE", "PRIVATE KEY"),
		"leaf":              mediaOwnerTestCA(t, false, at.Add(-time.Hour), at.Add(time.Hour)),
		"future":            mediaOwnerTestCA(t, true, at.Add(time.Hour), at.Add(2*time.Hour)),
		"expired":           mediaOwnerTestCA(t, true, at.Add(-2*time.Hour), at.Add(-time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := mediaOwnerRootPool(text, at)
			require.Nil(t, pool)
			require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
			require.NotContains(t, err.Error(), "PRIVATE_")
		})
	}
}

func TestMediaOwnerInputPinsRuntimeTargetAndPreservesConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg := config.DatabaseConfig{URL: "postgres://runtime:PRIVATE_RUNTIME_PASSWORD@db.example.invalid:25060/app?sslmode=require",
		MigrationURL: "postgres://MUST_NOT_USE:PRIVATE_MIGRATION_PASSWORD@other.invalid:5432/other"}
	before := cfg
	owner, err := mediaExplicitOwnerConfig(ctx, cfg, mediaOwnerTestEnvelope(t))
	require.NoError(t, err)
	require.Equal(t, before, cfg)
	require.Equal(t, "db.example.invalid", owner.Host)
	require.Equal(t, uint16(25060), owner.Port)
	require.Equal(t, "app", owner.Database)
	require.Equal(t, "inventory_owner", owner.User)
	require.Equal(t, "PRIVATE_OWNER_PASSWORD", owner.Password)
	require.Empty(t, owner.Fallbacks)
	require.False(t, owner.TLSConfig.InsecureSkipVerify)
	require.Equal(t, "db.example.invalid", owner.TLSConfig.ServerName)
	require.GreaterOrEqual(t, owner.TLSConfig.MinVersion, uint16(tls.VersionTLS12))
	require.NotNil(t, owner.TLSConfig.RootCAs)
	for name, target := range map[string]string{
		"plaintext":           "postgres://runtime:private@db.example.invalid/app?sslmode=disable",
		"plaintext-fallback":  "postgres://runtime:private@db.example.invalid/app?sslmode=prefer",
		"other-host-fallback": "postgres://runtime:private@db.example.invalid,other.invalid/app?sslmode=require",
		"role":                cfg.URL + "&role=owner", "empty-role": cfg.URL + "&role=",
		"options":       cfg.URL + "&options=-c%20role%3Downer",
		"authorization": cfg.URL + "&session_authorization=owner",
		"malformed":     "postgres://%XX",
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			bad.URL = target
			owner, err := mediaExplicitOwnerConfig(ctx, bad, mediaOwnerTestEnvelope(t))
			require.Nil(t, owner)
			require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
			require.NotContains(t, err.Error(), "private")
		})
	}
	_, err = mediaExplicitOwnerConfig(context.Background(), cfg, mediaOwnerTestEnvelope(t))
	require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
	cancel()
	_, err = mediaExplicitOwnerConfig(ctx, cfg, mediaOwnerTestEnvelope(t))
	require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
}
