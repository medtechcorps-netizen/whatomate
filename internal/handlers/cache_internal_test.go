package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// cacheTestApp builds a minimal App configured for cache tests, with both DB
// and Redis available. Skips the test if Redis isn't reachable.
func cacheTestApp(t *testing.T) *App {
	t.Helper()
	db := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	return &App{
		DB:    db,
		Log:   testutil.NopLogger(),
		Redis: rdb,
		Config: &config.Config{
			JWT: config.JWTConfig{Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 7},
		},
	}
}

func TestHasPermission_DoesNotFallBackToHomeRoleWithoutMembership(t *testing.T) {
	app := cacheTestApp(t)
	homeOrg := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, homeOrg.ID, "home-admin", []string{"contacts:read"})
	user := testutil.CreateTestUser(t, app.DB, homeOrg.ID, testutil.WithRoleID(&role.ID))

	assert.False(t, app.HasPermission(user.ID, "contacts", "read", otherOrg.ID),
		"a user's home role must not grant permissions in an organization they do not belong to")
}

// makeAccount inserts a WhatsApp account with the given phone ID and tokens.
func makeAccount(t *testing.T, app *App, orgID uuid.UUID, phoneID, accessToken, appSecret string) *models.WhatsAppAccount {
	t.Helper()
	acc := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     orgID,
		Name:               "cache-acc-" + uuid.New().String()[:8],
		PhoneID:            phoneID,
		BusinessID:         "biz-" + uuid.New().String()[:8],
		AccessToken:        accessToken,
		AppSecret:          appSecret,
		WebhookVerifyToken: "vt-" + uuid.New().String()[:8],
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)
	return acc
}

// --- getWhatsAppAccountCached ---

func TestGetWhatsAppAccountCached_CacheMissPopulatesCache(t *testing.T) {
	app := cacheTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	phoneID := "phone-" + uuid.New().String()[:8]
	acc := makeAccount(t, app, org.ID, phoneID, "tok", "secret")

	// Ensure cache is clean.
	cacheKey := fmt.Sprintf("%s%s", whatsappAccountCachePrefix, phoneID)
	app.Redis.Del(context.Background(), cacheKey)

	got, err := app.getWhatsAppAccountCached(phoneID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, acc.ID, got.ID)
	assert.Equal(t, "tok", got.AccessToken, "DB → cache path must return decrypted access token")
	assert.Equal(t, "secret", got.AppSecret)

	// Cache should now be populated.
	cached, err := app.Redis.Get(context.Background(), cacheKey).Result()
	require.NoError(t, err)
	require.NotEmpty(t, cached)
}

