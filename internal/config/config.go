package config

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by the coturn TURN REST API (RFC draft)
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	pathpkg "path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config holds all configuration for the application
type Config struct {
	App                     AppConfig                     `koanf:"app"`
	Server                  ServerConfig                  `koanf:"server"`
	Database                DatabaseConfig                `koanf:"database"`
	Redis                   RedisConfig                   `koanf:"redis"`
	JWT                     JWTConfig                     `koanf:"jwt"`
	WhatsApp                WhatsAppConfig                `koanf:"whatsapp"`
	AI                      AIConfig                      `koanf:"ai"`
	Storage                 StorageConfig                 `koanf:"storage"`
	DefaultAdmin            DefaultAdminConfig            `koanf:"default_admin"`
	RateLimit               RateLimitConfig               `koanf:"rate_limit"`
	Cookie                  CookieConfig                  `koanf:"cookie"`
	Calling                 CallingConfig                 `koanf:"calling"`
	TTS                     TTSConfig                     `koanf:"tts"`
	GoogleSearchConsole     GoogleSearchConsoleConfig     `koanf:"google_search_console"`
	MetaRelay               MetaRelayConfig               `koanf:"meta_relay"`
	MetaMessengerOnboarding MetaMessengerOnboardingConfig `koanf:"meta_messenger_onboarding"`
}

const (
	defaultMetaMessengerConfigID        = "1720929458946813"
	defaultMetaMessengerOwnerBusinessID = "2018290039073161"
	defaultMetaMessengerGraphAPIVersion = "v25.0"
	defaultMetaMessengerGraphBaseURL    = "https://graph.facebook.com"
)

// MetaMessengerOnboardingConfig controls the managed Facebook Login for
// Business flow used to stage Messenger Page accounts. It is deliberately
// separate from MetaRelayConfig: the provider app owner is not evidence that a
// tenant owns a Page, and relay inventory remains a protected deployment fact.
// AppSecret is server-only and must never be returned by an HTTP handler.
type MetaMessengerOnboardingConfig struct {
	Enabled             bool   `koanf:"enabled"`
	AppID               string `koanf:"app_id"`
	ConfigID            string `koanf:"config_id"`
	OwnerBusinessID     string `koanf:"owner_business_id"`
	AppSecret           string `koanf:"app_secret"`
	GraphAPIVersion     string `koanf:"graph_api_version"`
	GraphBaseURL        string `koanf:"graph_base_url"`
	TrustedRelayBaseURL string `koanf:"trusted_relay_base_url"`
}

// MetaRelayConfig is deployment-controlled trust material for Meta relay
// accounts. Tenant-supplied ChannelAccount URLs are never sufficient to choose
// a Meta readiness issuer: they must match this origin and protected inventory.
type MetaRelayConfig struct {
	BaseURL              string `koanf:"base_url"`
	ExpectedAccountsJSON string `koanf:"expected_accounts_json"`
	// ProviderProofSecret is deployment-held trust material shared only by
	// ReReply and the Meta relay. It must never be copied into tenant account
	// config or returned by the channel-account API.
	ProviderProofSecret string `koanf:"provider_proof_secret"`
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
	setMetaMessengerOnboardingDefaults(&cfg.MetaMessengerOnboarding)
	if err := validateMetaRelayDeploymentConfig(cfg.MetaRelay); err != nil {
		return nil, err
	}
	if err := validateMetaMessengerOnboardingConfig(cfg.MetaMessengerOnboarding, cfg.App.Environment); err != nil {
		return nil, err
	}
	if err := validateMetaMessengerRelayConsistency(
		cfg.MetaMessengerOnboarding,
		cfg.MetaRelay,
		cfg.App.Environment,
	); err != nil {
		return nil, err
	}

	// Set defaults
	setDefaults(&cfg)

	return &cfg, nil
}

func validateMetaRelayDeploymentConfig(meta MetaRelayConfig) error {
	configured := strings.TrimSpace(meta.BaseURL) != "" ||
		strings.TrimSpace(meta.ExpectedAccountsJSON) != "" ||
		meta.ProviderProofSecret != ""
	if !configured {
		return nil
	}
	if strings.TrimSpace(meta.BaseURL) == "" {
		return errors.New("WHATOMATE_META_RELAY__BASE_URL is required when Meta relay is configured")
	}
	if strings.TrimSpace(meta.ExpectedAccountsJSON) == "" {
		return errors.New("WHATOMATE_META_RELAY__EXPECTED_ACCOUNTS_JSON is required when Meta relay is configured")
	}
	if len([]byte(meta.ProviderProofSecret)) < 32 ||
		strings.TrimSpace(meta.ProviderProofSecret) != meta.ProviderProofSecret {
		return errors.New("WHATOMATE_META_RELAY__PROVIDER_PROOF_SECRET must contain at least 32 bytes without surrounding whitespace")
	}
	return nil
}

func setMetaMessengerOnboardingDefaults(meta *MetaMessengerOnboardingConfig) {
	if meta == nil {
		return
	}
	if strings.TrimSpace(meta.ConfigID) == "" {
		meta.ConfigID = defaultMetaMessengerConfigID
	}
	if strings.TrimSpace(meta.OwnerBusinessID) == "" {
		meta.OwnerBusinessID = defaultMetaMessengerOwnerBusinessID
	}
	if strings.TrimSpace(meta.GraphAPIVersion) == "" {
		meta.GraphAPIVersion = defaultMetaMessengerGraphAPIVersion
	}
	if strings.TrimSpace(meta.GraphBaseURL) == "" {
		meta.GraphBaseURL = defaultMetaMessengerGraphBaseURL
	}
}

