package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metareview"
)

var (
	errMetaMessengerReviewPreauthAuthority = errors.New("staging Messenger review preauthorization authority is inactive")
	errMetaMessengerReviewPreauthBody      = errors.New("staging Messenger review preauthorization body is invalid")
	errMetaMessengerReviewPreauthHeader    = errors.New("staging Messenger review preauthorization header is invalid")
	errMetaMessengerReviewPreauthProof     = errors.New("staging Messenger review preauthorization proof is invalid")
)

// preauthorizeMetaMessengerReviewWebhook authenticates requests for the one
// deployment-pinned review account before tenant discovery or any database
// access. The returned bool says that the path names that reserved account,
// including when its deployment authority is expired or otherwise inactive.
// Callers deliberately collapse every error to one public not-found response.
func (a *App) preauthorizeMetaMessengerReviewWebhook(
	accountID uuid.UUID,
	headers http.Header,
	body []byte,
	now time.Time,
) (bool, error) {
	if !a.deploymentPinsMetaMessengerReviewAccountID(accountID) {
		return false, nil
	}
	if len(body) == 0 || len(body) > metareview.MaximumInboundBodyBytes {
		return true, errMetaMessengerReviewPreauthBody
	}

	settings, tuple, err := a.metaMessengerReviewSettings(now)
	if err != nil || tuple.ChannelAccountID != accountID.String() {
		return true, errMetaMessengerReviewPreauthAuthority
	}

	generation, ok := exactMetaMessengerReviewHeader(headers, metareview.GenerationHeader)
	if !ok || generation != tuple.Generation {
		return true, errMetaMessengerReviewPreauthHeader
	}
	credentialIDText, ok := exactMetaMessengerReviewHeader(headers, metareview.CredentialIDHeader)
	if !ok {
		return true, errMetaMessengerReviewPreauthHeader
	}
	credentialID, err := uuid.Parse(credentialIDText)
	if err != nil || credentialID == uuid.Nil || credentialID.String() != credentialIDText {
		return true, errMetaMessengerReviewPreauthHeader
	}
	credentialVersionText, ok := exactMetaMessengerReviewHeader(
		headers,
		metareview.CredentialVersionHeader,
	)
	if !ok {
		return true, errMetaMessengerReviewPreauthHeader
	}
	credentialVersion, err := strconv.Atoi(credentialVersionText)
	if err != nil || credentialVersion <= 0 ||
		strconv.Itoa(credentialVersion) != credentialVersionText {
		return true, errMetaMessengerReviewPreauthHeader
	}
	proof, ok := exactMetaMessengerReviewHeader(headers, metareview.ReviewProofHeader)
	if !ok {
		return true, errMetaMessengerReviewPreauthProof
	}
	if err := metareview.VerifyInboundProof(
		settings.ProviderProofSecret,
		tuple,
		credentialIDText,
		credentialVersion,
		body,
		proof,
	); err != nil {
		return true, errMetaMessengerReviewPreauthProof
	}
	return true, nil
}

// deploymentPinsMetaMessengerReviewAccountID intentionally reads only
// process configuration. It keeps an expired reserved route on the preauth
// path so expiry cannot make the handler fall back to tenant discovery.
func (a *App) deploymentPinsMetaMessengerReviewAccountID(accountID uuid.UUID) bool {
	if a == nil || a.Config == nil || accountID == uuid.Nil {
		return false
	}
	// The immutable account UUID alone reserves the route. If any surrounding
	// mode/environment/expiry setting is later invalid, metaMessengerReviewSettings
	// fails and this path stays closed instead of falling through to DB lookup.
	return a.Config.MetaMessengerReviewRelay.ChannelAccountID == accountID.String()
}

func exactMetaMessengerReviewHeader(headers http.Header, name string) (string, bool) {
	values, ok := headers[http.CanonicalHeaderKey(name)]
	if !ok || len(values) != 1 {
		return "", false
	}
	value := values[0]
	return value, value != "" && strings.TrimSpace(value) == value
}