func TestGetWhatsAppAccountCached_CacheHitRequiresLiveIdentityButRetainsCachedValues(t *testing.T) {
	app := cacheTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	phoneID := "phone-" + uuid.New().String()[:8]
	acc := makeAccount(t, app, org.ID, phoneID, "real-tok", "real-secret")

	// A matching live identity still uses cached values. The probe must not
	// silently replace the existing same-row cache freshness semantics.
	cacheData := whatsAppAccountCache{
		WhatsAppAccount: models.WhatsAppAccount{
			BaseModel:      models.BaseModel{ID: acc.ID},
			OrganizationID: org.ID,
			Name:           "from-cache",
			PhoneID:        phoneID,
		},
		AccessToken: "cached-tok",
		AppSecret:   "cached-secret",
	}
	data, err := json.Marshal(cacheData)
	require.NoError(t, err)
	cacheKey := fmt.Sprintf("%s%s", whatsappAccountCachePrefix, phoneID)
	require.NoError(t, app.Redis.Set(context.Background(), cacheKey, data, time.Hour).Err())

	private, _ := h1OpenPool(t, 1)
	var probes atomic.Int32
	require.NoError(t, private.Callback().Query().Before("gorm:query").Register("test:cache_hit_identity", func(tx *gorm.DB) {
		if accountCacheIdentityQuery(tx) {
			probes.Add(1)
		}
	}))
	app.DB = private
	got, err := app.getWhatsAppAccountCached(phoneID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.EqualValues(t, 1, probes.Load(), "a valid cache hit still requires live identity")
	assert.Equal(t, "from-cache", got.Name, "a matching live row retains cached values")
	assert.Equal(t, "cached-tok", got.AccessToken,
		"cached AccessToken must be restored on the WhatsAppAccount even though it has json:\"-\"")
	assert.Equal(t, "cached-secret", got.AppSecret,
		"cached AppSecret must be restored")
}

func TestGetWhatsAppAccountCached_ScopedTenantRejectsPriorOwnerCache(t *testing.T) {
	app := cacheTestApp(t)
	priorOrg := testutil.CreateTestOrganization(t, app.DB)
	currentOrg := testutil.CreateTestOrganization(t, app.DB)
	phoneID := "phone-transfer-" + uuid.New().String()[:8]
	prior := makeAccount(t, app, priorOrg.ID, "  "+phoneID+"  ", "prior-owner-token", "prior-secret")
	require.NoError(t, app.DB.Delete(prior).Error)
	current := makeAccount(t, app, currentOrg.ID, phoneID, "current-owner-token", "current-secret")

	stale := whatsAppAccountCache{
		WhatsAppAccount: models.WhatsAppAccount{
			BaseModel:      models.BaseModel{ID: prior.ID},
			OrganizationID: priorOrg.ID,
			Name:           prior.Name,
			PhoneID:        "  " + phoneID + "  ",
		},
		AccessToken: "prior-owner-token",
		AppSecret:   "prior-secret",
	}
	encoded, err := json.Marshal(stale)
	require.NoError(t, err)
	cacheKey := fmt.Sprintf("%s%s", whatsappAccountCachePrefix, phoneID)
	require.NoError(t, app.Redis.Set(context.Background(), cacheKey, encoded, time.Hour).Err())

	scoped := app.scopedApp(app.DB, currentOrg.ID)
	got, err := scoped.getWhatsAppAccountCached("  " + phoneID + "  ")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, current.ID, got.ID)
	assert.Equal(t, currentOrg.ID, got.OrganizationID)
	assert.Equal(t, "current-owner-token", got.AccessToken)
	assert.NotEqual(t, "prior-owner-token", got.AccessToken)
}

func TestGetWhatsAppAccountCached_NotFoundReturnsError(t *testing.T) {
	app := cacheTestApp(t)
	_, err := app.getWhatsAppAccountCached("phone-does-not-exist-" + uuid.New().String()[:8])
	require.Error(t, err)
}

func TestGetWhatsAppAccountCachedForOutboundRejectsInactiveWithoutBlockingRecoveryLookup(t *testing.T) {
	app := cacheTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)

	for _, status := range []string{
		"pending_registration",
		"pending_subscription",
		"subscription_failed",
		"disconnected",
	} {
		t.Run(status, func(t *testing.T) {
			phoneID := "phone-outbound-" + uuid.New().String()[:8]
			account := makeAccount(t, app, org.ID, phoneID, "recovery-token", "recovery-secret")
			require.NoError(t, app.DB.Model(account).Update("status", status).Error)
			app.InvalidateWhatsAppAccountCache(phoneID)

			// Inbound delivery and pending registration/subscription recovery still
			// need credentials while the account is deliberately non-sendable.
			generic, err := app.getWhatsAppAccountCached(phoneID)
			require.NoError(t, err)
			assert.Equal(t, status, generic.Status)

			outbound, err := app.getWhatsAppAccountCachedForOutbound(phoneID)
			require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
			assert.Nil(t, outbound)
		})
	}
}

func TestResolveWhatsAppOrganization_NonRLSNormalizesPhoneID(t *testing.T) {
	app := cacheTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	phoneID := "phone-resolver-" + uuid.New().String()[:8]
	makeAccount(t, app, org.ID, "  "+phoneID+"  ", "resolver-token", "resolver-secret")

	resolved, err := app.resolveWhatsAppOrganization("  " + phoneID + "  ")
	require.NoError(t, err)
	assert.Equal(t, org.ID, resolved)
}

