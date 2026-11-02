package handlers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestManagedInstagramActivationRechecksFreshnessAfterLocks(t *testing.T) {
	for _, lockKind := range []string{"organization", "account"} {
		for _, test := range []struct {
			name    string
			elapsed time.Duration
			allowed bool
		}{
			{name: "expired subscription approval", elapsed: 61 * time.Second},
			{name: "still fresh approval", elapsed: 30 * time.Second, allowed: true},
		} {
			t.Run(lockKind+"/"+test.name, func(t *testing.T) {
				fixture := newMetaInstagramLifecycleFixture(t)
				now := time.Now().UTC().Truncate(time.Microsecond)
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_activation_state"] = "awaiting_admin_approval"
				metadata["meta_health_checked_at"] = now.Format(time.RFC3339Nano)
				metadata["meta_ownership_checked_at"] = now.Format(time.RFC3339Nano)
				metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
				metadata["meta_health_oauth_version"] = fixture.oauth.Version
				metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
				metadata["meta_health_webhook_version"] = fixture.webhook.Version
				accountConfig := cloneJSONB(fixture.account.Config)
				accountConfig["outbound_enabled"] = false
				accountConfig["ai_reply_enabled"] = false
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Updates(map[string]any{
					"status": models.ChannelAccountStatusPending, "config": accountConfig,
					"metadata": metadata, "last_health_check_at": now,
					"last_error": "", "last_error_at": nil, "connected_at": nil,
				}).Error)
				approval := metaInstagramSubscriptionApproval{
					ProfileID: fixture.profileID, CheckedAt: now,
					OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
					WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
				}
				var clockNanos atomic.Int64
				var clockReads atomic.Int32
				clockNanos.Store(now.UnixNano())
				clock := func() time.Time {
					clockReads.Add(1)
					return time.Unix(0, clockNanos.Load()).UTC()
				}
				lockTx := fixture.db.Begin()
				require.NoError(t, lockTx.Error)
				released, started, joined := false, false, false
				done := make(chan error, 1)
				activationCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				t.Cleanup(func() {
					if !released {
						_ = lockTx.Rollback().Error
					}
					cancel()
					if started && !joined {
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("activation did not finish after cleanup released its lock")
						}
					}
				})
				if lockKind == "organization" {
					require.NoError(t, lockChannelAIOrganizationScopeTx(lockTx, fixture.org.ID))
				} else {
					var locked models.ChannelAccount
					require.NoError(t, lockTx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", fixture.account.ID).First(&locked).Error)
				}
				pidReady := make(chan int, 1)
				started = true
				go func() {
					done <- fixture.db.WithContext(activationCtx).Connection(func(connection *gorm.DB) error {
						session := connection.Session(&gorm.Session{NewDB: true})
						var pid int
						if err := session.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
							pidReady <- 0
							return err
						}
						pidReady <- pid
						requestApp := &App{Config: fixture.app.Config, DB: session, Redis: fixture.app.Redis, Log: fixture.app.Log}
						_, err := requestApp.activateMetaInstagramAccount(fixture.org.ID, fixture.user.ID, fixture.account.ID, clock, approval)
						return err
					})
				}()
				var pid int
				select {
				case pid = <-pidReady:
				case <-time.After(5 * time.Second):
					t.Fatal("activation did not acquire a database connection")
				}
				require.Positive(t, pid)
				testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, pid)
				require.Zero(t, clockReads.Load(), "approval clock must not be sampled before database locks")
				clockNanos.Store(now.Add(test.elapsed).UnixNano())
				require.NoError(t, lockTx.Commit().Error)
				released = true
				select {
				case err := <-done:
					joined = true
					if test.allowed {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, "subscription approval was superseded")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("activation did not finish after the database lock was released")
				}
				assert.EqualValues(t, 1, clockReads.Load())
				var stored models.ChannelAccount
				require.NoError(t, fixture.db.First(&stored, "id = ?", fixture.account.ID).Error)
				assert.Equal(t, test.allowed, stored.Status == models.ChannelAccountStatusActive)
				assert.Equal(t, test.allowed, boolConfigValue(stored.Config, "outbound_enabled"))
				assert.False(t, boolConfigValue(stored.Config, "ai_reply_enabled"))
				if !test.allowed {
					assert.Equal(t, models.ChannelAccountStatusPending, stored.Status)
					assert.Equal(t, "awaiting_admin_approval", stringConfigValue(stored.Metadata, "meta_activation_state"))
					assert.Empty(t, stringConfigValue(stored.Metadata, "meta_activated_at"))
					assert.Nil(t, stored.ConnectedAt)
				}
			})
		}
	}
}