var metaMessengerGraphVersionPattern = regexp.MustCompile(`^v[1-9][0-9]*\.[0-9]+$`)

func validateMetaMessengerOnboardingConfig(meta MetaMessengerOnboardingConfig, environment string) error {
	if appID := strings.TrimSpace(meta.AppID); appID != "" && !numericMetaID(appID) {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID must be a canonical numeric Meta app ID")
	}
	if !numericMetaID(strings.TrimSpace(meta.ConfigID)) {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__CONFIG_ID must be a canonical numeric Facebook Login for Business configuration ID")
	}
	if !numericMetaID(strings.TrimSpace(meta.OwnerBusinessID)) {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__OWNER_BUSINESS_ID must be a canonical numeric Meta Business Portfolio ID")
	}
	if !metaMessengerGraphVersionPattern.MatchString(strings.TrimSpace(meta.GraphAPIVersion)) {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_API_VERSION must be explicit, for example v25.0")
	}
	if err := validateMetaMessengerBaseURL(meta.GraphBaseURL, environment, true); err != nil {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__GRAPH_BASE_URL must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	if relayBase := strings.TrimSpace(meta.TrustedRelayBaseURL); relayBase != "" {
		if err := validateMetaMessengerBaseURL(relayBase, environment, false); err != nil {
			return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL must be an absolute HTTPS URL without credentials, query, or fragment")
		}
	}
	if meta.AppSecret != "" && strings.TrimSpace(meta.AppSecret) != meta.AppSecret {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET must not contain surrounding whitespace")
	}
	if !meta.Enabled {
		return nil
	}
	if strings.TrimSpace(meta.AppID) == "" {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__APP_ID is required when Messenger onboarding is enabled")
	}
	if meta.AppSecret == "" {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__APP_SECRET is required when Messenger onboarding is enabled")
	}
	if strings.TrimSpace(meta.TrustedRelayBaseURL) == "" {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL is required when Messenger onboarding is enabled")
	}
	if strings.EqualFold(strings.TrimSpace(environment), "production") {
		return errors.New("managed Messenger onboarding is staging-only until relay secret provisioning and recurring Meta ownership revalidation are available")
	}
	return nil
}

func validateMetaMessengerRelayConsistency(
	onboarding MetaMessengerOnboardingConfig,
	relay MetaRelayConfig,
	environment string,
) error {
	if !onboarding.Enabled {
		return nil
	}
	onboardingBase, err := CanonicalMetaRelayBaseURL(onboarding.TrustedRelayBaseURL, environment)
	if err != nil {
		return errors.New("WHATOMATE_META_MESSENGER_ONBOARDING__TRUSTED_RELAY_BASE_URL is invalid")
	}
	protectedBase, err := CanonicalMetaRelayBaseURL(relay.BaseURL, environment)
	if err != nil {
		return errors.New("WHATOMATE_META_RELAY__BASE_URL is required and must be valid when Messenger onboarding is enabled")
	}
	if onboardingBase != protectedBase {
		return errors.New("messenger onboarding trusted relay base must match WHATOMATE_META_RELAY__BASE_URL")
	}
	return nil
}

// CanonicalMetaRelayBaseURL returns the exact deployment-controlled relay base
// used by both managed onboarding and runtime trust. Keeping this normalization
// shared prevents a Page from being staged against a relay origin/path that the
// protected inventory can never recognize.
func CanonicalMetaRelayBaseURL(rawURL, environment string) (string, error) {
	if err := validateMetaMessengerBaseURL(rawURL, environment, false); err != nil {
		return "", err
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.RawPath != "" || strings.Contains(parsed.Path, "//") {
		return "", errors.New("invalid relay base URL")
	}
	cleanPath := pathpkg.Clean("/" + strings.TrimPrefix(parsed.Path, "/"))
	if cleanPath == "/" {
		cleanPath = ""
	}
	if parsed.Path != cleanPath && parsed.Path != cleanPath+"/" {
		return "", errors.New("relay base URL path is not canonical")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = cleanPath
	return parsed.String(), nil
}

func validateMetaMessengerBaseURL(rawURL, environment string, graphEndpoint bool) error {
	trimmedURL := strings.TrimSpace(rawURL)
	parsed, err := url.Parse(trimmedURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid URL")
	}
	if graphEndpoint && strings.EqualFold(strings.TrimSpace(environment), "production") {
		if rawURL != trimmedURL || trimmedURL != defaultMetaMessengerGraphBaseURL ||
			parsed.Path != "" || parsed.RawPath != "" ||
			parsed.Scheme != "https" || parsed.Host != "graph.facebook.com" {
			return errors.New("production Graph origin is not pinned")
		}
		return nil
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(environment), "production") || !strings.EqualFold(parsed.Scheme, "http") {
		return errors.New("HTTPS is required")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	ip := net.ParseIP(host)
	if !graphEndpoint || (ip == nil || !ip.IsLoopback()) {
		return errors.New("only a loopback Graph test endpoint may use HTTP")
	}
	return nil
}

func numericMetaID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func setDefaults(cfg *Config) {
	setMetaMessengerOnboardingDefaults(&cfg.MetaMessengerOnboarding)
	if cfg.App.Name == "" {
		cfg.App.Name = "ReReply"
	}
	if cfg.App.Environment == "" {
		cfg.App.Environment = "development"
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