func TestWhatsAppAccountCacheNeverSerializesLegacyWebhookVerifyToken(t *testing.T) {
	t.Parallel()

	cacheData := whatsAppAccountCache{
		WhatsAppAccount: models.WhatsAppAccount{
			WebhookVerifyToken: "synthetic-legacy-verify-token",
		},
		AccessToken: "encrypted-access-token",
	}
	encoded, err := json.Marshal(cacheData)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "webhook_verify_token")
	assert.NotContains(t, string(encoded), "synthetic-legacy-verify-token")
}

func TestInvalidateWhatsAppAccountCache_DeletesKey(t *testing.T) {
	app := cacheTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	phoneID := "phone-" + uuid.New().String()[:8]
	makeAccount(t, app, org.ID, phoneID, "tok", "secret")

	// Populate the cache.
	_, err := app.getWhatsAppAccountCached(phoneID)
	require.NoError(t, err)

	cacheKey := fmt.Sprintf("%s%s", whatsappAccountCachePrefix, phoneID)
	exists, err := app.Redis.Exists(context.Background(), cacheKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)

	app.InvalidateWhatsAppAccountCache(phoneID)

	exists, err = app.Redis.Exists(context.Background(), cacheKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "invalidation must remove the cache key")
}

// --- decryptAccountSecrets handles encrypted + legacy plaintext ---

func TestDecryptAccountSecrets_DecryptsEncryptedValues(t *testing.T) {
	app := cacheTestApp(t)
	app.Config.App.EncryptionKey = "this-is-a-32-character-test-key-XX" // gitleaks:allow -- synthetic unit-test key

	encTok, err := crypto.Encrypt("plain-token", app.Config.App.EncryptionKey)
	require.NoError(t, err)
	encSecret, err := crypto.Encrypt("plain-secret", app.Config.App.EncryptionKey)
	require.NoError(t, err)

	acc := &models.WhatsAppAccount{AccessToken: encTok, AppSecret: encSecret}
	app.decryptAccountSecrets(acc)
	assert.Equal(t, "plain-token", acc.AccessToken)
	assert.Equal(t, "plain-secret", acc.AppSecret)
}

func TestDecryptAccountSecrets_LeavesLegacyPlaintextUnchanged(t *testing.T) {
	app := cacheTestApp(t)
	app.Config.App.EncryptionKey = "this-is-a-32-character-test-key-XX" // gitleaks:allow -- synthetic unit-test key

	// No "enc:" prefix → legacy value, returned as-is.
	acc := &models.WhatsAppAccount{AccessToken: "legacy-plain", AppSecret: "legacy-secret"}
	app.decryptAccountSecrets(acc)
	assert.Equal(t, "legacy-plain", acc.AccessToken)
	assert.Equal(t, "legacy-secret", acc.AppSecret)
}

func TestPrepareWhatsAppAccountForRuntimeRejectsExpiredOrUndecryptableToken(t *testing.T) {
	t.Parallel()

	t.Run("expired", func(t *testing.T) {
		expiresAt := time.Now().UTC().Add(-time.Minute)
		app := &App{Config: &config.Config{}}
		err := app.prepareWhatsAppAccountForRuntime(&models.WhatsAppAccount{
			AccessToken:          "legacy-token",
			AccessTokenExpiresAt: &expiresAt,
		})
		require.ErrorContains(t, err, "expired")
	})

	t.Run("encrypted token without server key", func(t *testing.T) {
		encrypted, err := crypto.Encrypt("tenant-token", "test-runtime-account-key")
		require.NoError(t, err)
		app := &App{Config: &config.Config{}}
		err = app.prepareWhatsAppAccountForRuntime(&models.WhatsAppAccount{
			AccessToken: encrypted,
		})
		require.ErrorContains(t, err, "could not be decrypted")
	})

	t.Run("valid encrypted token", func(t *testing.T) {
		const encryptionKey = "test-runtime-account-key"
		encrypted, err := crypto.Encrypt("tenant-token", encryptionKey)
		require.NoError(t, err)
		app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: encryptionKey}}}
		account := &models.WhatsAppAccount{AccessToken: encrypted}
		require.NoError(t, app.prepareWhatsAppAccountForRuntime(account))
		assert.Equal(t, "tenant-token", account.AccessToken)
	})
}

