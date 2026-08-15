package handlers

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

const threadsOAuthCallbackPath = "/api/integrations/threads/callback"

// threadsPublicSetupURLs returns only non-secret callback values that an
// operator must copy into the dedicated Meta Threads application. All URLs use
// the exact origin and base path of the configured OAuth redirect.
func (a *App) threadsPublicSetupURLs(
	organizationID uuid.UUID,
	redirectURI string,
	accountID *uuid.UUID,
) models.JSONB {
	result := models.JSONB{}
	redirectURI = strings.TrimSpace(redirectURI)
	if redirectURI == "" {
		return result
	}
	result["oauth_callback_url"] = redirectURI

	base, basePath, err := a.threadsCallbackBaseURL(redirectURI)
	if err != nil || organizationID == uuid.Nil {
		return result
	}
	callbackURL := func(path string) string {
		copy := *base
		copy.Path = basePath + path
		copy.RawPath = ""
		copy.RawQuery = ""
		copy.ForceQuery = false
		copy.Fragment = ""
		return copy.String()
	}
	result["deauthorization_callback_url"] = callbackURL(fmt.Sprintf(
		"/api/integrations/threads/%s/deauthorize",
		organizationID.String(),
	))
	result["data_deletion_callback_url"] = callbackURL(fmt.Sprintf(
		"/api/integrations/threads/%s/data-deletion",
		organizationID.String(),
	))
	if accountID != nil && *accountID != uuid.Nil {
		result["account_webhook_callback_url"] = callbackURL(fmt.Sprintf(
			"/api/webhooks/channels/%s",
			accountID.String(),
		))
	}
	return result
}

func (a *App) threadsCallbackBaseURL(redirectURI string) (*url.URL, string, error) {
	if validateThreadsRedirectURI(redirectURI) != nil {
		return nil, "", errThreadsOAuthNotConfigured
	}
	parsed, err := url.Parse(strings.TrimSpace(redirectURI))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, "", errThreadsOAuthNotConfigured
	}
	redirectPath := strings.TrimRight(parsed.Path, "/")
	basePath := strings.TrimSuffix(redirectPath, threadsOAuthCallbackPath)
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed, basePath, nil
}
