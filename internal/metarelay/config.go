package metarelay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	defaultListenAddr        = ":8081"
	defaultRedisPrefix       = "rereply:meta-relay:"
	defaultInboundRetention  = 7 * 24 * time.Hour
	defaultOutboundRetention = 7 * 24 * time.Hour
	defaultProcessingLease   = 60 * time.Second
	defaultForwardTimeout    = 15 * time.Second
	defaultPollInterval      = 500 * time.Millisecond
	defaultWorkerConcurrency = 4
	defaultMaxAttempts       = 12
	defaultRegistryCacheTTL  = 10 * time.Second
	defaultRegistryTimeout   = 3 * time.Second
	queueSettlementTimeout   = 5 * time.Second
	leaseSafetyMargin        = time.Second
	maxWorkerConcurrency     = 64
)

var (
	envNamePattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	graphVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)
	accountKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	metaAppIDPattern    = regexp.MustCompile(`^[0-9]+$`)
)

// LoadConfig loads and validates the relay's environment-only configuration.
func LoadConfig() (*Config, error) {
	return loadConfig(os.Getenv)
}

func loadConfig(getenv func(string) string) (*Config, error) {
	config := &Config{
		ListenAddr:                  strings.TrimSpace(getenv("META_RELAY_LISTEN_ADDR")),
		RedisURL:                    strings.TrimSpace(getenv("META_RELAY_REDIS_URL")),
		RedisPrefix:                 strings.TrimSpace(getenv("META_RELAY_REDIS_PREFIX")),
		MessengerAppSecret:          getenv("META_RELAY_MESSENGER_APP_SECRET"),
		MessengerVerifyToken:        getenv("META_RELAY_MESSENGER_VERIFY_TOKEN"),
		ManagedMessengerAppID:       strings.TrimSpace(getenv("META_RELAY_MANAGED_MESSENGER_APP_ID")),
		ManagedMessengerAppSecret:   getenv("META_RELAY_MANAGED_MESSENGER_APP_SECRET"),
		ManagedMessengerVerifyToken: getenv("META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN"),
		InstagramLoginAppSecret:     getenv("META_RELAY_INSTAGRAM_APP_SECRET"),
		InstagramLoginVerifyToken:   getenv("META_RELAY_INSTAGRAM_VERIFY_TOKEN"),
		GraphAPIVersion:             strings.TrimSpace(getenv("META_RELAY_GRAPH_API_VERSION")),
		InboundRetention:            defaultInboundRetention,
		OutboundRetention:           defaultOutboundRetention,
		ProcessingLease:             defaultProcessingLease,
		ForwardTimeout:              defaultForwardTimeout,
		PollInterval:                defaultPollInterval,
		WorkerConcurrency:           defaultWorkerConcurrency,
		MaxAttempts:                 defaultMaxAttempts,
		RegistryEnabled:             false,
		RegistryURL:                 strings.TrimSpace(getenv("META_RELAY_REGISTRY_URL")),
		RegistrySecret:              getenv("META_RELAY_REGISTRY_SECRET"),
		RegistryEdgeSecret:          getenv("META_RELAY_REGISTRY_EDGE_SECRET"),
		RegistryCacheTTL:            defaultRegistryCacheTTL,
		RegistryTimeout:             defaultRegistryTimeout,
	}
	if value := strings.TrimSpace(getenv("META_RELAY_REGISTRY_ENABLED")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, errors.New("environment variable META_RELAY_REGISTRY_ENABLED must be true or false")
		}
		config.RegistryEnabled = parsed
	}
	if value := strings.TrimSpace(getenv("META_RELAY_DYNAMIC_QUEUE_READER_VERSION")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return nil, errors.New("environment variable META_RELAY_DYNAMIC_QUEUE_READER_VERSION must be a non-negative integer")
		}
		config.RegistryQueueReader = parsed
	}
	if config.ListenAddr == "" {
		config.ListenAddr = defaultListenAddr
	}
	if config.RedisPrefix == "" {
		config.RedisPrefix = defaultRedisPrefix
	}

	var err error
	if config.InboundRetention, err = envDuration(getenv, "META_RELAY_INBOUND_RETENTION", config.InboundRetention); err != nil {
		return nil, err
	}
	if config.OutboundRetention, err = envDuration(getenv, "META_RELAY_OUTBOUND_RETENTION", config.OutboundRetention); err != nil {
		return nil, err
	}
	if config.ProcessingLease, err = envDuration(getenv, "META_RELAY_PROCESSING_LEASE", config.ProcessingLease); err != nil {
		return nil, err
	}
	if config.ForwardTimeout, err = envDuration(getenv, "META_RELAY_FORWARD_TIMEOUT", config.ForwardTimeout); err != nil {
		return nil, err
	}
	if config.PollInterval, err = envDuration(getenv, "META_RELAY_POLL_INTERVAL", config.PollInterval); err != nil {
		return nil, err
	}
	if config.WorkerConcurrency, err = envPositiveInt(getenv, "META_RELAY_WORKER_CONCURRENCY", config.WorkerConcurrency); err != nil {
		return nil, err
	}
	if config.MaxAttempts, err = envPositiveInt(getenv, "META_RELAY_MAX_ATTEMPTS", config.MaxAttempts); err != nil {
		return nil, err
	}
	if config.RegistryCacheTTL, err = envDuration(getenv, "META_RELAY_REGISTRY_CACHE_TTL", config.RegistryCacheTTL); err != nil {
		return nil, err
	}
	if config.RegistryTimeout, err = envDuration(getenv, "META_RELAY_REGISTRY_TIMEOUT", config.RegistryTimeout); err != nil {
		return nil, err
	}

	rawAccounts := strings.TrimSpace(getenv("META_RELAY_ACCOUNTS_JSON"))
	if rawAccounts != "" {
		decoder := json.NewDecoder(strings.NewReader(rawAccounts))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config.Accounts); err != nil {
			return nil, errors.New("environment variable META_RELAY_ACCOUNTS_JSON is invalid")
		}
		if err := rejectTrailingJSON(decoder); err != nil {
			return nil, errors.New("environment variable META_RELAY_ACCOUNTS_JSON is invalid")
		}
	}
	if err := config.validateAndIndex(getenv); err != nil {
		return nil, err
	}
	return config, nil
}