// --- getWebhooksCached ---

func TestGetWebhooksCached_OnlyActiveAndOrgScoped(t *testing.T) {
	app := cacheTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)

	// Active webhook in target org.
	require.NoError(t, app.DB.Create(&models.Webhook{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgA.ID,
		Name: "active-A", URL: "https://a.example.com", IsActive: true,
	}).Error)
	// Inactive webhook in target org — must be excluded. GORM's default:true tag
	// makes Create ignore an explicit `false`; insert active first, then flip.
	inactive := &models.Webhook{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgA.ID,
		Name: "inactive-A", URL: "https://a2.example.com", IsActive: true,
	}
	require.NoError(t, app.DB.Create(inactive).Error)
	require.NoError(t, app.DB.Model(inactive).Update("is_active", false).Error)
	// Active webhook in other org — must be excluded.
	require.NoError(t, app.DB.Create(&models.Webhook{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgB.ID,
		Name: "active-B", URL: "https://b.example.com", IsActive: true,
	}).Error)

	// Clear cache to force DB read.
	app.InvalidateWebhooksCache(orgA.ID)

	got, err := app.getWebhooksCached(orgA.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "active-A", got[0].Name)
}

func TestGetWebhooksCached_CacheHitSkipsDB(t *testing.T) {
	app := cacheTestApp(t)
	orgID := uuid.New()

	// Plant a synthetic cache entry — there is no matching row in the DB.
	cacheKey := fmt.Sprintf("%s%s", webhooksCachePrefix, orgID.String())
	cached := []models.Webhook{{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: "from-cache", IsActive: true,
	}}
	data, err := json.Marshal(cached)
	require.NoError(t, err)
	require.NoError(t, app.Redis.Set(context.Background(), cacheKey, data, time.Hour).Err())

	got, err := app.getWebhooksCached(orgID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "from-cache", got[0].Name, "cache hit must skip the DB read")
}

func TestInvalidateWebhooksCache_DeletesKey(t *testing.T) {
	app := cacheTestApp(t)
	orgID := uuid.New()
	cacheKey := fmt.Sprintf("%s%s", webhooksCachePrefix, orgID.String())
	require.NoError(t, app.Redis.Set(context.Background(), cacheKey, "[]", time.Hour).Err())

	app.InvalidateWebhooksCache(orgID)

	exists, err := app.Redis.Exists(context.Background(), cacheKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

// --- getSLAEnabledSettingsCached ---

func TestGetSLAEnabledSettingsCached_OnlySLAEnabledRows(t *testing.T) {
	app := cacheTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)

	// Wipe any pre-existing rows so we can assert exact counts.
	require.NoError(t, app.DB.Exec("DELETE FROM chatbot_settings").Error)
	app.InvalidateSLASettingsCache()

	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgA.ID,
		WhatsAppAccount: "acc-A", IsEnabled: true,
		SLA: models.SLAConfig{Enabled: true},
	}).Error)
	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgB.ID,
		WhatsAppAccount: "acc-B", IsEnabled: true,
		SLA: models.SLAConfig{Enabled: true},
	}).Error)
	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgA.ID,
		WhatsAppAccount: "acc-A2", IsEnabled: true,
		SLA: models.SLAConfig{Enabled: false},
	}).Error)

	got, err := app.getSLAEnabledSettingsCached()
	require.NoError(t, err)
	require.Len(t, got, 2, "only SLA-enabled rows must be returned, regardless of org")
	for _, s := range got {
		assert.True(t, s.SLA.Enabled)
	}
}

