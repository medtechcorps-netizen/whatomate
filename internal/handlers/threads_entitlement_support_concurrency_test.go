package handlers_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// This regression queues revoke behind an uncommitted grant on the same
// organization row. Both operations must report their own committed result;
// neither may perform a post-commit check that observes the inverse operation
// and turns a successful mutation into a 500 response.
func TestThreadsSupportEnableThenRevokeInverseRaceReturnsCommittedResults(t *testing.T) {
	app := newTestApp(t)
	controlOrg := testutil.CreateTestOrganization(t, app.DB)
	targetOrg := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrg.ID,
		testutil.WithEmail(testutil.UniqueEmail("threads-inverse-race-owner")),
		testutil.WithSuperAdmin(),
	)
	growthPlan := ensureThreadsSupportCatalogPlan(
		t,
		app,
		"rereply-growth",
		"ReReply Growth",
	)
	createThreadsSupportSubscription(
		t,
		app,
		targetOrg,
		&growthPlan,
		models.SubscriptionStatusActive,
		time.Now().UTC().Add(30*24*time.Hour),
		true,
	)

	enableRequest := newThreadsSupportRequest(
		t,
		targetOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{"reason": "inverse-race-grant"},
	)

	createdOverrideID := make(chan uuid.UUID, 1)
	releaseEnable := make(chan struct{})
	revokeAtOrganizationLock := make(chan struct{})
	var grantCreated atomic.Bool
	var createdOnce sync.Once
	var lockOnce sync.Once
	createCallback := "test:threads-inverse-race:create:" + uuid.NewString()
	queryCallback := "test:threads-inverse-race:query:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Create().
		After("gorm:create").
		Register(createCallback, func(tx *gorm.DB) {
			if tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "entitlement_overrides" {
				return
			}
			override, ok := tx.Statement.Dest.(*models.EntitlementOverride)
			if !ok {
				_ = tx.AddError(fmt.Errorf("unexpected entitlement override create destination %T", tx.Statement.Dest))
				return
			}
			grantCreated.Store(true)
			createdOnce.Do(func() { createdOverrideID <- override.ID })
			select {
			case <-releaseEnable:
			case <-time.After(5 * time.Second):
				_ = tx.AddError(fmt.Errorf("timed out releasing grant transaction"))
			}
		}))
	require.NoError(t, app.DB.Callback().Query().
		Before("gorm:query").
		Register(queryCallback, func(tx *gorm.DB) {
			if !grantCreated.Load() || tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "organizations" {
				return
			}
			if _, locksOrganization := tx.Statement.Clauses["FOR"]; !locksOrganization {
				return
			}
			lockOnce.Do(func() { close(revokeAtOrganizationLock) })
		}))
	t.Cleanup(func() {
		_ = app.DB.Callback().Create().Remove(createCallback)
		_ = app.DB.Callback().Query().Remove(queryCallback)
	})

	enableDone := make(chan error, 1)
	go func() {
		enableDone <- app.EnableOrganizationThreadsPublicEngagement(enableRequest)
	}()

	var overrideID uuid.UUID
	select {
	case overrideID = <-createdOverrideID:
	case <-time.After(5 * time.Second):
		t.Fatal("grant did not reach the create barrier")
	}
	revokeRequest := newThreadsSupportRequest(
		t,
		targetOrg.ID,
		owner,
		targetOrg.ID,
		map[string]any{
			"override_id": overrideID,
			"reason":      "inverse-race-revoke",
		},
	)
	revokeDone := make(chan error, 1)
	go func() {
		revokeDone <- app.RevokeOrganizationThreadsPublicEngagementSupport(revokeRequest)
	}()

	select {
	case <-revokeAtOrganizationLock:
	case <-time.After(5 * time.Second):
		close(releaseEnable)
		t.Fatal("revoke did not queue on the organization lock")
	}
	close(releaseEnable)

	select {
	case err := <-enableDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("enable request did not finish")
	}
	select {
	case err := <-revokeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("revoke request did not finish")
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(enableRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(revokeRequest))

	var enableEnvelope struct {
		Data handlers.EnableThreadsPublicEngagementResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(enableRequest), &enableEnvelope))
	assert.True(t, enableEnvelope.Data.EffectiveEnabled)
	var revokeEnvelope struct {
		Data handlers.RevokeThreadsPublicEngagementSupportResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(revokeRequest), &revokeEnvelope))
	assert.True(t, revokeEnvelope.Data.Revoked)
	assert.False(t, revokeEnvelope.Data.EffectiveEnabled)

	var stored models.EntitlementOverride
	require.NoError(t, app.DB.First(&stored, "id = ?", overrideID).Error)
	assert.False(t, stored.IsActive)
	assert.Equal(t, "inverse-race-revoke", stored.RevocationReason)
}
