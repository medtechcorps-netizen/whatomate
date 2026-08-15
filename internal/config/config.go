package config

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by the coturn TURN REST API (RFC draft)
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
)

// Config holds all configuration for the application
type Config struct {
	App                 AppConfig                 `koanf:"app"`
	Server              ServerConfig              `koanf:"server"`
	Database            DatabaseConfig            `koanf:"database"`
	Redis               RedisConfig               `koanf:"redis"`
	JWT                 JWTConfig                 `koanf:"jwt"`
	WhatsApp            WhatsAppConfig            `koanf:"whatsapp"`
	AI                  AIConfig                  `koanf:"ai"`
	Storage             StorageConfig             `koanf:"storage"`
	DefaultAdmin        DefaultAdminConfig        `koanf:"default_admin"`
	RateLimit           RateLimitConfig           `koanf:"rate_limit"`
	Cookie              CookieConfig              `koanf:"cookie"`
	Calling             CallingConfig             `koanf:"calling"`
	TTS                 TTSConfig                 `koanf:"tts"`
	GoogleSearchConsole GoogleSearchConsoleConfig `koanf:"google_search_console"`
	MetaRegistry        MetaRegistryConfig        `koanf:"meta_registry"`
	MetaMessenger       MetaMessengerConfig       `koanf:"meta_messenger_onboarding"`
	ThreadsAppReview    ThreadsAppReviewConfig    `koanf:"threads_app_review"`
}

// ThreadsAppReviewConfig is a deployment-owned escape hatch used only to
// record Meta App Review demonstrations in a non-production environment. It
// binds the exception to one workspace, one Meta app, and one app-role Threads
// profile. Production must use server-recorded approval evidence instead.
type ThreadsAppReviewConfig struct {
	DevelopmentTestingEnabled bool   `koanf:"development_testing_enabled"`
	DevelopmentOrganizationID string `koanf:"development_organization_id"`
	DevelopmentAppID          string `koanf:"development_app_id"`
	DevelopmentProfileID      string `koanf:"development_profile_id"`
}

// MetaRegistryConfig protects the private broker used by the isolated Meta
// relay. ServiceSecret is deployment-managed and never tenant configurable.
type MetaRegistryConfig struct {
	Enabled             bool   `koanf:"enabled"`
	ServiceSecret       string `koanf:"service_secret"`
	RelayEdgeSecret     string `koanf:"relay_edge_secret"`
	LeaseSeconds        int    `koanf:"lease_seconds"`
	OwnershipMaxAgeMins int    `koanf:"ownership_max_age_mins"`
	ReplayWindowSeconds int    `koanf:"replay_window_seconds"`
	QueueReaderVersion  int    `koanf:"queue_reader_version"`
}

// MetaMessengerConfig is the deployment-owned platform-app configuration for
// Facebook Login for Business. It is deliberately default-off and contains no
// tenant-selectable app credentials.
type MetaMessengerConfig struct {
	Enabled                   bool   `koanf:"enabled"`
	AppID                     string `koanf:"app_id"`
	ConfigID                  string `koanf:"config_id"`
	AppSecret                 string `koanf:"app_secret"`
	GraphAPIVersion           string `koanf:"graph_api_version"`
	GraphBaseURL              string `koanf:"graph_base_url"`
	ReReplyBaseURL            string `koanf:"rereply_base_url"`
	RelayBaseURL              string `koanf:"relay_base_url"`
	HealthApprovalMaxAgeMins  int    `koanf:"health_approval_max_age_mins"`
	RevalidationLeadMins      int    `koanf:"revalidation_lead_mins"`
	SchedulerIntervalSeconds  int    `koanf:"scheduler_interval_seconds"`
	AllowedOrganizationIDs    string `koanf:"allowed_organization_ids"`
	AllowAllOrganizations     bool   `koanf:"allow_all_organizations"`
	AllowDevelopmentUserToken bool   `koanf:"allow_development_user_token"`
}