func TestInvalidateSLASettingsCache_DeletesKey(t *testing.T) {
	app := cacheTestApp(t)
	require.NoError(t, app.Redis.Set(context.Background(), slaSettingsCacheKey, "[]", time.Hour).Err())

	app.InvalidateSLASettingsCache()

	exists, err := app.Redis.Exists(context.Background(), slaSettingsCacheKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

// --- deleteKeysByPattern ---

func TestDeleteKeysByPattern_RemovesAllMatchingKeys(t *testing.T) {
	app := cacheTestApp(t)
	ctx := context.Background()

	prefix := "test:cache:" + uuid.New().String()[:8] + ":"
	for i := range 5 {
		require.NoError(t, app.Redis.Set(ctx, fmt.Sprintf("%s%d", prefix, i), "x", time.Hour).Err())
	}
	// Sentinel key with a similar but non-matching prefix.
	other := "test:other:" + uuid.New().String()[:8]
	require.NoError(t, app.Redis.Set(ctx, other, "y", time.Hour).Err())

	app.deleteKeysByPattern(ctx, prefix+"*")

	for i := range 5 {
		exists, err := app.Redis.Exists(ctx, fmt.Sprintf("%s%d", prefix, i)).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists, "key %d should have been deleted", i)
	}
	exists, err := app.Redis.Exists(ctx, other).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "non-matching key must be left alone")
}

// --- InvalidateChatbotFlowsCache (uses Del on a single key) ---

func TestInvalidateChatbotFlowsCache_DeletesKey(t *testing.T) {
	app := cacheTestApp(t)
	orgID := uuid.New()
	cacheKey := fmt.Sprintf("%s%s", flowsCachePrefix, orgID.String())
	require.NoError(t, app.Redis.Set(context.Background(), cacheKey, "[]", time.Hour).Err())

	app.InvalidateChatbotFlowsCache(orgID)

	exists, err := app.Redis.Exists(context.Background(), cacheKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists)
}

// --- InvalidateChatbotSettingsCache (pattern-based) ---

func TestInvalidateChatbotSettingsCache_DeletesAllAccountVariants(t *testing.T) {
	app := cacheTestApp(t)
	ctx := context.Background()
	orgID := uuid.New()

	// Plant several account-specific cache keys for the same org.
	keyA := fmt.Sprintf("%s%s:%s", settingsCachePrefix, orgID.String(), "acc-A")
	keyB := fmt.Sprintf("%s%s:%s", settingsCachePrefix, orgID.String(), "acc-B")
	keyOther := fmt.Sprintf("%s%s:%s", settingsCachePrefix, uuid.New().String(), "acc-X")
	require.NoError(t, app.Redis.Set(ctx, keyA, "{}", time.Hour).Err())
	require.NoError(t, app.Redis.Set(ctx, keyB, "{}", time.Hour).Err())
	require.NoError(t, app.Redis.Set(ctx, keyOther, "{}", time.Hour).Err())

	app.InvalidateChatbotSettingsCache(orgID)

	// Both org keys gone…
	for _, k := range []string{keyA, keyB} {
		exists, err := app.Redis.Exists(ctx, k).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(0), exists, "expected %s to be deleted", k)
	}
	// …other org's key untouched.
	exists, err := app.Redis.Exists(ctx, keyOther).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), exists, "other org's cache must not be invalidated")
}

type accountCacheMutationFault struct {
	key     string
	blocked atomic.Int32
}

func (h *accountCacheMutationFault) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *accountCacheMutationFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *accountCacheMutationFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if (cmd.Name() == "del" || cmd.Name() == "set") && len(cmd.Args()) > 1 && fmt.Sprint(cmd.Args()[1]) == h.key {
			h.blocked.Add(1)
			return fmt.Errorf("synthetic account cache mutation unavailable")
		}
		return next(ctx, cmd)
	}
}

func blockAccountCacheMutations(t *testing.T, app *App, phoneID string) *accountCacheMutationFault {
	t.Helper()
	// SetupTestRedis is package-shared. Give the fault a private client so it
	// cannot poison another test or prevent cleanup through the healthy client.
	options := *app.Redis.Options()
	client := redis.NewClient(&options)
	t.Cleanup(func() { _ = client.Close() })
	hook := &accountCacheMutationFault{key: whatsappAccountCachePrefix + phoneID}
	client.AddHook(hook)
	app.Redis = client
	return hook
}

