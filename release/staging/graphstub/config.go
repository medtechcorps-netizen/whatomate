package graphstub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Environment names the stub accepts. Anything else, production included, is
// refused before the listener opens.
const (
	EnvironmentLocal   = "local"
	EnvironmentStaging = "staging"
)

const (
	// DefaultListenAddr matches the staging app spec's internal port.
	DefaultListenAddr = ":8090"
	// DefaultStatusInterval spaces the scheduled status webhooks of a send.
	DefaultStatusInterval = 250 * time.Millisecond
	// WebhookPath is the product's Meta webhook route (cmd/whatomate/main.go).
	WebhookPath = "/api/webhook"

	maxAccessTokens    = 32
	maxAccounts        = 256
	maxStatusInterval  = time.Minute
	minControlKeyBytes = 32
	minSecretBytes     = 16
	maxSecretBytes     = 512
)

// refusedDomains are real production hosts. The stub never dials them, never
// answers a request addressed to them and never stores a callback on them, so
// a misrouted client cannot hand it a real credential and a misconfigured stub
// cannot post synthetic events into production.
var refusedDomains = []string{
	"rereply.app",
	"facebook.com",
	"fbsbx.com",
	"instagram.com",
	"googleapis.com",
}

var (
	graphIDPattern       = regexp.MustCompile(`^[0-9]{1,32}$`)
	displayPhonePattern  = regexp.MustCompile(`^\+?[0-9][0-9 ()-]{4,30}$`)
	printableNamePattern = regexp.MustCompile(`^[\x20-\x7e]{1,96}$`)
	secretPattern        = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// Account is one synthetic WhatsApp Business Account and phone number pair.
type Account struct {
	BusinessAccountID  string `json:"business_account_id"`
	PhoneNumberID      string `json:"phone_number_id"`
	DisplayPhoneNumber string `json:"display_phone_number"`
	VerifiedName       string `json:"verified_name,omitempty"`
}

// Config is the validated stub configuration. ConfigFromEnv builds it from
// the STUB_* environment; tests may build it directly and call Validate.
type Config struct {
	Environment    string
	ListenAddr     string
	AccessTokens   []string
	ControlKey     string
	AppID          string
	AppSecret      string
	CallbackOrigin string
	Accounts       []Account
	StatusSequence []string
	StatusInterval time.Duration
}

// ConfigFromEnv reads and validates the STUB_* variables. Errors name the
// variable only, never its value.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	config := Config{
		Environment:    strings.TrimSpace(getenv("STUB_ENVIRONMENT")),
		ListenAddr:     strings.TrimSpace(getenv("STUB_LISTEN_ADDR")),
		ControlKey:     getenv("STUB_CONTROL_KEY"),
		AppID:          strings.TrimSpace(getenv("STUB_APP_ID")),
		AppSecret:      getenv("STUB_APP_SECRET"),
		CallbackOrigin: strings.TrimSpace(getenv("STUB_CALLBACK_ORIGIN")),
		StatusInterval: DefaultStatusInterval,
		StatusSequence: []string{"sent", "delivered"},
	}
	if config.ListenAddr == "" {
		config.ListenAddr = DefaultListenAddr
	}
	for _, token := range strings.Split(getenv("STUB_ACCESS_TOKENS"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			config.AccessTokens = append(config.AccessTokens, token)
		}
	}
	if raw := strings.TrimSpace(getenv("STUB_ACCOUNTS")); raw != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config.Accounts); err != nil || decoder.More() {
			return Config{}, errors.New("STUB_ACCOUNTS must be a JSON array of accounts")
		}
	}
	if raw := strings.TrimSpace(getenv("STUB_STATUS_SEQUENCE")); raw != "" {
		config.StatusSequence = nil
		if raw != "none" {
			for _, status := range strings.Split(raw, ",") {
				config.StatusSequence = append(config.StatusSequence, strings.TrimSpace(status))
			}
		}
	}
	if raw := strings.TrimSpace(getenv("STUB_STATUS_INTERVAL")); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, errors.New("STUB_STATUS_INTERVAL must be a Go duration")
		}
		config.StatusInterval = interval
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate enforces every startup refusal. It normalizes CallbackOrigin.
func (c *Config) Validate() error {
	if c.Environment != EnvironmentLocal && c.Environment != EnvironmentStaging {
		return errors.New("STUB_ENVIRONMENT must be local or staging")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		return errors.New("STUB_LISTEN_ADDR must be host:port")
	}
	if len(c.ControlKey) < minControlKeyBytes || len(c.ControlKey) > maxSecretBytes || !secretPattern.MatchString(c.ControlKey) {
		return fmt.Errorf("STUB_CONTROL_KEY must be %d to %d printable characters", minControlKeyBytes, maxSecretBytes)
	}
	if len(c.AccessTokens) == 0 || len(c.AccessTokens) > maxAccessTokens {
		return fmt.Errorf("STUB_ACCESS_TOKENS must list 1 to %d tokens", maxAccessTokens)
	}
	for _, token := range c.AccessTokens {
		if !validSecret(token) || strings.Contains(token, ",") {
			return fmt.Errorf("STUB_ACCESS_TOKENS entries must be %d to %d printable characters", minSecretBytes, maxSecretBytes)
		}
	}
	if !graphIDPattern.MatchString(c.AppID) {
		return errors.New("STUB_APP_ID must be a numeric Graph ID")
	}
	if !validSecret(c.AppSecret) {
		return fmt.Errorf("STUB_APP_SECRET must be %d to %d printable characters", minSecretBytes, maxSecretBytes)
	}
	secrets := map[string]string{c.ControlKey: "STUB_CONTROL_KEY"}
	if _, reused := secrets[c.AppSecret]; reused {
		return errors.New("STUB_APP_SECRET must differ from STUB_CONTROL_KEY")
	}
	secrets[c.AppSecret] = "STUB_APP_SECRET"
	for _, token := range c.AccessTokens {
		if name, reused := secrets[token]; reused {
			return fmt.Errorf("STUB_ACCESS_TOKENS must not reuse %s", name)
		}
	}
	origin, err := normalizeCallbackOrigin(c.CallbackOrigin)
	if err != nil {
		return err
	}
	c.CallbackOrigin = origin
	if len(c.Accounts) > maxAccounts {
		return fmt.Errorf("STUB_ACCOUNTS must list at most %d accounts", maxAccounts)
	}
	phones := make(map[string]struct{}, len(c.Accounts))
	for index := range c.Accounts {
		account, err := normalizeAccount(c.Accounts[index])
		if err != nil {
			return fmt.Errorf("STUB_ACCOUNTS[%d]: %w", index, err)
		}
		if _, duplicate := phones[account.PhoneNumberID]; duplicate {
			return fmt.Errorf("STUB_ACCOUNTS[%d]: duplicate phone_number_id", index)
		}
		phones[account.PhoneNumberID] = struct{}{}
		c.Accounts[index] = account
	}
	for _, status := range c.StatusSequence {
		if !scheduledStatuses[status] {
			return errors.New("STUB_STATUS_SEQUENCE must list sent, delivered, read or failed, or be none")
		}
	}
	if c.StatusInterval < 0 || c.StatusInterval > maxStatusInterval {
		return errors.New("STUB_STATUS_INTERVAL must be between 0s and 1m")
	}
	return nil
}