// GoogleSearchConsoleConfig contains deployment-managed OAuth credentials.
// Tenants authorize access but never supply or receive these platform secrets.
// AuthURL, TokenURL, and APIBaseURL are configurable for isolated tests and
// private proxies; production deployments should use the defaults.
type GoogleSearchConsoleConfig struct {
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
	RedirectURL  string `koanf:"redirect_url"`
	AuthURL      string `koanf:"auth_url"`
	TokenURL     string `koanf:"token_url"`
	APIBaseURL   string `koanf:"api_base_url"`
}

type TTSConfig struct {
	PiperBinary   string `koanf:"piper_binary"`   // path to piper executable
	PiperModel    string `koanf:"piper_model"`    // path to .onnx voice model
	OpusencBinary string `koanf:"opusenc_binary"` // path to opusenc (defaults to "opusenc")
}

// defaultTURNCredentialTTLSecs is the lifetime of generated coturn REST
// credentials when an ICE server has a secret but no explicit credential_ttl.
const defaultTURNCredentialTTLSecs = 86400 // 24h

type ICEServerConfig struct {
	URLs       []string `koanf:"urls"`
	Username   string   `koanf:"username"`
	Credential string   `koanf:"credential"`
	// Secret enables coturn's "use-auth-secret" (TURN REST API) mode. When set,
	// short-lived credentials are generated per request instead of using the
	// static Username/Credential above.
	Secret string `koanf:"secret"`
	// CredentialTTL is the lifetime (seconds) of generated credentials. Defaults
	// to defaultTURNCredentialTTLSecs when <= 0. Only used when Secret is set.
	CredentialTTL int `koanf:"credential_ttl"`
}

// ResolveCredentials returns the username and credential to advertise for this
// ICE server. When Secret is set it derives short-lived coturn REST credentials
// (use-auth-secret): the username is "<expiry-unix>" — optionally prefixed onto
// any configured Username as "<expiry-unix>:<username>" — and the credential is
// the base64-encoded HMAC-SHA1 of that username keyed by the secret. When Secret
// is empty, the static Username/Credential are returned unchanged.
func (s ICEServerConfig) ResolveCredentials(now time.Time) (username, credential string) {
	if s.Secret == "" {
		return s.Username, s.Credential
	}

	ttl := s.CredentialTTL
	if ttl <= 0 {
		ttl = defaultTURNCredentialTTLSecs
	}

	expiry := now.Add(time.Duration(ttl) * time.Second).Unix()
	username = strconv.FormatInt(expiry, 10)
	if s.Username != "" {
		username = username + ":" + s.Username
	}

	mac := hmac.New(sha1.New, []byte(s.Secret))
	mac.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, credential
}

type CallingConfig struct {
	MaxCallDuration     int               `koanf:"max_call_duration"`
	AudioDir            string            `koanf:"audio_dir"`
	HoldMusicFile       string            `koanf:"hold_music_file"`
	TransferTimeoutSecs int               `koanf:"transfer_timeout_secs"`
	PerAgentTimeoutSecs int               `koanf:"per_agent_timeout_secs"`
	RingbackFile        string            `koanf:"ringback_file"`
	UDPPortMin          uint16            `koanf:"udp_port_min"` // WebRTC UDP port range start (default: 10000)
	UDPPortMax          uint16            `koanf:"udp_port_max"` // WebRTC UDP port range end (default: 10100)
	PublicIP            string            `koanf:"public_ip"`    // Public IP for NAT mapping (required on AWS/cloud)
	RelayOnly           bool              `koanf:"relay_only"`   // Force all media through TURN relay (no direct UDP)
	ICEServers          []ICEServerConfig `koanf:"ice_servers"`
	RecordingEnabled    bool              `koanf:"recording_enabled"` // Enable call recording to S3
}