func TestWhatsAppAccountCacheRejectsStaleIdentityWhenEvictionFails(t *testing.T) {
	for _, entry := range []string{"root", "phone-tenant"} {
		for _, sameOrg := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-org=%v", entry, sameOrg), func(t *testing.T) {
				app := cacheTestApp(t)
				priorOrg := testutil.CreateTestOrganization(t, app.DB)
				currentOrg := priorOrg
				if !sameOrg {
					currentOrg = testutil.CreateTestOrganization(t, app.DB)
				}
				phoneID := "cache-reclaim-" + uuid.NewString()
				prior := makeAccount(t, app, priorOrg.ID, "  "+phoneID+"  ", "prior-owner-token", "prior-secret")
				_, err := app.getWhatsAppAccountCached(phoneID)
				require.NoError(t, err)
				key := whatsappAccountCachePrefix + phoneID
				healthyRedis := app.Redis
				stale, err := healthyRedis.Get(context.Background(), key).Result()
				require.NoError(t, err)
				t.Cleanup(func() { _ = healthyRedis.Del(context.Background(), key).Err() })
				require.NoError(t, app.DB.Delete(prior).Error)
				current := makeAccount(t, app, currentOrg.ID, phoneID, "current-owner-token", "current-secret")
				hook := blockAccountCacheMutations(t, app, phoneID)
				for attempt := 0; attempt < 2; attempt++ {
					var got *models.WhatsAppAccount
					if entry == "root" {
						got, err = app.getWhatsAppAccountCached("  " + phoneID + "  ")
					} else {
						err = app.withPhoneTenant(phoneID, func(scoped *App) error {
							assert.False(t, scoped.hasTenantScope(), "non-RLS logical identity is not a transaction")
							assert.Equal(t, currentOrg.ID, scoped.expectedAccountOrganizationID)
							var lookupErr error
							got, lookupErr = scoped.getWhatsAppAccountCached(phoneID)
							return lookupErr
						})
					}
					require.NoError(t, err)
					require.NotNil(t, got)
					assert.Equal(t, current.ID, got.ID)
					assert.Equal(t, currentOrg.ID, got.OrganizationID)
					assert.Equal(t, "current-owner-token", got.AccessToken)
					assert.NotEqual(t, prior.ID, got.ID)
					stillStale, readErr := healthyRedis.Get(context.Background(), key).Result()
					require.NoError(t, readErr)
					assert.Equal(t, stale, stillStale, "the safety proof cannot depend on successful eviction or replacement")
				}
				assert.GreaterOrEqual(t, hook.blocked.Load(), int32(4), "both stale eviction and cache refill fail on both reads")
			})
		}
	}
}

