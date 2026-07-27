package channel

import (
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestSubscriptionPermitsOmnichannelFailsClosedAfterExpiry(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	assert.False(t, subscriptionPermitsOmnichannel(&models.Subscription{
		Status:           models.SubscriptionStatusActive,
		CurrentPeriodEnd: &expired,
	}, now))
	assert.True(t, subscriptionPermitsOmnichannel(&models.Subscription{
		Status:           models.SubscriptionStatusActive,
		CurrentPeriodEnd: &future,
	}, now))
	assert.False(t, subscriptionPermitsOmnichannel(&models.Subscription{
		Status:      models.SubscriptionStatusTrialing,
		TrialEndsAt: &expired,
	}, now))
	assert.False(t, subscriptionPermitsOmnichannel(nil, now))
}

func TestDurableOmnichannelEntitlementValues(t *testing.T) {
	t.Parallel()

	assert.True(t, durableEntitlementAllows(true))
	assert.True(t, durableEntitlementAllows(models.JSONB{"enabled": true}))
	assert.True(t, durableEntitlementAllows("enabled"))
	assert.False(t, durableEntitlementAllows(false))
	assert.False(t, durableEntitlementAllows("disabled"))
	assert.False(t, durableEntitlementAllows(nil))
}
