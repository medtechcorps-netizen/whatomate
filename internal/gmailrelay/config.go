// Package gmailrelay implements the Gmail transport used by ReReply's generic
// signed-relay email channel.
package gmailrelay

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultListenAddr        = ":8082"
	defaultRedisPrefix       = "rereply:gmail-relay:"
	defaultGoogleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultGoogleTokenURL    = "https://oauth2.googleapis.com/token"
	defaultGmailAPIBaseURL   = "https://gmail.googleapis.com/gmail/v1"
	defaultHTTPTimeout       = 30 * time.Second
	defaultGmailPollInterval = 30 * time.Second
	defaultQueuePollInterval = 500 * time.Millisecond
	defaultForwardTimeout    = 15 * time.Second
	defaultProcessingLease   = 60 * time.Second
	defaultInboundRetention  = 7 * 24 * time.Hour
	defaultOutboundRetention = 7 * 24 * time.Hour
	defaultMaxAttempts       = 12
	defaultWorkerConcurrency = 2
	minimumEncryptionKeyLen  = 32
	minimumSetupKeyLen       = 32
)

// Config is environment-only so the relay has one unambiguous production
// mailbox and one durable Redis security boundary. Mailbox may be any ordinary
// Gmail or Google Workspace address; deployments for the initial customer set
// it to realignphysiolates@gmail.com.
type Config struct {
	ListenAddr            string
	RedisURL              string
	RedisPrefix           string
	Mailbox               string
	GoogleClientID        string
	GoogleClientSecret    string
	GoogleRedirectURL     string
	GoogleAuthURL         string
	GoogleTokenURL        string
	GmailAPIBaseURL       string
	EncryptionKey         string
	SetupKey              string
	ReReplyWebhookURL     string
	ReReplyInboundSecret  string
	ReReplyOutboundSecret string
	HTTPTimeout           time.Duration
	GmailPollInterval     time.Duration
	QueuePollInterval     time.Duration
	ForwardTimeout        time.Duration
	ProcessingLease       time.Duration
	InboundRetention      time.Duration
	OutboundRetention     time.Duration
	MaxAttempts           int
	WorkerConcurrency     int
}

// LoadConfig loads and validates the Gmail relay's environment configuration.
func LoadConfig() (*Config, error) {
	return loadConfig(os.Getenv)
}