func TestWhatsAppAccountCacheRejectsMissingOrUnavailableLiveIdentity(t *testing.T) {
	for _, failure := range []string{"deleted", "phone-moved", "wrong-tenant", "wrong-tenant-no-redis", "database"} {
		t.Run(failure, func(t *testing.T) {
			app := cacheTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			otherOrg := testutil.CreateTestOrganization(t, app.DB)
			phoneID := "cache-refuse-" + uuid.NewString()
			account := makeAccount(t, app, org.ID, phoneID, "cached-secret-token", "cached-app-secret")
			_, err := app.getWhatsAppAccountCached(phoneID)
			require.NoError(t, err)
			key := whatsappAccountCachePrefix + phoneID
			healthyRedis := app.Redis
			t.Cleanup(func() { _ = healthyRedis.Del(context.Background(), key).Err() })
			stale, err := healthyRedis.Get(context.Background(), key).Result()
			require.NoError(t, err)
			var faultHits atomic.Int32
			switch failure {
			case "deleted":
				require.NoError(t, app.DB.Delete(account).Error)
			case "phone-moved":
				require.NoError(t, app.DB.Model(account).Update("phone_id", phoneID+"-new").Error)
			case "wrong-tenant-no-redis":
				app.Redis = nil
			case "database":
				private, _ := h1OpenPool(t, 1)
				require.NoError(t, private.Callback().Query().Before("gorm:query").Register("test:cache_identity_fault", func(tx *gorm.DB) {
					if accountCacheIdentityQuery(tx) {
						faultHits.Add(1)
						// Real PostgreSQL error on this handle; the cache must not
						// mask it. Exec has a separate statement, not a recursive query.
						tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec("SELECT 1 / 0").Error)
					}
				}))
				app.DB = private
			}
			var got *models.WhatsAppAccount
			if failure == "wrong-tenant" || failure == "wrong-tenant-no-redis" {
				err = app.WithTenantApp(otherOrg.ID, func(scoped *App) error {
					assert.False(t, scoped.hasTenantScope())
					var lookupErr error
					got, lookupErr = scoped.getWhatsAppAccountCached(phoneID)
					return lookupErr
				})
			} else {
				got, err = app.getWhatsAppAccountCached(phoneID)
			}
			require.Error(t, err)
			assert.Nil(t, got, "unavailable authority must never return cached credentials")
			if failure == "database" {
				assert.Equal(t, "22012", postgresErrorCode(err))
				assert.EqualValues(t, 1, faultHits.Load())
				require.NoError(t, app.DB.Callback().Query().Remove("test:cache_identity_fault"))
				recovered, recoveryErr := app.getWhatsAppAccountCached(phoneID)
				require.NoError(t, recoveryErr)
				require.NotNil(t, recovered)
				assert.Equal(t, account.ID, recovered.ID)
			} else {
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
			}
			stillStale, readErr := healthyRedis.Get(context.Background(), key).Result()
			require.NoError(t, readErr)
			assert.Equal(t, stale, stillStale)
			require.NoError(t, app.DB.Exec("SELECT 1").Error)
		})
	}
}

func accountCacheIdentityQuery(tx *gorm.DB) bool {
	return tx.Statement.Table == "whatsapp_accounts" && len(tx.Statement.Selects) == 3 &&
		tx.Statement.Selects[0] == "id" && tx.Statement.Selects[1] == "organization_id" && tx.Statement.Selects[2] == "phone_id"
}

func TestWhatsAppAccountCachePinsDiscoveryOrganizationAcrossOwnerChange(t *testing.T) {
	for _, sameOrg := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-org=%v", sameOrg), func(t *testing.T) {
			app := cacheTestApp(t)
			priorOrg := testutil.CreateTestOrganization(t, app.DB)
			currentOrg := priorOrg
			if !sameOrg {
				currentOrg = testutil.CreateTestOrganization(t, app.DB)
			}
			phoneID := "cache-discovery-" + uuid.NewString()
			t.Cleanup(func() { _ = app.Redis.Del(context.Background(), whatsappAccountCachePrefix+phoneID).Err() })
			prior := makeAccount(t, app, priorOrg.ID, phoneID, "prior-token", "prior-secret")
			_, err := app.getWhatsAppAccountCached(phoneID)
			require.NoError(t, err)
			var current *models.WhatsAppAccount
			var got *models.WhatsAppAccount
			err = app.withPhoneTenant(phoneID, func(scoped *App) error {
				// Discovery has completed, and this callback still belongs to its
				// original logical tenant even though non-RLS uses the root pool.
				assert.False(t, scoped.hasTenantScope())
				assert.Equal(t, priorOrg.ID, scoped.expectedAccountOrganizationID)
				require.NoError(t, app.DB.Delete(prior).Error)
				current = makeAccount(t, app, currentOrg.ID, phoneID, "new-token", "new-secret")
				var lookupErr error
				got, lookupErr = scoped.getWhatsAppAccountCached(phoneID)
				return lookupErr
			})
			if sameOrg {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, current.ID, got.ID)
				assert.Equal(t, "new-token", got.AccessToken)
			} else {
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				assert.Nil(t, got, "the old callback must not adopt the new tenant")
			}
			fresh, err := app.getWhatsAppAccountCached(phoneID)
			require.NoError(t, err)
			require.NotNil(t, fresh)
			assert.Equal(t, current.ID, fresh.ID)
			assert.Equal(t, currentOrg.ID, fresh.OrganizationID)
		})
	}
}

