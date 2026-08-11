package worker

import (
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
)

// configuredMetaMessengerReviewAccount is a denial predicate and therefore
// deliberately ignores authority expiry and mutable markers. Expiry or marker
// corruption must never turn the deployment-pinned review row into an egress
// account in a long-running worker.
func configuredMetaMessengerReviewAccount(
	cfg *config.Config,
	account *models.ChannelAccount,
) bool {
	if cfg == nil || cfg.App.Environment != "staging" ||
		!cfg.MetaMessengerReviewRelay.Enabled ||
		cfg.MetaMessengerReviewRelay.Mode != metareview.Mode {
		return false
	}
	settings := cfg.MetaMessengerReviewRelay
	return metareview.DeploymentIdentityMatches(metareview.ProvisionTuple{
		OrganizationID:   settings.OrganizationID,
		MetaBusinessID:   settings.MetaBusinessID,
		PageID:           settings.PageID,
		MetaAppID:        cfg.MetaMessengerOnboarding.AppID,
		ChannelAccountID: settings.ChannelAccountID,
		Generation:       settings.Generation,
		ExpiresAt:        settings.ExpiresAt,
	}, account)
}