type AppConfig struct {
	Name          string `koanf:"name"`
	Environment   string `koanf:"environment"` // development, staging, production
	Debug         bool   `koanf:"debug"`
	EncryptionKey string `koanf:"encryption_key"` // AES-256 key for encrypting secrets at rest
}

type ServerConfig struct {
	Host           string `koanf:"host"`
	Port           int    `koanf:"port"`
	ReadTimeout    int    `koanf:"read_timeout"`
	WriteTimeout   int    `koanf:"write_timeout"`
	BasePath       string `koanf:"base_path"`       // Base path for frontend (e.g., "/whatomate" for proxy pass)
	AllowedOrigins string `koanf:"allowed_origins"` // Comma-separated list of allowed CORS origins
}

type DatabaseConfig struct {
	URL             string `koanf:"url"`
	MigrationURL    string `koanf:"migration_url"` // Owner connection used only by pre-deploy migrations.
	RuntimeRole     string `koanf:"runtime_role"`  // Restricted PostgreSQL role targeted by RLS policies.
	RLSEnabled      bool   `koanf:"rls_enabled"`   // Require tenant-scoped transactions for CRM queries.
	Host            string `koanf:"host"`
	Port            int    `koanf:"port"`
	User            string `koanf:"user"`
	Password        string `koanf:"password"`
	Name            string `koanf:"name"`
	SSLMode         string `koanf:"ssl_mode"`
	MaxOpenConns    int    `koanf:"max_open_conns"`
	MaxIdleConns    int    `koanf:"max_idle_conns"`
	ConnMaxLifetime int    `koanf:"conn_max_lifetime"`
}