var scheduledStatuses = map[string]bool{"sent": true, "delivered": true, "read": true, "failed": true}

func validSecret(value string) bool {
	return len(value) >= minSecretBytes && len(value) <= maxSecretBytes && secretPattern.MatchString(value)
}

// normalizeCallbackOrigin accepts an http(s) origin only, the same shape the
// product requires of whatsapp.base_url, and refuses production hosts.
func normalizeCallbackOrigin(raw string) (string, error) {
	invalid := errors.New("STUB_CALLBACK_ORIGIN must be an http(s) origin without credentials, path, query or fragment")
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", invalid
	}
	if port := parsed.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			return "", invalid
		}
	}
	if RefusedHost(parsed.Hostname()) {
		return "", errors.New("STUB_CALLBACK_ORIGIN must not be a production host")
	}
	return parsed.Scheme + "://" + strings.ToLower(parsed.Host), nil
}

// RefusedHost reports whether host (a hostname, optionally with a port) is a
// production domain or one of its subdomains.
func RefusedHost(host string) bool {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, domain := range refusedDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func normalizeAccount(account Account) (Account, error) {
	account.BusinessAccountID = strings.TrimSpace(account.BusinessAccountID)
	account.PhoneNumberID = strings.TrimSpace(account.PhoneNumberID)
	account.DisplayPhoneNumber = strings.TrimSpace(account.DisplayPhoneNumber)
	account.VerifiedName = strings.TrimSpace(account.VerifiedName)
	if account.VerifiedName == "" {
		account.VerifiedName = "Graph Stub Business"
	}
	switch {
	case !graphIDPattern.MatchString(account.BusinessAccountID):
		return Account{}, errors.New("business_account_id must be a numeric Graph ID")
	case !graphIDPattern.MatchString(account.PhoneNumberID):
		return Account{}, errors.New("phone_number_id must be a numeric Graph ID")
	case account.BusinessAccountID == account.PhoneNumberID:
		return Account{}, errors.New("business_account_id and phone_number_id must differ")
	case !displayPhonePattern.MatchString(account.DisplayPhoneNumber):
		return Account{}, errors.New("display_phone_number must be a phone number")
	case !printableNamePattern.MatchString(account.VerifiedName):
		return Account{}, errors.New("verified_name must be 1 to 96 printable characters")
	}
	return account, nil
}
