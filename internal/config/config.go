package config

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by the coturn TURN REST API (RFC draft)
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
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
	MetaInstagram       MetaInstagramConfig       `koanf:"meta_instagram_onboarding"`
	ThreadsAppReview    ThreadsAppReviewConfig    `koanf:"threads_app_review"`
	ThreadsManaged      ThreadsManagedConfig      `koanf:"threads_managed"`
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

// ThreadsManagedConfig owns the shared Threads application control plane at
// deployment scope. Tenant records select only a PlatformAppKey and never
// receive either secret below. PlatformApps is a list so the persistence and
// runtime contracts are shard-ready even though the first deployment may
// configure just one app.
type ThreadsManagedConfig struct {
	Enabled                  bool                       `koanf:"enabled" json:"enabled"`
	AllowedOrganizationIDs   string                     `koanf:"allowed_organization_ids" json:"allowed_organization_ids"`
	AllowAllOrganizations    bool                       `koanf:"allow_all_organizations" json:"allow_all_organizations"`
	ComplianceOrganizationID string                     `koanf:"compliance_organization_id" json:"compliance_organization_id"`
	PlatformApp              ThreadsPlatformAppConfig   `koanf:"platform_app" json:"-"`
	PlatformApps             []ThreadsPlatformAppConfig `koanf:"platform_apps" json:"platform_apps"`
}