func TestWhatsAppAccountCacheIdentityUsesHeldSingleConnection(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("rls=%v", rlsEnabled), func(t *testing.T) {
			app := cacheTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			phoneID := "cache-one-connection-" + uuid.NewString()
			t.Cleanup(func() { _ = app.Redis.Del(context.Background(), whatsappAccountCachePrefix+phoneID).Err() })
			account := makeAccount(t, app, org.ID, phoneID, "single-connection-token", "secret")
			_, err := app.getWhatsAppAccountCached(phoneID)
			require.NoError(t, err)
			private, pool := h1OpenPool(t, 1)
			var probes atomic.Int32
			require.NoError(t, private.Callback().Query().Before("gorm:query").Register("test:cache_identity_probe", func(tx *gorm.DB) {
				if accountCacheIdentityQuery(tx) {
					probes.Add(1)
				}
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			app.DB = private.WithContext(ctx)
			app.Config.Database.RLSEnabled = rlsEnabled
			waits := pool.Stats().WaitCount
			err = app.WithCommittedTenantApp(org.ID, func(scoped *App) error {
				assert.Equal(t, 1, pool.Stats().InUse)
				got, lookupErr := scoped.getWhatsAppAccountCached(phoneID)
				if lookupErr != nil {
					return lookupErr
				}
				require.NotNil(t, got)
				assert.Equal(t, account.ID, got.ID)
				assert.Equal(t, 1, pool.Stats().InUse)
				return nil
			})
			require.NoError(t, err, "an identity probe must use the held connection, never the root pool")
			assert.EqualValues(t, 1, probes.Load())
			assert.Equal(t, waits, pool.Stats().WaitCount)
			assert.Zero(t, pool.Stats().InUse)
		})
	}
}

// Serial: installing actual runtime-role policies changes shared test schema.
func TestWhatsAppAccountCacheRuntimeRoleRLSChecksCurrentOwner(t *testing.T) {
	app := cacheTestApp(t)
	admin := app.DB
	testutil.TruncateTables(admin)
	runtimeRole := "rereply_cache_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, admin.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole, runtimePassword,
	)).Error)
	t.Cleanup(func() {
		_ = database.RemoveTenantRLS(admin)
		_ = admin.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = admin.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(admin)
	})
	priorOrg := testutil.CreateTestOrganization(t, admin)
	currentOrg := testutil.CreateTestOrganization(t, admin)
	phoneID := "cache-runtime-" + uuid.NewString()
	healthyRedis := app.Redis
	t.Cleanup(func() { _ = healthyRedis.Del(context.Background(), whatsappAccountCachePrefix+phoneID).Err() })
	prior := makeAccount(t, app, priorOrg.ID, phoneID, "prior-runtime-token", "prior-secret")
	_, err := app.getWhatsAppAccountCached(phoneID)
	require.NoError(t, err)
	require.NoError(t, admin.Delete(prior).Error)
	current := makeAccount(t, app, currentOrg.ID, phoneID, "current-runtime-token", "current-secret")
	require.NoError(t, database.ApplyTenantRLS(admin, runtimeRole))
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))
	pool, err := runtimeDB.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app.DB = runtimeDB.WithContext(ctx)
	app.Config.Database.RLSEnabled = true
	app.Config.Database.RuntimeRole = runtimeRole
	hook := blockAccountCacheMutations(t, app, phoneID)
	got, err := app.getWhatsAppAccountCached(phoneID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, current.ID, got.ID)
	assert.Equal(t, currentOrg.ID, got.OrganizationID)
	assert.Equal(t, "current-runtime-token", got.AccessToken)
	assert.GreaterOrEqual(t, hook.blocked.Load(), int32(2))
	err = app.WithTenantApp(priorOrg.ID, func(scoped *App) error {
		refused, lookupErr := scoped.getWhatsAppAccountCached(phoneID)
		assert.Nil(t, refused)
		return lookupErr
	})
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var unscoped int64
	require.NoError(t, runtimeDB.Model(&models.WhatsAppAccount{}).Count(&unscoped).Error)
	assert.Zero(t, unscoped, "the root runtime connection must retain no tenant setting")
}
