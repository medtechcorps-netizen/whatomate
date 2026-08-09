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

	"github.com/google/uuid"
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
	queueSettlementTimeout   = 5 * time.Second
	leaseSafetyMargin        = time.Second
	maxWorkerConcurrency     = 64
	maxGovernanceReviewAge   = 90 * 24 * time.Hour
	governanceClockSkew      = 5 * time.Minute
	minimumHMACSecretBytes   = 32
)

var (
	envNamePattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	graphVersionPattern   = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)
	accountKeyPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	metaAppIDPattern      = regexp.MustCompile(`^[1-9][0-9]{5,31}$`)
	metaBusinessIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,31}$`)
	metaPermissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)
	reviewerPattern       = regexp.MustCompile(`^[^<>\r\n]{2,100} <[^<>\s@]+@[^<>\s@]+\.[^<>\s@]+>$`)
	utcReviewTimePattern  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$`)
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
		ReReplyBaseURL:              strings.TrimSpace(getenv("META_RELAY_REREPLY_BASE_URL")),
		ReReplyProviderProofSecret:  getenv("META_RELAY_REREPLY_PROVIDER_PROOF_SECRET"),
		MessengerAppSecret:          getenv("META_RELAY_MESSENGER_APP_SECRET"),
		MessengerAppID:              strings.TrimSpace(getenv("META_RELAY_MESSENGER_APP_ID")),
		MessengerAppMode:            strings.ToLower(strings.TrimSpace(getenv("META_RELAY_MESSENGER_APP_MODE"))),
		MessengerAppOwnerBusinessID: strings.TrimSpace(getenv("META_RELAY_MESSENGER_APP_OWNER_BUSINESS_ID")),
		MessengerTechProviderStatus: strings.TrimSpace(getenv("META_RELAY_MESSENGER_TECH_PROVIDER_STATUS")),
		MessengerAppReviewStatus:    strings.ToLower(strings.TrimSpace(getenv("META_RELAY_MESSENGER_APP_REVIEW_STATUS"))),
		MessengerAppPermissions:     commaSeparatedValues(getenv("META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS")),
		MessengerReviewedBy:         strings.TrimSpace(getenv("META_RELAY_MESSENGER_REVIEWED_BY")),
		MessengerReviewedAt:         strings.TrimSpace(getenv("META_RELAY_MESSENGER_REVIEWED_AT")),
		MessengerReviewEvidence:     strings.TrimSpace(getenv("META_RELAY_MESSENGER_REVIEW_EVIDENCE")),
		MessengerVerifyToken:        getenv("META_RELAY_MESSENGER_VERIFY_TOKEN"),
		InstagramLoginAppSecret:     getenv("META_RELAY_INSTAGRAM_APP_SECRET"),
		InstagramLoginAppID:         strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_APP_ID")),
		InstagramAppMode:            strings.ToLower(strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_APP_MODE"))),
		InstagramAppOwnerBusinessID: strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_APP_OWNER_BUSINESS_ID")),
		InstagramTechProviderStatus: strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_TECH_PROVIDER_STATUS")),
		InstagramAppReviewStatus:    strings.ToLower(strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_APP_REVIEW_STATUS"))),
		InstagramAppPermissions:     commaSeparatedValues(getenv("META_RELAY_INSTAGRAM_APP_REVIEW_PERMISSIONS")),
		InstagramReviewedBy:         strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_REVIEWED_BY")),
		InstagramReviewedAt:         strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_REVIEWED_AT")),
		InstagramReviewEvidence:     strings.TrimSpace(getenv("META_RELAY_INSTAGRAM_REVIEW_EVIDENCE")),
		InstagramLoginVerifyToken:   getenv("META_RELAY_INSTAGRAM_VERIFY_TOKEN"),
		GraphAPIVersion:             strings.TrimSpace(getenv("META_RELAY_GRAPH_API_VERSION")),
		InboundRetention:            defaultInboundRetention,
		OutboundRetention:           defaultOutboundRetention,
		ProcessingLease:             defaultProcessingLease,
		ForwardTimeout:              defaultForwardTimeout,
		PollInterval:                defaultPollInterval,
		WorkerConcurrency:           defaultWorkerConcurrency,
		MaxAttempts:                 defaultMaxAttempts,
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

	rawAccounts := strings.TrimSpace(getenv("META_RELAY_ACCOUNTS_JSON"))
	if rawAccounts == "" {
		return nil, errors.New("environment variable META_RELAY_ACCOUNTS_JSON is required")
	}
	decoder := json.NewDecoder(strings.NewReader(rawAccounts))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config.Accounts); err != nil {
		return nil, errors.New("environment variable META_RELAY_ACCOUNTS_JSON is invalid")
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		return nil, errors.New("environment variable META_RELAY_ACCOUNTS_JSON is invalid")
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
	if !metaAppIDPattern.MatchString(c.MessengerAppID) {
		return errors.New("environment variable META_RELAY_MESSENGER_APP_ID must be a valid numeric Meta app ID")
	}
	if c.MessengerAppMode != "live" {
		return errors.New("environment variable META_RELAY_MESSENGER_APP_MODE must be live")
	}
	if err := validateAppGovernanceEvidence(
		"META_RELAY_MESSENGER",
		c.MessengerAppOwnerBusinessID,
		c.MessengerTechProviderStatus,
		c.MessengerReviewedBy,
		c.MessengerReviewedAt,
		c.MessengerReviewEvidence,
	); err != nil {
		return err
	}
	if c.MessengerAppReviewStatus != "approved" {
		return errors.New("environment variable META_RELAY_MESSENGER_APP_REVIEW_STATUS must be approved")
	}
	if err := validatePermissionEvidence("META_RELAY_MESSENGER_APP_REVIEW_PERMISSIONS", c.MessengerAppPermissions); err != nil {
		return err
	}
	if strings.TrimSpace(c.MessengerVerifyToken) == "" {
		return errors.New("environment variable META_RELAY_MESSENGER_VERIFY_TOKEN is required")
	}
	if strings.TrimSpace(c.InstagramLoginAppSecret) == "" {
		return errors.New("environment variable META_RELAY_INSTAGRAM_APP_SECRET is required")
	}
	if !metaAppIDPattern.MatchString(c.InstagramLoginAppID) {
		return errors.New("environment variable META_RELAY_INSTAGRAM_APP_ID must be a valid numeric Meta app ID")
	}
	if c.InstagramAppMode != "live" {
		return errors.New("environment variable META_RELAY_INSTAGRAM_APP_MODE must be live")
	}
	if err := validateAppGovernanceEvidence(
		"META_RELAY_INSTAGRAM",
		c.InstagramAppOwnerBusinessID,
		c.InstagramTechProviderStatus,
		c.InstagramReviewedBy,
		c.InstagramReviewedAt,
		c.InstagramReviewEvidence,
	); err != nil {
		return err
	}
	if c.InstagramAppReviewStatus != "approved" {
		return errors.New("environment variable META_RELAY_INSTAGRAM_APP_REVIEW_STATUS must be approved")
	}
	if err := validatePermissionEvidence("META_RELAY_INSTAGRAM_APP_REVIEW_PERMISSIONS", c.InstagramAppPermissions); err != nil {
		return err
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
	if c.MessengerAppID == c.InstagramLoginAppID {
		return errors.New("messenger and Instagram app IDs must be distinct")
	}
	if err := validateHMACSecretValue(
		"environment variable META_RELAY_REREPLY_PROVIDER_PROOF_SECRET",
		c.ReReplyProviderProofSecret,
	); err != nil {
		return err
	}
	for label, value := range map[string]string{
		"Messenger app secret":   c.MessengerAppSecret,
		"Instagram app secret":   c.InstagramLoginAppSecret,
		"Messenger verify token": c.MessengerVerifyToken,
		"Instagram verify token": c.InstagramLoginVerifyToken,
	} {
		if c.ReReplyProviderProofSecret == value {
			return fmt.Errorf("meta provider proof secret must be distinct from %s", label)
		}
	}
	if !graphVersionPattern.MatchString(c.GraphAPIVersion) {
		return errors.New("environment variable META_RELAY_GRAPH_API_VERSION must be explicit, for example v25.0")
	}
	if len(c.Accounts) == 0 {
		return errors.New("at least one Meta account mapping is required")
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
	if c.WorkerConcurrency > maxWorkerConcurrency {
		return fmt.Errorf("environment variable META_RELAY_WORKER_CONCURRENCY must not exceed %d", maxWorkerConcurrency)
	}
	if c.ProcessingLease < c.ForwardTimeout+queueSettlementTimeout+leaseSafetyMargin {
		return errors.New("environment variable META_RELAY_PROCESSING_LEASE must cover forward timeout and queue settlement")
	}
	reReplyBase, err := validateReReplyBaseURL(c.ReReplyBaseURL, c.allowInsecureTestEndpoints)
	if err != nil {
		return fmt.Errorf("environment variable META_RELAY_REREPLY_BASE_URL: %w", err)
	}

	c.byExternal = make(map[string]*AccountConfig, len(c.Accounts))
	c.byKey = make(map[string]*AccountConfig, len(c.Accounts))
	seenChannelAccountIDs := make(map[string]string, len(c.Accounts))
	seenWebhookURLs := make(map[string]string, len(c.Accounts))
	seenHMACEnvNames := make(map[string]string, len(c.Accounts)*2)
	seenHMACValues := make(map[string]string, len(c.Accounts)*2)
	organizationBusinesses := make(map[string]string, len(c.Accounts))
	for i := range c.Accounts {
		account := &c.Accounts[i]
		account.Key = strings.TrimSpace(account.Key)
		account.ExternalAccountID = strings.TrimSpace(account.ExternalAccountID)
		account.ReReplyWebhookURL = strings.TrimSpace(account.ReReplyWebhookURL)
		if !accountKeyPattern.MatchString(account.Key) {
			return fmt.Errorf("account %d key must be 1-64 safe identifier characters", i)
		}
		organizationID, organizationErr := canonicalOrganizationID(account.OrganizationID)
		if organizationErr != nil {
			return fmt.Errorf("account %q organization_id: %w", account.Key, organizationErr)
		}
		account.OrganizationID = organizationID
		if !metaBusinessIDPattern.MatchString(account.MetaBusinessID) {
			return fmt.Errorf("account %q meta_business_id must be a valid numeric Meta Business Portfolio ID", account.Key)
		}
		if prior, exists := organizationBusinesses[account.OrganizationID]; exists && prior != account.MetaBusinessID {
			return fmt.Errorf("organization %q is bound to multiple Meta Business IDs", account.OrganizationID)
		}
		organizationBusinesses[account.OrganizationID] = account.MetaBusinessID
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
		for _, permission := range requiredAppReviewPermissions(account) {
			if !containsExact(c.appReviewPermissions(account.webhookApp()), permission) {
				return fmt.Errorf(
					"account %q requires approved Meta App Review permission %s",
					account.Key,
					permission,
				)
			}
		}
		if !metaBusinessIDPattern.MatchString(account.ExternalAccountID) {
			return fmt.Errorf("account %q external_account_id must be a valid numeric Meta asset ID", account.Key)
		}
		if err := validateEndpoint(account.ReReplyWebhookURL, c.allowInsecureTestEndpoints); err != nil {
			return fmt.Errorf("account %q rereply_webhook_url: %w", account.Key, err)
		}
		channelAccountID, parseErr := reReplyChannelAccountID(account.ReReplyWebhookURL)
		if parseErr != nil {
			return fmt.Errorf("account %q rereply_webhook_url: %w", account.Key, parseErr)
		}
		account.reReplyChannelAccountID = channelAccountID
		expectedWebhookURL := exactReReplyWebhookURL(reReplyBase, channelAccountID)
		if account.ReReplyWebhookURL != expectedWebhookURL {
			return fmt.Errorf("account %q rereply_webhook_url must use the configured ReReply production origin", account.Key)
		}
		if prior, exists := seenChannelAccountIDs[channelAccountID]; exists {
			return fmt.Errorf("accounts %q and %q reuse ReReply channel account ID %s", prior, account.Key, channelAccountID)
		}
		if prior, exists := seenWebhookURLs[account.ReReplyWebhookURL]; exists {
			return fmt.Errorf("accounts %q and %q reuse rereply_webhook_url", prior, account.Key)
		}
		seenChannelAccountIDs[channelAccountID] = account.Key
		seenWebhookURLs[account.ReReplyWebhookURL] = account.Key
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
		if account.reReplyInboundSecret, err = hmacSecretFromEnv(getenv, account.Key, "rereply_inbound_secret_env", account.ReReplyInboundSecretEnv); err != nil {
			return err
		}
		if account.reReplyOutboundSecret, err = hmacSecretFromEnv(getenv, account.Key, "rereply_outbound_secret_env", account.ReReplyOutboundSecretEnv); err != nil {
			return err
		}
		for label, value := range map[string]string{
			"access token":         account.accessToken,
			"inbound HMAC secret":  account.reReplyInboundSecret,
			"outbound HMAC secret": account.reReplyOutboundSecret,
		} {
			if c.ReReplyProviderProofSecret == value {
				return fmt.Errorf("account %q %s must differ from the deployment Meta provider proof secret", account.Key, label)
			}
		}
		for field, envName := range map[string]string{
			"rereply_inbound_secret_env":  strings.TrimSpace(account.ReReplyInboundSecretEnv),
			"rereply_outbound_secret_env": strings.TrimSpace(account.ReReplyOutboundSecretEnv),
		} {
			if prior, exists := seenHMACEnvNames[envName]; exists {
				return fmt.Errorf("account %q %s reuses account-scoped HMAC environment variable already used by %s", account.Key, field, prior)
			}
			seenHMACEnvNames[envName] = account.Key + " " + field
		}
		for field, value := range map[string]string{
			"rereply_inbound_secret":  account.reReplyInboundSecret,
			"rereply_outbound_secret": account.reReplyOutboundSecret,
		} {
			if prior, exists := seenHMACValues[value]; exists {
				return fmt.Errorf("account %q %s reuses account-scoped HMAC secret material already used by %s", account.Key, field, prior)
			}
			seenHMACValues[value] = account.Key + " " + field
		}
		c.byKey[account.Key] = account
		c.byExternal[externalKey] = account
	}
	return nil
}

func commaSeparatedValues(raw string) []string {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		value := strings.ToLower(strings.TrimSpace(part))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

func validateAppGovernanceEvidence(
	prefix, ownerBusinessID, techProviderStatus, reviewedBy, reviewedAt, reviewEvidence string,
) error {
	if !metaBusinessIDPattern.MatchString(ownerBusinessID) {
		return fmt.Errorf("environment variable %s_APP_OWNER_BUSINESS_ID must be a valid numeric Meta Business Portfolio ID", prefix)
	}
	if techProviderStatus != "verified" {
		return fmt.Errorf("environment variable %s_TECH_PROVIDER_STATUS must be verified", prefix)
	}
	if !reviewerPattern.MatchString(reviewedBy) {
		return fmt.Errorf("environment variable %s_REVIEWED_BY must use Name <email> format", prefix)
	}
	if err := validateGovernanceReviewTime(prefix, reviewedAt, time.Now().UTC()); err != nil {
		return err
	}
	parsedEvidence, err := url.Parse(reviewEvidence)
	if err != nil ||
		!strings.EqualFold(parsedEvidence.Scheme, "https") ||
		parsedEvidence.Hostname() == "" ||
		parsedEvidence.User != nil ||
		parsedEvidence.RawQuery != "" ||
		parsedEvidence.Fragment != "" {
		return fmt.Errorf("environment variable %s_REVIEW_EVIDENCE must be an HTTPS URL without credentials, query, or fragment", prefix)
	}
	return nil
}

func validateGovernanceReviewTime(prefix, reviewedAt string, now time.Time) error {
	if !utcReviewTimePattern.MatchString(reviewedAt) {
		return fmt.Errorf("environment variable %s_REVIEWED_AT must be a UTC RFC3339 timestamp", prefix)
	}
	reviewedTime, err := time.Parse(time.RFC3339Nano, reviewedAt)
	if err != nil {
		return fmt.Errorf("environment variable %s_REVIEWED_AT must be a UTC RFC3339 timestamp", prefix)
	}
	now = now.UTC()
	if reviewedTime.After(now.Add(governanceClockSkew)) {
		return fmt.Errorf("environment variable %s_REVIEWED_AT must not be future-dated", prefix)
	}
	if reviewedTime.Before(now.Add(-maxGovernanceReviewAge)) {
		return fmt.Errorf("environment variable %s_REVIEWED_AT is stale and must be renewed within %s", prefix, maxGovernanceReviewAge)
	}
	return nil
}

func (c *Config) validateCurrentGovernance(account *AccountConfig, now time.Time) error {
	if c == nil || account == nil {
		return errors.New("meta governance configuration is unavailable")
	}
	switch account.webhookApp() {
	case WebhookAppMessenger:
		return validateGovernanceReviewTime(
			"META_RELAY_MESSENGER",
			c.MessengerReviewedAt,
			now,
		)
	case WebhookAppInstagramLogin:
		return validateGovernanceReviewTime(
			"META_RELAY_INSTAGRAM",
			c.InstagramReviewedAt,
			now,
		)
	default:
		return errors.New("meta governance application binding is invalid")
	}
}

func canonicalOrganizationID(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", errors.New("must be a canonical non-zero UUID")
	}
	organizationID, err := uuid.Parse(raw)
	if err != nil || organizationID == uuid.Nil || organizationID.String() != raw {
		return "", errors.New("must be a canonical non-zero UUID")
	}
	return raw, nil
}

func validatePermissionEvidence(name string, permissions []string) error {
	if len(permissions) == 0 {
		return fmt.Errorf("environment variable %s is required", name)
	}
	for _, permission := range permissions {
		if !metaPermissionPattern.MatchString(permission) {
			return fmt.Errorf("environment variable %s contains an invalid permission name", name)
		}
	}
	return nil
}

func requiredAppReviewPermissions(account *AccountConfig) []string {
	if account == nil {
		return nil
	}
	if account.Channel == models.ChannelMessenger {
		return []string{"pages_messaging", "pages_manage_metadata"}
	}
	if account.InstagramAPIMode == InstagramAPIModeInstagramLogin {
		return []string{"instagram_business_basic", "instagram_business_manage_messages"}
	}
	return []string{"instagram_basic", "instagram_manage_messages", "pages_manage_metadata", "pages_show_list"}
}

func containsExact(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func reReplyChannelAccountID(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.RawQuery != "" {
		return "", errors.New("must be the exact channel webhook URL without a query")
	}
	const prefix = "/api/webhooks/channels/"
	if !strings.HasPrefix(parsed.EscapedPath(), prefix) {
		return "", errors.New("must use /api/webhooks/channels/{channel_account_id}")
	}
	rawID := strings.TrimPrefix(parsed.EscapedPath(), prefix)
	if rawID == "" || strings.Contains(rawID, "/") {
		return "", errors.New("must use /api/webhooks/channels/{channel_account_id}")
	}
	accountID, err := uuid.Parse(rawID)
	if err != nil || accountID == uuid.Nil {
		return "", errors.New("must contain a valid channel account ID")
	}
	return accountID.String(), nil
}

func validateReReplyBaseURL(raw string, allowInsecure bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil ||
		parsed.Hostname() == "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" ||
		parsed.RawPath != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("must be an absolute origin without path, credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && (!allowInsecure || parsed.Scheme != "http") {
		return nil, errors.New("https is required")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed, nil
}

func exactReReplyWebhookURL(base *url.URL, channelAccountID string) string {
	if base == nil {
		return ""
	}
	exact := *base
	exact.Path = "/api/webhooks/channels/" + channelAccountID
	return exact.String()
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

func hmacSecretFromEnv(
	getenv func(string) string,
	accountKey, field, envName string,
) (string, error) {
	value, err := secretFromEnv(getenv, accountKey, field, envName)
	if err != nil {
		return "", err
	}
	if err := validateHMACSecretValue(
		fmt.Sprintf("account %q secret environment variable %s", accountKey, strings.TrimSpace(envName)),
		value,
	); err != nil {
		return "", err
	}
	return value, nil
}

func validateHMACSecretValue(name, value string) error {
	if len([]byte(value)) < minimumHMACSecretBytes || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must contain at least %d bytes and no surrounding whitespace", name, minimumHMACSecretBytes)
	}
	return nil
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