type RedisConfig struct {
	URL      string `koanf:"url"`
	Host     string `koanf:"host"`
	Port     int    `koanf:"port"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	DB       int    `koanf:"db"`
	TLS      bool   `koanf:"tls"`
}

type JWTConfig struct {
	Secret            string `koanf:"secret"`
	AccessExpiryMins  int    `koanf:"access_expiry_mins"`
	RefreshExpiryDays int    `koanf:"refresh_expiry_days"`
}

type WhatsAppConfig struct {
	WebhookVerifyToken string `koanf:"webhook_verify_token"`
	APIVersion         string `koanf:"api_version"`
	BaseURL            string `koanf:"base_url"` // Meta Graph API base URL
	AppID              string `koanf:"app_id"`   // WhatsApp App ID for frontend
	AppSecret          string `koanf:"app_secret"`
	ConfigID           string `koanf:"config_id"` // WhatsApp Config ID for frontend
}

type AIConfig struct {
	OpenAIKey    string `koanf:"openai_key"`
	AnthropicKey string `koanf:"anthropic_key"`
	GoogleKey    string `koanf:"google_key"`
	QwenBaseURL  string `koanf:"qwen_base_url"`
}

type StorageConfig struct {
	Type           string `koanf:"type"` // local, s3
	LocalPath      string `koanf:"local_path"`
	S3Bucket       string `koanf:"s3_bucket"`
	S3Region       string `koanf:"s3_region"`
	S3Key          string `koanf:"s3_key"`
	S3Secret       string `koanf:"s3_secret"`
	S3Endpoint     string `koanf:"s3_endpoint"`       // Optional for S3-compatible providers such as Railway.
	S3UsePathStyle bool   `koanf:"s3_use_path_style"` // Required only by providers that do not use virtual-hosted URLs.
}

type DefaultAdminConfig struct {
	Email    string `koanf:"email"`
	Password string `koanf:"password"`
	FullName string `koanf:"full_name"`
}

type CookieConfig struct {
	Domain string `koanf:"domain"` // Cookie domain (e.g., ".example.com"). Empty = current host.
	Secure bool   `koanf:"secure"` // Set Secure flag. Auto-set true when environment=production.
}

type RateLimitConfig struct {
	Enabled             bool `koanf:"enabled"`
	LoginMaxAttempts    int  `koanf:"login_max_attempts"`
	RegisterMaxAttempts int  `koanf:"register_max_attempts"`
	RefreshMaxAttempts  int  `koanf:"refresh_max_attempts"`
	SSOMaxAttempts      int  `koanf:"sso_max_attempts"`
	WindowSeconds       int  `koanf:"window_seconds"`
	TrustProxy          bool `koanf:"trust_proxy"`
	APIMaxRequests      int  `koanf:"api_max_requests"`
	APIWindowSeconds    int  `koanf:"api_window_seconds"`
}

// Load loads configuration from file and environment variables
func Load(configPath string) (*Config, error) {
	k := koanf.New(".")

	// Load from config file if provided
	if configPath != "" {
		if err := k.Load(file.Provider(configPath), toml.Parser()); err != nil {
			return nil, err
		}
	}

	// Load from environment variables (WHATOMATE_ prefix). A DOUBLE underscore
	// separates config levels; single underscores are preserved as part of the
	// key. This is required because both section and field names contain
	// underscores (e.g. default_admin, rate_limit, whatsapp.app_id) — collapsing
	// every "_" to "." would mangle them (whatsapp.app_id -> whatsapp.app.id), so
	// those keys could never be set via env.
	// e.g. WHATOMATE_DATABASE__HOST -> database.host
	//      WHATOMATE_WHATSAPP__APP_ID -> whatsapp.app_id
	//      WHATOMATE_DEFAULT_ADMIN__EMAIL -> default_admin.email
	if err := k.Load(env.Provider("WHATOMATE_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "WHATOMATE_")), "__", ".")
	}), nil); err != nil {
		return nil, err
	}

	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, err
	}

	// Set defaults
	setDefaults(&cfg)
	if err := validateMetaRegistryConfig(cfg.MetaRegistry, cfg.MetaMessenger, cfg.App); err != nil {
		return nil, err
	}
	if err := validateMetaMessengerConfig(cfg.MetaMessenger, cfg.MetaRegistry, cfg.App.Environment, cfg.Server); err != nil {
		return nil, err
	}
	if err := validateThreadsAppReviewConfig(cfg.ThreadsAppReview, cfg.App.Environment); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validateThreadsAppReviewConfig(config ThreadsAppReviewConfig, environment string) error {
	organizationID := strings.TrimSpace(config.DevelopmentOrganizationID)
	appID := strings.TrimSpace(config.DevelopmentAppID)
	profileID := strings.TrimSpace(config.DevelopmentProfileID)
	configured := config.DevelopmentTestingEnabled || organizationID != "" || appID != "" || profileID != ""
	production := strings.EqualFold(strings.TrimSpace(environment), "production")
	if production && configured {
		return errors.New("threads development testing gate is forbidden in production")
	}
	if !config.DevelopmentTestingEnabled {
		if configured {
			return errors.New("threads development testing must be explicitly enabled when its allowlist is configured")
		}
		return nil
	}
	if organizationID != config.DevelopmentOrganizationID || !canonicalUUID(organizationID) {
		return errors.New("threads development organization_id must be a canonical UUID")
	}
	if appID != config.DevelopmentAppID || profileID != config.DevelopmentProfileID ||
		!canonicalNumericMetaID(appID) || !canonicalNumericMetaID(profileID) {
		return errors.New("threads development app_id and profile_id must be canonical numeric Meta IDs")
	}
	return nil
}

func validateMetaRegistryConfig(config MetaRegistryConfig, messenger MetaMessengerConfig, app AppConfig) error {
	serviceSecret := strings.TrimSpace(config.ServiceSecret)
	edgeSecret := strings.TrimSpace(config.RelayEdgeSecret)
	if serviceSecret != "" && len(serviceSecret) < 32 {
		return errors.New("meta registry service secret must contain at least 32 bytes")
	}
	if edgeSecret != "" && len(edgeSecret) < 32 {
		return errors.New("meta registry relay edge secret must contain at least 32 bytes")
	}
	if (serviceSecret == "") != (edgeSecret == "") {
		return errors.New("meta registry service and relay edge secrets must be configured together")
	}
	if config.LeaseSeconds < 5 || config.LeaseSeconds > 300 ||
		config.OwnershipMaxAgeMins < 1 || config.OwnershipMaxAgeMins > 7*24*60 ||
		time.Duration(config.ReplayWindowSeconds)*time.Second < metaregistry.ReplayWindowFloor ||
		config.ReplayWindowSeconds > 600 {
		return errors.New("meta registry timing configuration is outside safe bounds")
	}
	configured := serviceSecret != "" || edgeSecret != "" || config.Enabled
	if !config.Enabled && configured {
		return errors.New("meta registry must be explicitly enabled when service credentials are configured")
	}
	if config.Enabled {
		if serviceSecret == "" || edgeSecret == "" {
			return errors.New("meta registry service and relay edge secrets are required when enabled")
		}
		if !messenger.Enabled {
			return errors.New("meta registry cannot be enabled before the Messenger lifecycle is enabled")
		}
		if config.QueueReaderVersion != 2 {
			return errors.New("meta registry requires queue reader schema version 2 before producers are enabled")
		}
		if strings.TrimSpace(app.EncryptionKey) == "" {
			return errors.New("app encryption key is required when the Meta registry is enabled")
		}
	}
	return nil
}

var metaGraphVersionPattern = regexp.MustCompile(`^v[1-9][0-9]*\.[0-9]+$`)

func validateMetaMessengerConfig(
	config MetaMessengerConfig,
	registry MetaRegistryConfig,
	environment string,
	server ServerConfig,
) error {
	if !config.Enabled {
		if config.AppID != "" || config.ConfigID != "" || config.AppSecret != "" ||
			config.ReReplyBaseURL != "" || config.RelayBaseURL != "" ||
			strings.TrimSpace(config.AllowedOrganizationIDs) != "" || config.AllowAllOrganizations ||
			config.AllowDevelopmentUserToken {
			return errors.New("messenger onboarding must be explicitly enabled when platform credentials or endpoints are configured")
		}
		return nil
	}
	if !registry.Enabled {
		return errors.New("messenger onboarding requires the encrypted Meta registry lifecycle")
	}
	if server.WriteTimeout < 120 {
		return errors.New("messenger onboarding requires server.write_timeout of at least 120 seconds")
	}
	allowedOrganizations, err := canonicalMetaMessengerOrganizationAllowlist(config.AllowedOrganizationIDs)
	if err != nil {
		return err
	}
	if config.AllowAllOrganizations == (len(allowedOrganizations) > 0) {
		return errors.New("messenger onboarding requires exactly one of an organization allowlist or allow_all_organizations")
	}
	production := strings.EqualFold(strings.TrimSpace(environment), "production")
	if config.AllowAllOrganizations && !production {
		return errors.New("messenger onboarding allow_all_organizations is reserved for an explicit production release")
	}
	if config.AllowDevelopmentUserToken && (production || config.AllowAllOrganizations) {
		return errors.New("messenger onboarding USER tokens are permitted only for an allowlisted non-production pilot")
	}
	if !canonicalNumericMetaID(config.AppID) || !canonicalNumericMetaID(config.ConfigID) {
		return errors.New("messenger onboarding app_id and config_id must be canonical numeric Meta IDs")
	}
	if len(config.AppSecret) < 32 || strings.TrimSpace(config.AppSecret) != config.AppSecret {
		return errors.New("messenger onboarding app_secret must contain at least 32 bytes without surrounding whitespace")
	}
	if !metaGraphVersionPattern.MatchString(strings.TrimSpace(config.GraphAPIVersion)) {
		return errors.New("messenger onboarding graph_api_version must be explicit, for example v25.0")
	}
	if err := validateMetaLifecycleBaseURL(config.GraphBaseURL, production, true); err != nil {
		return errors.New("messenger onboarding graph_base_url must be an approved absolute HTTPS origin")
	}
	if err := validateMetaLifecycleBaseURL(config.ReReplyBaseURL, production, false); err != nil {
		return errors.New("messenger onboarding rereply_base_url must be an absolute HTTPS base URL")
	}
	if err := validateMetaLifecycleBaseURL(config.RelayBaseURL, production, false); err != nil {
		return errors.New("messenger onboarding relay_base_url must be an absolute HTTPS base URL")
	}
	if config.HealthApprovalMaxAgeMins < 1 || config.HealthApprovalMaxAgeMins > 60 ||
		config.RevalidationLeadMins < 5 || config.RevalidationLeadMins >= registry.OwnershipMaxAgeMins ||
		config.SchedulerIntervalSeconds < 10 || config.SchedulerIntervalSeconds > 300 {
		return errors.New("messenger onboarding lifecycle timing configuration is outside safe bounds")
	}
	return nil
}

func canonicalMetaMessengerOrganizationAllowlist(raw string) ([]string, error) {
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !canonicalUUID(item) {
			return nil, errors.New("messenger onboarding allowed_organization_ids must contain canonical UUIDs")
		}
		if _, exists := seen[item]; exists {
			return nil, errors.New("messenger onboarding allowed_organization_ids contains a duplicate UUID")
		}
		seen[item] = struct{}{}
		values = append(values, item)
	}
	return values, nil
}

func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
	}
	return true
}

func canonicalNumericMetaID(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validateMetaLifecycleBaseURL(raw string, production, graph bool) error {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || trimmed == "" || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		parsed.ForceQuery || parsed.Opaque != "" {
		return errors.New("invalid URL")
	}
	if graph {
		if parsed.Path != "" && parsed.Path != "/" {
			return errors.New("graph URL must be an origin")
		}
		if production && (trimmed != "https://graph.facebook.com" || parsed.Host != "graph.facebook.com") {
			return errors.New("production Graph origin is not pinned")
		}
	}
	return nil
}

func setDefaults(cfg *Config) {
	if cfg.App.Name == "" {
		cfg.App.Name = "ReReply"
	}
	if cfg.App.Environment == "" {
		cfg.App.Environment = "development"
	}
	if cfg.MetaRegistry.LeaseSeconds == 0 {
		cfg.MetaRegistry.LeaseSeconds = 30
	}
	if cfg.MetaRegistry.OwnershipMaxAgeMins == 0 {
		cfg.MetaRegistry.OwnershipMaxAgeMins = 24 * 60
	}
	if cfg.MetaRegistry.ReplayWindowSeconds == 0 {
		cfg.MetaRegistry.ReplayWindowSeconds = 5 * 60
	}
	if cfg.MetaMessenger.GraphAPIVersion == "" {
		cfg.MetaMessenger.GraphAPIVersion = "v25.0"
	}
	if cfg.MetaMessenger.GraphBaseURL == "" {
		cfg.MetaMessenger.GraphBaseURL = "https://graph.facebook.com"
	}
	if cfg.MetaMessenger.HealthApprovalMaxAgeMins == 0 {
		cfg.MetaMessenger.HealthApprovalMaxAgeMins = 15
	}
	if cfg.MetaMessenger.RevalidationLeadMins == 0 {
		cfg.MetaMessenger.RevalidationLeadMins = 60
	}
	if cfg.MetaMessenger.SchedulerIntervalSeconds == 0 {
		cfg.MetaMessenger.SchedulerIntervalSeconds = 60
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.ReadTimeout == 0 {
		cfg.Server.ReadTimeout = 30
	}
	if cfg.Server.WriteTimeout == 0 {
		cfg.Server.WriteTimeout = 30
	}
	if cfg.Database.Port == 0 {
		cfg.Database.Port = 5432
	}
	if cfg.Database.RuntimeRole == "" {
		cfg.Database.RuntimeRole = "rereply_app"
	}
	if cfg.Database.SSLMode == "" {
		cfg.Database.SSLMode = "disable"
	}
	if cfg.Database.MaxOpenConns == 0 {
		cfg.Database.MaxOpenConns = 25
	}
	if cfg.Database.MaxIdleConns == 0 {
		cfg.Database.MaxIdleConns = 5
	}
	if cfg.Database.ConnMaxLifetime == 0 {
		cfg.Database.ConnMaxLifetime = 300
	}
	if cfg.Redis.Port == 0 {
		cfg.Redis.Port = 6379
	}
	if cfg.JWT.AccessExpiryMins == 0 {
		cfg.JWT.AccessExpiryMins = 15
	}
	if cfg.JWT.RefreshExpiryDays == 0 {
		cfg.JWT.RefreshExpiryDays = 1
	}
	if cfg.WhatsApp.APIVersion == "" {
		cfg.WhatsApp.APIVersion = "v24.0"
	}
	if cfg.WhatsApp.BaseURL == "" {
		cfg.WhatsApp.BaseURL = "https://graph.facebook.com"
	}
	if cfg.AI.QwenBaseURL == "" {
		cfg.AI.QwenBaseURL = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
	}
	if cfg.GoogleSearchConsole.AuthURL == "" {
		cfg.GoogleSearchConsole.AuthURL = "https://accounts.google.com/o/oauth2/auth"
	}
	if cfg.GoogleSearchConsole.TokenURL == "" {
		cfg.GoogleSearchConsole.TokenURL = "https://oauth2.googleapis.com/token"
	}
	if cfg.GoogleSearchConsole.APIBaseURL == "" {
		cfg.GoogleSearchConsole.APIBaseURL = "https://www.googleapis.com/webmasters/v3"
	}
	if cfg.Storage.Type == "" {
		cfg.Storage.Type = "local"
	}
	if cfg.Storage.LocalPath == "" {
		cfg.Storage.LocalPath = "./uploads"
	}
	// Default admin credentials (only used during initial setup)
	if cfg.DefaultAdmin.Email == "" {
		cfg.DefaultAdmin.Email = "admin@rereply.app"
	}
	if cfg.DefaultAdmin.Password == "" {
		cfg.DefaultAdmin.Password = "admin"
	}
	if cfg.DefaultAdmin.FullName == "" {
		cfg.DefaultAdmin.FullName = "ReReply Administrator"
	}
	// Cookie defaults
	if cfg.App.Environment == "production" {
		cfg.Cookie.Secure = true
	}
	// Rate limiting defaults
	if cfg.RateLimit.LoginMaxAttempts == 0 {
		cfg.RateLimit.LoginMaxAttempts = 10
	}
	if cfg.RateLimit.RegisterMaxAttempts == 0 {
		cfg.RateLimit.RegisterMaxAttempts = 10
	}
	if cfg.RateLimit.RefreshMaxAttempts == 0 {
		cfg.RateLimit.RefreshMaxAttempts = 30
	}
	if cfg.RateLimit.SSOMaxAttempts == 0 {
		cfg.RateLimit.SSOMaxAttempts = 10
	}
	if cfg.RateLimit.WindowSeconds == 0 {
		cfg.RateLimit.WindowSeconds = 60
	}
	// Calling defaults
	if cfg.Calling.MaxCallDuration == 0 {
		cfg.Calling.MaxCallDuration = 300
	}
	if cfg.Calling.AudioDir == "" {
		cfg.Calling.AudioDir = "./audio"
	}
	if cfg.Calling.HoldMusicFile == "" {
		cfg.Calling.HoldMusicFile = "hold_music.opus"
	}
	if cfg.Calling.TransferTimeoutSecs == 0 {
		cfg.Calling.TransferTimeoutSecs = 120
	}
}