func loadConfig(getenv func(string) string) (*Config, error) {
	config := &Config{
		ListenAddr:            strings.TrimSpace(getenv("GMAIL_RELAY_LISTEN_ADDR")),
		RedisURL:              strings.TrimSpace(getenv("GMAIL_RELAY_REDIS_URL")),
		RedisPrefix:           strings.TrimSpace(getenv("GMAIL_RELAY_REDIS_PREFIX")),
		Mailbox:               strings.ToLower(strings.TrimSpace(getenv("GMAIL_RELAY_MAILBOX"))),
		GoogleClientID:        strings.TrimSpace(getenv("GMAIL_RELAY_GOOGLE_CLIENT_ID")),
		GoogleClientSecret:    getenv("GMAIL_RELAY_GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:     strings.TrimSpace(getenv("GMAIL_RELAY_GOOGLE_REDIRECT_URL")),
		GoogleAuthURL:         strings.TrimSpace(getenv("GMAIL_RELAY_GOOGLE_AUTH_URL")),
		GoogleTokenURL:        strings.TrimSpace(getenv("GMAIL_RELAY_GOOGLE_TOKEN_URL")),
		GmailAPIBaseURL:       strings.TrimSpace(getenv("GMAIL_RELAY_GMAIL_API_BASE_URL")),
		EncryptionKey:         getenv("GMAIL_RELAY_ENCRYPTION_KEY"),
		SetupKey:              getenv("GMAIL_RELAY_SETUP_KEY"),
		ReReplyWebhookURL:     strings.TrimSpace(getenv("GMAIL_RELAY_REREPLY_WEBHOOK_URL")),
		ReReplyInboundSecret:  getenv("GMAIL_RELAY_REREPLY_INBOUND_SECRET"),
		ReReplyOutboundSecret: getenv("GMAIL_RELAY_REREPLY_OUTBOUND_SECRET"),
		HTTPTimeout:           defaultHTTPTimeout,
		GmailPollInterval:     defaultGmailPollInterval,
		QueuePollInterval:     defaultQueuePollInterval,
		ForwardTimeout:        defaultForwardTimeout,
		ProcessingLease:       defaultProcessingLease,
		InboundRetention:      defaultInboundRetention,
		OutboundRetention:     defaultOutboundRetention,
		MaxAttempts:           defaultMaxAttempts,
		WorkerConcurrency:     defaultWorkerConcurrency,
	}
	if config.ListenAddr == "" {
		config.ListenAddr = defaultListenAddr
	}
	if config.RedisPrefix == "" {
		config.RedisPrefix = defaultRedisPrefix
	}
	if config.GoogleAuthURL == "" {
		config.GoogleAuthURL = defaultGoogleAuthURL
	}
	if config.GoogleTokenURL == "" {
		config.GoogleTokenURL = defaultGoogleTokenURL
	}
	if config.GmailAPIBaseURL == "" {
		config.GmailAPIBaseURL = defaultGmailAPIBaseURL
	}

	var err error
	for _, item := range []struct {
		name   string
		target *time.Duration
	}{
		{"GMAIL_RELAY_HTTP_TIMEOUT", &config.HTTPTimeout},
		{"GMAIL_RELAY_GMAIL_POLL_INTERVAL", &config.GmailPollInterval},
		{"GMAIL_RELAY_QUEUE_POLL_INTERVAL", &config.QueuePollInterval},
		{"GMAIL_RELAY_FORWARD_TIMEOUT", &config.ForwardTimeout},
		{"GMAIL_RELAY_PROCESSING_LEASE", &config.ProcessingLease},
		{"GMAIL_RELAY_INBOUND_RETENTION", &config.InboundRetention},
		{"GMAIL_RELAY_OUTBOUND_RETENTION", &config.OutboundRetention},
	} {
		if *item.target, err = envDuration(getenv, item.name, *item.target); err != nil {
			return nil, err
		}
	}
	if config.MaxAttempts, err = envPositiveInt(getenv, "GMAIL_RELAY_MAX_ATTEMPTS", config.MaxAttempts); err != nil {
		return nil, err
	}
	if config.WorkerConcurrency, err = envPositiveInt(getenv, "GMAIL_RELAY_WORKER_CONCURRENCY", config.WorkerConcurrency); err != nil {
		return nil, err
	}
	if config.ReReplyOutboundSecret == "" {
		config.ReReplyOutboundSecret = config.ReReplyInboundSecret
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return config, nil
}

func (c *Config) validate() error {
	if c == nil {
		return errors.New("gmail relay configuration is required")
	}
	if c.RedisURL == "" {
		return errors.New("environment variable GMAIL_RELAY_REDIS_URL is required; OAuth state and tokens have no in-memory fallback")
	}
	if _, err := redis.ParseURL(c.RedisURL); err != nil {
		return errors.New("environment variable GMAIL_RELAY_REDIS_URL is invalid")
	}
	redisEndpoint, err := url.Parse(c.RedisURL)
	if err != nil {
		return errors.New("environment variable GMAIL_RELAY_REDIS_URL is invalid")
	}
	redisHost := strings.ToLower(redisEndpoint.Hostname())
	redisIsLocal := redisHost == "localhost" || redisHost == "127.0.0.1" || redisHost == "::1"
	if redisEndpoint.Scheme != "rediss" && !redisIsLocal {
		return errors.New("environment variable GMAIL_RELAY_REDIS_URL must use TLS (rediss) outside localhost")
	}
	if strings.TrimSpace(c.RedisPrefix) == "" {
		return errors.New("environment variable GMAIL_RELAY_REDIS_PREFIX must not be empty")
	}
	if err := validateMailbox(c.Mailbox); err != nil {
		return fmt.Errorf("environment variable GMAIL_RELAY_MAILBOX is invalid: %w", err)
	}
	if strings.TrimSpace(c.GoogleClientID) == "" {
		return errors.New("environment variable GMAIL_RELAY_GOOGLE_CLIENT_ID is required")
	}
	if strings.TrimSpace(c.GoogleClientSecret) == "" {
		return errors.New("environment variable GMAIL_RELAY_GOOGLE_CLIENT_SECRET is required")
	}
	if len(c.GoogleClientID) > 1024 || len(c.GoogleClientSecret) > 8192 {
		return errors.New("google OAuth credentials are invalid")
	}
	if err := validateEndpoint(c.GoogleRedirectURL, true); err != nil {
		return fmt.Errorf("environment variable GMAIL_RELAY_GOOGLE_REDIRECT_URL is invalid: %w", err)
	}
	if err := validateEndpoint(c.GoogleAuthURL, true); err != nil {
		return errors.New("environment variable GMAIL_RELAY_GOOGLE_AUTH_URL is invalid")
	}
	if err := validateEndpoint(c.GoogleTokenURL, true); err != nil {
		return errors.New("environment variable GMAIL_RELAY_GOOGLE_TOKEN_URL is invalid")
	}
	if err := validateEndpoint(c.GmailAPIBaseURL, true); err != nil {
		return errors.New("environment variable GMAIL_RELAY_GMAIL_API_BASE_URL is invalid")
	}
	if len(c.EncryptionKey) < minimumEncryptionKeyLen {
		return fmt.Errorf("environment variable GMAIL_RELAY_ENCRYPTION_KEY must contain at least %d bytes", minimumEncryptionKeyLen)
	}
	if len(c.SetupKey) < minimumSetupKeyLen {
		return fmt.Errorf("environment variable GMAIL_RELAY_SETUP_KEY must contain at least %d bytes", minimumSetupKeyLen)
	}
	pairedValues := 0
	for _, value := range []string{c.ReReplyWebhookURL, c.ReReplyInboundSecret, c.ReReplyOutboundSecret} {
		if strings.TrimSpace(value) != "" {
			pairedValues++
		}
	}
	if pairedValues != 0 && pairedValues != 3 {
		return errors.New("rereply webhook URL and both relay signing secrets must be configured together")
	}
	if pairedValues == 3 {
		if err := validateEndpoint(c.ReReplyWebhookURL, true); err != nil {
			return fmt.Errorf("environment variable GMAIL_RELAY_REREPLY_WEBHOOK_URL is invalid: %w", err)
		}
		if len(c.ReReplyInboundSecret) < 32 || len(c.ReReplyOutboundSecret) < 32 {
			return errors.New("rereply relay signing secrets must each contain at least 32 bytes")
		}
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("gmail relay HTTP timeout must be positive")
	}
	if c.GmailPollInterval <= 0 || c.QueuePollInterval <= 0 || c.ForwardTimeout <= 0 ||
		c.ProcessingLease <= c.ForwardTimeout+5*time.Second || c.InboundRetention <= 0 ||
		c.OutboundRetention <= 0 || c.MaxAttempts <= 0 || c.WorkerConcurrency <= 0 ||
		c.WorkerConcurrency > 32 {
		return errors.New("gmail relay worker configuration is invalid")
	}
	return nil
}

// Paired reports whether the relay has the ReReply webhook and signing
// material required to exchange messages. OAuth setup is available before
// pairing so the public deployment can be bootstrapped safely.
func (c *Config) Paired() bool {
	return c != nil && c.ReReplyWebhookURL != "" && c.ReReplyInboundSecret != "" && c.ReReplyOutboundSecret != ""
}

func envDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return duration, nil
}

func envPositiveInt(getenv func(string) string, name string, fallback int) (int, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 || fmt.Sprintf("%d", parsed) != value {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func validateMailbox(value string) error {
	if value == "" || len(value) > 254 || strings.ContainsAny(value, "\r\n") {
		return errors.New("a mailbox address is required")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || strings.ToLower(parsed.Address) != value {
		return errors.New("must be one normalized email address without a display name")
	}
	parts := strings.Split(parsed.Address, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.Contains(parts[1], ".") {
		return errors.New("must be a complete email address")
	}
	return nil
}

func validateEndpoint(raw string, allowLocalHTTP bool) error {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 2048 {
		return errors.New("an absolute URL is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("must be an absolute URL without credentials or fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if allowLocalHTTP && parsed.Scheme == "http" {
		host := strings.ToLower(parsed.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
	}
	return errors.New("secure HTTPS endpoint is required")
}