func (c *Config) validateAndIndex(getenv func(string) string) error {
	if strings.TrimSpace(c.RedisURL) == "" {
		return errors.New("environment variable META_RELAY_REDIS_URL is required; durable acceptance has no in-memory fallback")
	}
	if _, err := redis.ParseURL(c.RedisURL); err != nil {
		return errors.New("environment variable META_RELAY_REDIS_URL is invalid")
	}
	if strings.TrimSpace(c.MessengerAppSecret) == "" {
		return errors.New("environment variable META_RELAY_MESSENGER_APP_SECRET is required")
	}
	if strings.TrimSpace(c.MessengerVerifyToken) == "" {
		return errors.New("environment variable META_RELAY_MESSENGER_VERIFY_TOKEN is required")
	}
	if strings.TrimSpace(c.InstagramLoginAppSecret) == "" {
		return errors.New("environment variable META_RELAY_INSTAGRAM_APP_SECRET is required")
	}
	if strings.TrimSpace(c.InstagramLoginVerifyToken) == "" {
		return errors.New("environment variable META_RELAY_INSTAGRAM_VERIFY_TOKEN is required")
	}
	if c.MessengerAppSecret == c.InstagramLoginAppSecret {
		return errors.New("messenger and Instagram app secrets must be distinct")
	}
	if c.MessengerVerifyToken == c.InstagramLoginVerifyToken {
		return errors.New("messenger and Instagram verify tokens must be distinct")
	}
	if !graphVersionPattern.MatchString(c.GraphAPIVersion) {
		return errors.New("environment variable META_RELAY_GRAPH_API_VERSION must be explicit, for example v25.0")
	}
	registryConfigured := strings.TrimSpace(c.RegistryURL) != "" ||
		strings.TrimSpace(c.RegistrySecret) != "" || strings.TrimSpace(c.RegistryEdgeSecret) != "" ||
		strings.TrimSpace(c.ManagedMessengerAppID) != "" ||
		strings.TrimSpace(c.ManagedMessengerAppSecret) != "" ||
		strings.TrimSpace(c.ManagedMessengerVerifyToken) != ""
	if registryConfigured != c.RegistryEnabled {
		return errors.New("dynamic Meta registry URL and credentials must be configured exactly when META_RELAY_REGISTRY_ENABLED=true")
	}
	if registryConfigured {
		if !metaAppIDPattern.MatchString(c.ManagedMessengerAppID) {
			return errors.New("environment variable META_RELAY_MANAGED_MESSENGER_APP_ID is required when the dynamic registry is enabled")
		}
		if len(c.ManagedMessengerAppSecret) < 32 {
			return errors.New("environment variable META_RELAY_MANAGED_MESSENGER_APP_SECRET must contain at least 32 bytes")
		}
		if strings.TrimSpace(c.ManagedMessengerVerifyToken) == "" {
			return errors.New("environment variable META_RELAY_MANAGED_MESSENGER_VERIFY_TOKEN is required")
		}
		if c.ManagedMessengerAppSecret == c.MessengerAppSecret ||
			c.ManagedMessengerAppSecret == c.InstagramLoginAppSecret {
			return errors.New("managed Messenger and legacy Meta app secrets must be distinct")
		}
		if c.ManagedMessengerVerifyToken == c.MessengerVerifyToken ||
			c.ManagedMessengerVerifyToken == c.InstagramLoginVerifyToken {
			return errors.New("managed Messenger and legacy Meta verify tokens must be distinct")
		}
		if c.RegistryQueueReader != InboundJobSchemaVersion {
			return fmt.Errorf("META_RELAY_DYNAMIC_QUEUE_READER_VERSION must be %d before dynamic producers are enabled", InboundJobSchemaVersion)
		}
		if err := validateEndpoint(c.RegistryURL, c.allowInsecureTestEndpoints); err != nil {
			return fmt.Errorf("environment variable META_RELAY_REGISTRY_URL: %w", err)
		}
		parsedRegistryURL, _ := url.Parse(c.RegistryURL)
		if parsedRegistryURL.Path != "/internal/meta-registry/v1/resolve" ||
			parsedRegistryURL.EscapedPath() != "/internal/meta-registry/v1/resolve" ||
			parsedRegistryURL.RawPath != "" || parsedRegistryURL.RawQuery != "" ||
			parsedRegistryURL.ForceQuery || parsedRegistryURL.Opaque != "" {
			return errors.New("environment variable META_RELAY_REGISTRY_URL must be the exact resolve endpoint without a query")
		}
		if len(c.RegistrySecret) < 32 {
			return errors.New("environment variable META_RELAY_REGISTRY_SECRET must contain at least 32 bytes")
		}
		if len(c.RegistryEdgeSecret) < 32 {
			return errors.New("environment variable META_RELAY_REGISTRY_EDGE_SECRET must contain at least 32 bytes")
		}
	}
	if len(c.Accounts) == 0 && !registryConfigured {
		return errors.New("at least one static Meta account mapping or dynamic registry is required")
	}
	if c.InboundRetention <= 0 ||
		c.OutboundRetention <= 0 ||
		c.ProcessingLease <= 0 ||
		c.ForwardTimeout <= 0 ||
		c.PollInterval <= 0 ||
		c.WorkerConcurrency <= 0 ||
		c.MaxAttempts <= 0 {
		return errors.New("relay durations and max attempts must be positive")
	}
	if registryConfigured && (c.RegistryCacheTTL <= 0 || c.RegistryCacheTTL > time.Minute ||
		c.RegistryTimeout <= 0 || c.RegistryTimeout > 10*time.Second) {
		return errors.New("meta registry cache and timeout durations are outside safe bounds")
	}
	if c.WorkerConcurrency > maxWorkerConcurrency {
		return fmt.Errorf("environment variable META_RELAY_WORKER_CONCURRENCY must not exceed %d", maxWorkerConcurrency)
	}
	requiredProcessingLease := c.ForwardTimeout + queueSettlementTimeout + leaseSafetyMargin
	if registryConfigured {
		requiredProcessingLease += c.RegistryTimeout
	}
	if c.ProcessingLease < requiredProcessingLease {
		return errors.New("environment variable META_RELAY_PROCESSING_LEASE must cover registry lookup, forward timeout, and queue settlement")
	}

	c.byExternal = make(map[string]*AccountConfig, len(c.Accounts))
	c.byKey = make(map[string]*AccountConfig, len(c.Accounts))
	for i := range c.Accounts {
		account := &c.Accounts[i]
		account.Key = strings.TrimSpace(account.Key)
		account.ExternalAccountID = strings.TrimSpace(account.ExternalAccountID)
		account.ReReplyWebhookURL = strings.TrimSpace(account.ReReplyWebhookURL)
		if !accountKeyPattern.MatchString(account.Key) {
			return fmt.Errorf("account %d key must be 1-64 safe identifier characters", i)
		}
		switch account.Channel {
		case models.ChannelMessenger:
			if account.InstagramAPIMode != "" {
				return fmt.Errorf("account %q instagram_api_mode is only valid for Instagram", account.Key)
			}
		case models.ChannelInstagram:
			switch account.InstagramAPIMode {
			case InstagramAPIModeInstagramLogin, InstagramAPIModeFacebookLogin:
			default:
				return fmt.Errorf(
					"account %q instagram_api_mode must be instagram_login or facebook_login",
					account.Key,
				)
			}
		default:
			return fmt.Errorf("account %q channel must be messenger or instagram", account.Key)
		}
		if account.ExternalAccountID == "" {
			return fmt.Errorf("account %q external_account_id is required", account.Key)
		}
		if err := validateEndpoint(account.ReReplyWebhookURL, c.allowInsecureTestEndpoints); err != nil {
			return fmt.Errorf("account %q rereply_webhook_url: %w", account.Key, err)
		}
		if _, exists := c.byKey[account.Key]; exists {
			return fmt.Errorf("duplicate account key %q", account.Key)
		}
		externalKey := accountIndexKey(account.Channel, account.ExternalAccountID)
		if _, exists := c.byExternal[externalKey]; exists {
			return fmt.Errorf("duplicate %s account %q", account.Channel, account.ExternalAccountID)
		}

		var err error
		if account.accessToken, err = secretFromEnv(getenv, account.Key, "access_token_env", account.AccessTokenEnv); err != nil {
			return err
		}
		if account.reReplyInboundSecret, err = secretFromEnv(getenv, account.Key, "rereply_inbound_secret_env", account.ReReplyInboundSecretEnv); err != nil {
			return err
		}
		if account.reReplyOutboundSecret, err = secretFromEnv(getenv, account.Key, "rereply_outbound_secret_env", account.ReReplyOutboundSecretEnv); err != nil {
			return err
		}
		c.byKey[account.Key] = account
		c.byExternal[externalKey] = account
	}
	return nil
}

func validateEndpoint(raw string, allowInsecure bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("must be an absolute URL without credentials or fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if allowInsecure && parsed.Scheme == "http" {
		return nil
	}
	return errors.New("https is required")
}

func secretFromEnv(getenv func(string) string, accountKey, field, envName string) (string, error) {
	envName = strings.TrimSpace(envName)
	if !envNamePattern.MatchString(envName) {
		return "", fmt.Errorf("account %q %s must name an environment variable", accountKey, field)
	}
	value := getenv(envName)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("account %q secret environment variable %s is empty", accountKey, envName)
	}
	return value, nil
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
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return number, nil
}

func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values are not allowed")
}