// ThreadsPlatformAppConfig describes one deployment-owned Meta application.
// AppSecret and WebhookVerifyToken are deliberately excluded from JSON so an
// accidental config response cannot disclose them to a tenant or UI.
type ThreadsPlatformAppConfig struct {
	PlatformAppKey          string `koanf:"platform_app_key" json:"platform_app_key"`
	AppID                   string `koanf:"app_id" json:"app_id"`
	AppSecret               string `koanf:"app_secret" json:"-"`
	WebhookVerifyToken      string `koanf:"webhook_verify_token" json:"-"`
	AppReviewStatus         string `koanf:"app_review_status" json:"app_review_status"`
	AppReviewEvidenceSHA256 string `koanf:"app_review_evidence_sha256" json:"-"`
	AppReviewApprovedAt     string `koanf:"app_review_approved_at" json:"app_review_approved_at,omitempty"`
	ConfigurationGeneration uint64 `koanf:"configuration_generation" json:"configuration_generation"`
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

// MetaInstagramConfig is the deployment-owned Instagram Login application
// used only by the managed Instagram lifecycle. It is intentionally separate
// from the relay's legacy/static Instagram application. Active and
// quarantined organization sets remain deployment-owned and bounded.
type MetaInstagramConfig struct {
	Enabled                  bool   `koanf:"enabled"`
	QuarantineOnly           bool   `koanf:"quarantine_only"`
	AppID                    string `koanf:"app_id"`
	AppSecret                string `koanf:"app_secret"`
	AppReviewStatus          string `koanf:"app_review_status"`
	GraphAPIVersion          string `koanf:"graph_api_version"`
	AuthorizationBaseURL     string `koanf:"authorization_base_url"`
	TokenBaseURL             string `koanf:"token_base_url"`
	GraphBaseURL             string `koanf:"graph_base_url"`
	ReReplyBaseURL           string `koanf:"rereply_base_url"`
	RelayBaseURL             string `koanf:"relay_base_url"`
	HealthApprovalMaxAgeMins int    `koanf:"health_approval_max_age_mins"`
	RevalidationLeadMins     int    `koanf:"revalidation_lead_mins"`
	TokenRefreshLeadHours    int    `koanf:"token_refresh_lead_hours"`
	SchedulerIntervalSeconds int    `koanf:"scheduler_interval_seconds"`
	// AllowedOrganizationID is the legacy singleton gate. New deployments use
	// AllowedOrganizationIDs; both may overlap only as the same singleton while
	// a reader-first rollout is in progress.
	AllowedOrganizationID                string `koanf:"allowed_organization_id"`
	AllowedOrganizationIDs               string `koanf:"allowed_organization_ids"`
	QuarantinedOrganizationIDs           string `koanf:"quarantined_organization_ids"`
	DataDeletionComplianceOrganizationID string `koanf:"data_deletion_compliance_organization_id"`
	DevelopmentTestProfileID             string `koanf:"development_test_profile_id"`
	DevelopmentTestOAuthSubjectID        string `koanf:"development_test_oauth_subject_id"`
	DevelopmentAppRole                   string `koanf:"development_app_role"`
}

// MaxMetaInstagramManagedOrganizations bounds every tenant-scoped registry,
// callback, lifecycle, and privacy lookup performed for the managed Instagram
// app. The bound is deliberately much smaller than an untrusted database scan.
const MaxMetaInstagramManagedOrganizations = 32

type metaInstagramOrganizationSets struct {
	active       []string
	quarantined  []string
	compliance   string
	usesSetModel bool
}

// ActiveOrganizationIDs returns the canonical sorted organizations that may
// start OAuth and acquire runtime authority. Invalid unvalidated configuration
// returns an empty set so call sites fail closed.
func (config MetaInstagramConfig) ActiveOrganizationIDs() []string {
	sets, err := canonicalMetaInstagramOrganizationSets(config)
	if err != nil {
		return nil
	}
	return append([]string(nil), sets.active...)
}

// ManagedOrganizationIDs includes active and explicitly quarantined tenants.
// Quarantined tenants remain addressable only for callbacks, lifecycle
// cancellation, disconnect, and retained privacy status obligations.
func (config MetaInstagramConfig) ManagedOrganizationIDs() []string {
	sets, err := canonicalMetaInstagramOrganizationSets(config)
	if err != nil {
		return nil
	}
	values := append(append([]string(nil), sets.active...), sets.quarantined...)
	sort.Strings(values)
	return values
}

func (config MetaInstagramConfig) OrganizationManaged(organizationID string) bool {
	organizationID = strings.TrimSpace(organizationID)
	for _, configured := range config.ManagedOrganizationIDs() {
		if configured == organizationID {
			return true
		}
	}
	return false
}

func (config MetaInstagramConfig) OrganizationReleased(organizationID string) bool {
	if config.QuarantineOnly {
		return false
	}
	organizationID = strings.TrimSpace(organizationID)
	for _, configured := range config.ActiveOrganizationIDs() {
		if configured == organizationID {
			return true
		}
	}
	return false
}

func (config MetaInstagramConfig) OrganizationQuarantined(organizationID string) bool {
	organizationID = strings.TrimSpace(organizationID)
	if config.QuarantineOnly {
		return config.OrganizationManaged(organizationID)
	}
	sets, err := canonicalMetaInstagramOrganizationSets(config)
	if err != nil {
		return false
	}
	for _, configured := range sets.quarantined {
		if configured == organizationID {
			return true
		}
	}
	return false
}

func (config MetaInstagramConfig) DataDeletionComplianceOrganization() string {
	sets, err := canonicalMetaInstagramOrganizationSets(config)
	if err != nil {
		return ""
	}
	return sets.compliance
}

func (config MetaInstagramConfig) UsesOrganizationSetModel() bool {
	sets, err := canonicalMetaInstagramOrganizationSets(config)
	return err == nil && sets.usesSetModel
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
	canonicalThreadsManaged, err := CanonicalizeThreadsManagedConfig(cfg.ThreadsManaged)
	if err != nil {
		return nil, err
	}
	cfg.ThreadsManaged = canonicalThreadsManaged
	if err := validateMetaRegistryConfig(
		cfg.MetaRegistry,
		cfg.MetaMessenger,
		cfg.MetaInstagram,
		cfg.App,
	); err != nil {
		return nil, err
	}
	if err := validateMetaMessengerConfig(cfg.MetaMessenger, cfg.MetaRegistry, cfg.App.Environment, cfg.Server); err != nil {
		return nil, err
	}
	if err := validateMetaInstagramConfig(
		cfg.MetaInstagram,
		cfg.MetaMessenger,
		cfg.MetaRegistry,
		cfg.App.Environment,
		cfg.Server,
	); err != nil {
		return nil, err
	}
	if err := ValidateThreadsAppReviewConfig(cfg.ThreadsAppReview, cfg.App.Environment); err != nil {
		return nil, err
	}
	if err := ValidateThreadsManagedConfig(cfg.ThreadsManaged, cfg.ThreadsAppReview, cfg.App.Environment); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func ValidateThreadsAppReviewConfig(config ThreadsAppReviewConfig, environment string) error {
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
	if organizationID != config.DevelopmentOrganizationID || !canonicalNonNilUUID(organizationID) {
		return errors.New("threads development organization_id must be a canonical UUID")
	}
	if appID != config.DevelopmentAppID || profileID != config.DevelopmentProfileID ||
		!canonicalNumericMetaID(appID) || !canonicalNumericMetaID(profileID) {
		return errors.New("threads development app_id and profile_id must be canonical numeric Meta IDs")
	}
	return nil
}

var (
	threadsPlatformAppKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	lowerHexSHA256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidateThreadsManagedConfig is exported so every server/worker entry point
// and the runtime policy helper can fail closed before using managed-app state.
func ValidateThreadsManagedConfig(
	managed ThreadsManagedConfig,
	development ThreadsAppReviewConfig,
	environment string,
) error {
	if err := ValidateThreadsAppReviewConfig(development, environment); err != nil {
		return err
	}
	canonicalManaged, err := CanonicalizeThreadsManagedConfig(managed)
	if err != nil {
		return err
	}
	managed = canonicalManaged
	configured := managed.AllowAllOrganizations ||
		strings.TrimSpace(managed.AllowedOrganizationIDs) != "" ||
		strings.TrimSpace(managed.ComplianceOrganizationID) != "" ||
		len(managed.PlatformApps) > 0
	if !managed.Enabled {
		if configured {
			return errors.New("managed Threads must be explicitly enabled when platform apps, an organization release policy, or a compliance organization are configured")
		}
		return nil
	}

	normalizedEnvironment := strings.ToLower(strings.TrimSpace(environment))
	switch normalizedEnvironment {
	case "development", "staging", "production":
	default:
		return errors.New("managed Threads environment must be development, staging, or production")
	}

	allowedOrganizations, err := canonicalThreadsManagedOrganizationAllowlist(managed.AllowedOrganizationIDs)
	if err != nil {
		return err
	}
	if managed.AllowAllOrganizations == (len(allowedOrganizations) > 0) {
		return errors.New("managed Threads requires exactly one of an organization allowlist or allow_all_organizations")
	}
	if managed.AllowAllOrganizations && normalizedEnvironment != "production" {
		return errors.New("managed Threads allow_all_organizations is reserved for an explicit production release")
	}
	complianceOrganizationID := strings.TrimSpace(managed.ComplianceOrganizationID)
	if !canonicalNonNilUUID(complianceOrganizationID) {
		return errors.New("managed Threads compliance_organization_id must be a canonical UUID")
	}
	for _, organizationID := range allowedOrganizations {
		if organizationID == complianceOrganizationID {
			return errors.New("managed Threads compliance organization must be separate from every onboarding organization")
		}
	}
	if len(managed.PlatformApps) == 0 {
		return errors.New("managed Threads requires at least one deployment-owned platform app")
	}

	seenKeys := make(map[string]struct{}, len(managed.PlatformApps))
	seenAppIDs := make(map[string]struct{}, len(managed.PlatformApps))
	for index, platformApp := range managed.PlatformApps {
		key := strings.TrimSpace(platformApp.PlatformAppKey)
		if key != platformApp.PlatformAppKey || !threadsPlatformAppKeyPattern.MatchString(key) {
			return fmt.Errorf("managed Threads platform_apps[%d].platform_app_key must be a canonical lowercase app key", index)
		}
		if _, duplicate := seenKeys[key]; duplicate {
			return errors.New("managed Threads platform_app_key values must be unique")
		}
		seenKeys[key] = struct{}{}

		if !canonicalNumericMetaID(platformApp.AppID) {
			return fmt.Errorf("managed Threads platform app %q app_id must be a canonical numeric Meta ID", key)
		}
		if _, duplicate := seenAppIDs[platformApp.AppID]; duplicate {
			return errors.New("managed Threads app_id values must be unique across platform app keys")
		}
		seenAppIDs[platformApp.AppID] = struct{}{}
		if len(platformApp.AppSecret) < 32 || strings.TrimSpace(platformApp.AppSecret) != platformApp.AppSecret {
			return fmt.Errorf("managed Threads platform app %q app_secret must contain at least 32 bytes without surrounding whitespace", key)
		}
		if len(platformApp.WebhookVerifyToken) < 32 ||
			strings.TrimSpace(platformApp.WebhookVerifyToken) != platformApp.WebhookVerifyToken {
			return fmt.Errorf("managed Threads platform app %q webhook_verify_token must contain at least 32 bytes without surrounding whitespace", key)
		}
		if subtleConstantTimeConfigEqual(platformApp.AppSecret, platformApp.WebhookVerifyToken) {
			return fmt.Errorf("managed Threads platform app %q must use distinct app-secret and webhook-verification credentials", key)
		}
		if platformApp.ConfigurationGeneration == 0 {
			return fmt.Errorf("managed Threads platform app %q configuration_generation must be at least 1", key)
		}

		reviewStatus := strings.ToLower(strings.TrimSpace(platformApp.AppReviewStatus))
		if reviewStatus != platformApp.AppReviewStatus {
			return fmt.Errorf("managed Threads platform app %q app_review_status must be canonical lowercase", key)
		}
		switch reviewStatus {
		case "not_submitted", "pending", "approved", "rejected":
		default:
			return fmt.Errorf("managed Threads platform app %q app_review_status must be not_submitted, pending, approved, or rejected", key)
		}
		approved := reviewStatus == "approved"
		if approved {
			if !lowerHexSHA256Pattern.MatchString(platformApp.AppReviewEvidenceSHA256) {
				return fmt.Errorf("managed Threads platform app %q approved review requires a lowercase SHA-256 evidence digest", key)
			}
			approvedAt, parseErr := time.Parse(time.RFC3339Nano, platformApp.AppReviewApprovedAt)
			if parseErr != nil || approvedAt.IsZero() {
				return fmt.Errorf("managed Threads platform app %q approved review requires an RFC3339 approval timestamp", key)
			}
		} else if platformApp.AppReviewEvidenceSHA256 != "" || platformApp.AppReviewApprovedAt != "" {
			return fmt.Errorf("managed Threads platform app %q cannot retain approval evidence before review is approved", key)
		}

		if normalizedEnvironment == "production" && !approved {
			return fmt.Errorf("managed Threads platform app %q requires approved app review in production", key)
		}
		if normalizedEnvironment != "production" && !approved {
			if !development.DevelopmentTestingEnabled ||
				development.DevelopmentAppID != platformApp.AppID ||
				managed.AllowAllOrganizations || len(allowedOrganizations) != 1 ||
				allowedOrganizations[0] != development.DevelopmentOrganizationID {
				return fmt.Errorf("managed Threads platform app %q without approved review requires the exact nonproduction app-role workspace and app gate", key)
			}
		}
	}

	return nil
}

// CanonicalizeThreadsManagedConfig converts the env-friendly single-app shape
// into the app-keyed slice consumed by runtime and persistence code. Operators
// must choose one shape so credentials cannot be shadowed by precedence rules.
func CanonicalizeThreadsManagedConfig(managed ThreadsManagedConfig) (ThreadsManagedConfig, error) {
	singleConfigured := threadsPlatformAppConfigured(managed.PlatformApp)
	if singleConfigured && len(managed.PlatformApps) > 0 {
		return ThreadsManagedConfig{}, errors.New("managed Threads must configure platform_app or platform_apps, not both")
	}
	if singleConfigured {
		managed.PlatformApps = []ThreadsPlatformAppConfig{managed.PlatformApp}
		managed.PlatformApp = ThreadsPlatformAppConfig{}
	}
	return managed, nil
}

func threadsPlatformAppConfigured(platformApp ThreadsPlatformAppConfig) bool {
	return platformApp.PlatformAppKey != "" || platformApp.AppID != "" ||
		platformApp.AppSecret != "" || platformApp.WebhookVerifyToken != "" ||
		platformApp.AppReviewStatus != "" || platformApp.AppReviewEvidenceSHA256 != "" ||
		platformApp.AppReviewApprovedAt != "" || platformApp.ConfigurationGeneration != 0
}

func canonicalThreadsManagedOrganizationAllowlist(raw string) ([]string, error) {
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !canonicalNonNilUUID(item) {
			return nil, errors.New("managed Threads allowed_organization_ids must contain canonical UUIDs")
		}
		if _, exists := seen[item]; exists {
			return nil, errors.New("managed Threads allowed_organization_ids contains a duplicate UUID")
		}
		seen[item] = struct{}{}
		values = append(values, item)
	}
	return values, nil
}

func validateMetaRegistryConfig(
	config MetaRegistryConfig,
	messenger MetaMessengerConfig,
	instagram MetaInstagramConfig,
	app AppConfig,
) error {
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
		if !messenger.Enabled && !instagram.Enabled {
			return errors.New("meta registry cannot be enabled before a managed Meta lifecycle is enabled")
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

func canonicalMetaInstagramOrganizationList(raw, field string) ([]string, error) {
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !canonicalNonNilUUID(item) {
			return nil, fmt.Errorf("instagram onboarding %s must contain canonical UUIDs", field)
		}
		if _, exists := seen[item]; exists {
			return nil, fmt.Errorf("instagram onboarding %s contains a duplicate UUID", field)
		}
		seen[item] = struct{}{}
		values = append(values, item)
	}
	sort.Strings(values)
	return values, nil
}

func canonicalMetaInstagramOrganizationSets(
	config MetaInstagramConfig,
) (metaInstagramOrganizationSets, error) {
	legacy := strings.TrimSpace(config.AllowedOrganizationID)
	pluralRaw := strings.TrimSpace(config.AllowedOrganizationIDs)
	quarantineRaw := strings.TrimSpace(config.QuarantinedOrganizationIDs)
	compliance := strings.TrimSpace(config.DataDeletionComplianceOrganizationID)

	active, err := canonicalMetaInstagramOrganizationList(
		pluralRaw, "allowed_organization_ids",
	)
	if err != nil {
		return metaInstagramOrganizationSets{}, err
	}
	if pluralRaw == "" {
		active = nil
		if legacy != "" {
			if !canonicalNonNilUUID(legacy) {
				return metaInstagramOrganizationSets{}, errors.New(
					"instagram onboarding allowed_organization_id must contain one canonical UUID",
				)
			}
			active = []string{legacy}
		}
	} else if legacy != "" && (len(active) != 1 || active[0] != legacy) {
		return metaInstagramOrganizationSets{}, errors.New(
			"instagram onboarding legacy allowed_organization_id may overlap allowed_organization_ids only as the same singleton",
		)
	}

	quarantined, err := canonicalMetaInstagramOrganizationList(
		quarantineRaw, "quarantined_organization_ids",
	)
	if err != nil {
		return metaInstagramOrganizationSets{}, err
	}
	activeSet := make(map[string]struct{}, len(active))
	for _, organizationID := range active {
		activeSet[organizationID] = struct{}{}
	}
	for _, organizationID := range quarantined {
		if _, overlaps := activeSet[organizationID]; overlaps {
			return metaInstagramOrganizationSets{}, errors.New(
				"instagram onboarding active and quarantined organization IDs must be disjoint",
			)
		}
	}
	managedCount := len(active) + len(quarantined)
	if managedCount == 0 {
		return metaInstagramOrganizationSets{}, errors.New(
			"instagram onboarding requires at least one active or quarantined organization UUID",
		)
	}
	if managedCount > MaxMetaInstagramManagedOrganizations {
		return metaInstagramOrganizationSets{}, fmt.Errorf(
			"instagram onboarding supports at most %d managed organizations",
			MaxMetaInstagramManagedOrganizations,
		)
	}

	usesSetModel := pluralRaw != "" || quarantineRaw != ""
	if usesSetModel {
		if !canonicalNonNilUUID(compliance) {
			return metaInstagramOrganizationSets{}, errors.New(
				"instagram onboarding data_deletion_compliance_organization_id must contain one canonical UUID when organization sets are configured",
			)
		}
		if _, collides := activeSet[compliance]; collides {
			return metaInstagramOrganizationSets{}, errors.New(
				"instagram onboarding data-deletion compliance organization must be distinct from every managed clinic organization",
			)
		}
		for _, organizationID := range quarantined {
			if organizationID == compliance {
				return metaInstagramOrganizationSets{}, errors.New(
					"instagram onboarding data-deletion compliance organization must be distinct from every managed clinic organization",
				)
			}
		}
	} else if compliance != "" {
		return metaInstagramOrganizationSets{}, errors.New(
			"instagram onboarding data-deletion compliance organization requires the organization-set model",
		)
	}

	return metaInstagramOrganizationSets{
		active: active, quarantined: quarantined, compliance: compliance,
		usesSetModel: usesSetModel,
	}, nil
}

func validateMetaInstagramConfig(
	config MetaInstagramConfig,
	messenger MetaMessengerConfig,
	registry MetaRegistryConfig,
	environment string,
	server ServerConfig,
) error {
	configured := config.AppID != "" || config.AppSecret != "" ||
		config.QuarantineOnly ||
		strings.TrimSpace(config.AppReviewStatus) != "" ||
		config.ReReplyBaseURL != "" || config.RelayBaseURL != "" ||
		strings.TrimSpace(config.AllowedOrganizationID) != "" ||
		strings.TrimSpace(config.AllowedOrganizationIDs) != "" ||
		strings.TrimSpace(config.QuarantinedOrganizationIDs) != "" ||
		strings.TrimSpace(config.DataDeletionComplianceOrganizationID) != "" ||
		strings.TrimSpace(config.DevelopmentTestProfileID) != "" ||
		strings.TrimSpace(config.DevelopmentTestOAuthSubjectID) != "" ||
		strings.TrimSpace(config.DevelopmentAppRole) != ""
	if !config.Enabled {
		if configured {
			return errors.New("instagram onboarding must be explicitly enabled when platform credentials, review state, endpoints, or an organization allowlist are configured")
		}
		return nil
	}
	if !registry.Enabled {
		return errors.New("instagram onboarding requires the encrypted Meta registry lifecycle")
	}
	normalizedEnvironment := strings.ToLower(strings.TrimSpace(environment))
	switch normalizedEnvironment {
	case "development", "staging", "production":
	default:
		return errors.New("instagram onboarding environment must be development, staging, or production")
	}
	if server.WriteTimeout < 120 {
		return errors.New("instagram onboarding requires server.write_timeout of at least 120 seconds")
	}
	organizationSets, err := canonicalMetaInstagramOrganizationSets(config)
	if err != nil {
		return err
	}
	if !canonicalNumericMetaID(config.AppID) {
		return errors.New("instagram onboarding app_id must be a canonical numeric Meta ID")
	}
	if len(config.AppSecret) < 32 || strings.TrimSpace(config.AppSecret) != config.AppSecret {
		return errors.New("instagram onboarding app_secret must contain at least 32 bytes without surrounding whitespace")
	}
	if messenger.Enabled && (strings.TrimSpace(messenger.AppID) == strings.TrimSpace(config.AppID) ||
		subtleConstantTimeConfigEqual(messenger.AppSecret, config.AppSecret)) {
		return errors.New("managed Messenger and Instagram applications must use distinct IDs and secrets")
	}
	reviewStatus := strings.ToLower(strings.TrimSpace(config.AppReviewStatus))
	switch reviewStatus {
	case "not_submitted", "pending", "approved", "rejected":
	default:
		return errors.New("instagram onboarding app_review_status must be not_submitted, pending, approved, or rejected")
	}
	production := normalizedEnvironment == "production"
	developmentProfileID := strings.TrimSpace(config.DevelopmentTestProfileID)
	developmentOAuthSubjectID := strings.TrimSpace(config.DevelopmentTestOAuthSubjectID)
	developmentRole := strings.ToLower(strings.TrimSpace(config.DevelopmentAppRole))
	if production && (developmentProfileID != "" || developmentOAuthSubjectID != "" || developmentRole != "") {
		return errors.New("instagram onboarding development profile, OAuth subject, and app-role overrides are forbidden in production")
	}
	if config.QuarantineOnly {
		if developmentProfileID != "" || developmentOAuthSubjectID != "" || developmentRole != "" {
			return errors.New("instagram quarantine-only mode cannot retain development profile, OAuth subject, or app-role overrides")
		}
	} else if production {
		if reviewStatus != "approved" {
			return errors.New("instagram onboarding requires approved app review in production unless quarantine_only is enabled")
		}
	} else if reviewStatus != "approved" {
		if len(organizationSets.active) != 1 || len(organizationSets.quarantined) != 0 {
			return errors.New("nonproduction Instagram app-role testing requires exactly one active organization")
		}
		if !canonicalNumericMetaID(developmentProfileID) {
			return errors.New("nonproduction Instagram testing requires one canonical development_test_profile_id")
		}
		if !canonicalNumericMetaID(developmentOAuthSubjectID) {
			return errors.New("nonproduction Instagram testing requires one canonical development_test_oauth_subject_id")
		}
		switch developmentRole {
		case "administrator", "developer", "tester":
		default:
			return errors.New("nonproduction Instagram testing requires an exact administrator, developer, or tester app role")
		}
	} else if developmentProfileID != "" || developmentOAuthSubjectID != "" || developmentRole != "" {
		return errors.New("approved Instagram releases cannot retain development profile, OAuth subject, or app-role overrides")
	}
	if !metaGraphVersionPattern.MatchString(strings.TrimSpace(config.GraphAPIVersion)) {
		return errors.New("instagram onboarding graph_api_version must be explicit, for example v25.0")
	}
	if err := validateMetaLifecyclePinnedOrigin(
		config.AuthorizationBaseURL,
		production,
		"www.instagram.com",
	); err != nil {
		return errors.New("instagram onboarding authorization_base_url must be the approved Instagram HTTPS origin")
	}
	if err := validateMetaLifecyclePinnedOrigin(config.TokenBaseURL, production, "api.instagram.com"); err != nil {
		return errors.New("instagram onboarding token_base_url must be the approved Instagram HTTPS origin")
	}
	if err := validateMetaLifecyclePinnedOrigin(config.GraphBaseURL, production, "graph.instagram.com"); err != nil {
		return errors.New("instagram onboarding graph_base_url must be the approved Instagram Graph HTTPS origin")
	}
	if err := validateMetaLifecycleBaseURL(config.ReReplyBaseURL, production, false); err != nil {
		return errors.New("instagram onboarding rereply_base_url must be an absolute HTTPS base URL")
	}
	if production && config.ReReplyBaseURL != "https://app.rereply.app" {
		return errors.New("instagram onboarding production rereply_base_url must be exactly https://app.rereply.app")
	}
	if err := validateMetaLifecycleBaseURL(config.RelayBaseURL, production, false); err != nil {
		return errors.New("instagram onboarding relay_base_url must be an absolute HTTPS base URL")
	}
	if production && config.RelayBaseURL != "https://app.rereply.app/meta-relay" {
		return errors.New("instagram onboarding production relay_base_url must be exactly https://app.rereply.app/meta-relay")
	}
	if config.HealthApprovalMaxAgeMins < 1 || config.HealthApprovalMaxAgeMins > 60 ||
		config.RevalidationLeadMins < 5 || config.RevalidationLeadMins >= registry.OwnershipMaxAgeMins ||
		config.TokenRefreshLeadHours < 24 || config.TokenRefreshLeadHours > 30*24 ||
		config.SchedulerIntervalSeconds < 10 || config.SchedulerIntervalSeconds > 300 {
		return errors.New("instagram onboarding lifecycle timing configuration is outside safe bounds")
	}
	return nil
}

func subtleConstantTimeConfigEqual(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return hmac.Equal([]byte(left), []byte(right))
}

func validateMetaLifecyclePinnedOrigin(raw string, production bool, productionHost string) error {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.ForceQuery || parsed.Opaque != "" {
		return errors.New("URL must be an origin")
	}
	if parsed.Scheme != "https" {
		return errors.New("URL must use HTTPS")
	}
	if production && (parsed.Host != productionHost || parsed.Hostname() != productionHost) {
		return errors.New("production origin is not pinned")
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

func canonicalNonNilUUID(value string) bool {
	return canonicalUUID(value) && value != "00000000-0000-0000-0000-000000000000"
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
	if cfg.MetaInstagram.GraphAPIVersion == "" {
		cfg.MetaInstagram.GraphAPIVersion = "v25.0"
	}
	if cfg.MetaInstagram.AuthorizationBaseURL == "" {
		cfg.MetaInstagram.AuthorizationBaseURL = "https://www.instagram.com"
	}
	if cfg.MetaInstagram.TokenBaseURL == "" {
		cfg.MetaInstagram.TokenBaseURL = "https://api.instagram.com"
	}
	if cfg.MetaInstagram.GraphBaseURL == "" {
		cfg.MetaInstagram.GraphBaseURL = "https://graph.instagram.com"
	}
	if cfg.MetaInstagram.HealthApprovalMaxAgeMins == 0 {
		cfg.MetaInstagram.HealthApprovalMaxAgeMins = 15
	}
	if cfg.MetaInstagram.RevalidationLeadMins == 0 {
		cfg.MetaInstagram.RevalidationLeadMins = 60
	}
	if cfg.MetaInstagram.TokenRefreshLeadHours == 0 {
		cfg.MetaInstagram.TokenRefreshLeadHours = 7 * 24
	}
	if cfg.MetaInstagram.SchedulerIntervalSeconds == 0 {
		cfg.MetaInstagram.SchedulerIntervalSeconds = 60
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
