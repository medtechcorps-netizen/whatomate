package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	threadsLifecycleMaxRequestBytes = 16 * 1024
	threadsLifecycleClockSkew       = 5 * time.Minute
	threadsLifecycleSubjectType     = "threads_user"
	threadsLifecycleReceivedChannel = "threads"
	threadsLifecycleVerification    = "meta_signed_request"
	threadsLifecycleEventHashesKey  = "_lifecycle_event_hashes"
	threadsLifecycleEventHashLimit  = 8
	threadsLifecycleTimestampBucket = time.Second
)

var (
	errThreadsLifecycleMalformed     = errors.New("threads lifecycle request is malformed")
	errThreadsLifecycleUnauthorized  = errors.New("threads lifecycle request is unauthorized")
	errThreadsLifecycleNotConfigured = errors.New("threads lifecycle callback is not configured")
)

type threadsLifecycleConfig struct {
	IntegrationID uuid.UUID
	AppID         string
	RedirectURI   string
	AppSecret     string
	Config        models.JSONB
}

type threadsSignedRequestPayload struct {
	Algorithm string      `json:"algorithm"`
	IssuedAt  json.Number `json:"issued_at"`
	Expires   json.Number `json:"expires"`
	UserID    threadsID   `json:"user_id"`
}

type verifiedThreadsSignedRequest struct {
	UserID   string
	IssuedAt int64
	Hash     string
}

type threadsDataDeletionResponse struct {
	URL              string `json:"url"`
	ConfirmationCode string `json:"confirmation_code"`
}

// DeauthorizeThreads handles Meta's Threads uninstall/deauthorization ping.
// The tenant selector only chooses the encrypted app secret; the signed
// request is still required and its provider user must match the bound account.
func (a *App) DeauthorizeThreads(r *fastglue.Request) error {
	setThreadsLifecycleResponseHeaders(r)
	orgID, signedRequest, err := parseThreadsLifecycleRequest(r)
	if err != nil {
		return sendThreadsLifecycleError(r, fasthttp.StatusBadRequest)
	}

	err = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Transaction(func(tx *gorm.DB) error {
			config, err := scoped.lockThreadsLifecycleConfig(tx, orgID)
			if err != nil {
				return err
			}
			verified, err := verifyThreadsSignedRequest(
				signedRequest,
				config.AppSecret,
				time.Now().UTC(),
			)
			if err != nil {
				return err
			}
			eventHash := threadsLifecycleEventHash(
				"deauthorize",
				config.AppID,
				verified.UserID,
				verified.Hash,
			)
			if threadsLifecycleEventSeen(config.Config, eventHash) {
				return nil
			}
			accounts, err := lockMatchingThreadsLifecycleAccounts(
				tx,
				orgID,
				verified,
			)
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			if err := markThreadsLifecycleDisconnected(
				tx,
				orgID,
				config,
				accounts,
				now,
				"provider_deauthorized",
				"Threads authorization was revoked by the provider",
				eventHash,
			); err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		return a.sendThreadsLifecycleHandlerError(r, orgID, "deauthorize", err)
	}

	return sendThreadsLifecycleJSON(r, fasthttp.StatusOK, map[string]bool{"success": true})
}

// DeleteThreadsUserData accepts Meta's signed data-deletion request, starts a
// verified privacy workflow, and returns Meta's required status URL and
// confirmation code. Exact retries reuse the same request and code.
func (a *App) DeleteThreadsUserData(r *fastglue.Request) error {
	setThreadsLifecycleResponseHeaders(r)
	orgID, signedRequest, err := parseThreadsLifecycleRequest(r)
	if err != nil {
		return sendThreadsLifecycleError(r, fasthttp.StatusBadRequest)
	}

	var response threadsDataDeletionResponse
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Transaction(func(tx *gorm.DB) error {
			config, err := scoped.lockThreadsLifecycleConfig(tx, orgID)
			if err != nil {
				return err
			}
			verified, err := verifyThreadsSignedRequest(
				signedRequest,
				config.AppSecret,
				time.Now().UTC(),
			)
			if err != nil {
				return err
			}
			accounts, err := lockMatchingThreadsLifecycleAccounts(
				tx,
				orgID,
				verified,
			)
			if err != nil {
				return err
			}

			now := time.Now().UTC()
			request, created, err := findOrCreateThreadsDeletionRequest(
				tx,
				orgID,
				config.AppID,
				verified,
				now,
			)
			if err != nil {
				return err
			}
			if created {
				if err := markThreadsLifecycleDisconnected(
					tx,
					orgID,
					config,
					accounts,
					now,
					"data_deletion_requested",
					"Meta Threads data deletion was requested",
					request.VerificationTokenHash,
				); err != nil {
					return err
				}
			}
			statusURL, err := scoped.threadsDeletionStatusURL(
				config.RedirectURI,
				orgID,
				request.RequestNumber,
			)
			if err != nil {
				return errThreadsLifecycleNotConfigured
			}
			response = threadsDataDeletionResponse{
				URL:              statusURL,
				ConfirmationCode: request.RequestNumber,
			}
			return nil
		})
	})
	if err != nil {
		return a.sendThreadsLifecycleHandlerError(r, orgID, "data_deletion", err)
	}

	return sendThreadsLifecycleJSON(r, fasthttp.StatusOK, response)
}

// ThreadsDataDeletionStatus is the public, non-sensitive status page returned
// to Meta. The 128-bit confirmation code is the only request lookup token.
func (a *App) ThreadsDataDeletionStatus(r *fastglue.Request) error {
	setThreadsLifecycleResponseHeaders(r)
	orgID, err := parseThreadsLifecycleOrganization(r)
	confirmationCode := strings.TrimSpace(stringPathValue(r, "confirmation_code"))
	if err != nil || !validThreadsDeletionConfirmationCode(confirmationCode) {
		return sendThreadsLifecycleText(r, fasthttp.StatusNotFound, "Request not found")
	}

	var privacyRequest models.PrivacyRequest
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Where(
			"organization_id = ? AND request_number = ? AND type = ? AND received_channel = ? AND verification_method = ?",
			orgID,
			confirmationCode,
			models.PrivacyRequestTypeDeletion,
			threadsLifecycleReceivedChannel,
			threadsLifecycleVerification,
		).First(&privacyRequest).Error
	})
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			a.Log.Error(
				"Failed to load Threads data deletion status",
				"error", err,
				"organization_id", orgID,
			)
		}
		return sendThreadsLifecycleText(r, fasthttp.StatusNotFound, "Request not found")
	}

	displayStatus, statusReason := threadsDeletionDisplayStatus(
		privacyRequest.Status,
		privacyRequest.DecisionReason,
	)
	reasonHTML := ""
	if statusReason != "" {
		reasonHTML = fmt.Sprintf(
			"<p>Decision: %s</p>",
			html.EscapeString(statusReason),
		)
	}
	body := fmt.Sprintf(
		"<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><meta name=\"robots\" content=\"noindex,nofollow\"><title>ReReply data deletion status</title></head><body><main><h1>Data deletion request</h1><p>Confirmation code: <strong>%s</strong></p><p>Status: <strong>%s</strong></p>%s<p>Received: %s</p><p>Target completion date: %s</p></main></body></html>",
		html.EscapeString(privacyRequest.RequestNumber),
		html.EscapeString(displayStatus),
		reasonHTML,
		privacyRequest.ReceivedAt.UTC().Format("2 January 2006"),
		privacyRequest.DueAt.UTC().Format("2 January 2006"),
	)
	r.RequestCtx.Response.Header.SetContentType("text/html; charset=utf-8")
	r.RequestCtx.Response.Header.Set(
		"Content-Security-Policy",
		"default-src 'none'; base-uri 'none'; frame-ancestors 'none'",
	)
	r.RequestCtx.Response.SetStatusCode(fasthttp.StatusOK)
	r.RequestCtx.Response.SetBodyString(body)
	return nil
}

func parseThreadsLifecycleRequest(r *fastglue.Request) (uuid.UUID, string, error) {
	orgID, err := parseThreadsLifecycleOrganization(r)
	if err != nil {
		return uuid.Nil, "", err
	}
	body := r.RequestCtx.PostBody()
	if len(body) == 0 || len(body) > threadsLifecycleMaxRequestBytes {
		return uuid.Nil, "", errThreadsLifecycleMalformed
	}
	contentType := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.Request.Header.ContentType())))
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = strings.TrimSpace(contentType[:separator])
	}
	if contentType != "application/x-www-form-urlencoded" {
		return uuid.Nil, "", errThreadsLifecycleMalformed
	}
	signedRequest := strings.TrimSpace(string(r.RequestCtx.PostArgs().Peek("signed_request")))
	if signedRequest == "" || len(signedRequest) > threadsLifecycleMaxRequestBytes {
		return uuid.Nil, "", errThreadsLifecycleMalformed
	}
	return orgID, signedRequest, nil
}

func parseThreadsLifecycleOrganization(r *fastglue.Request) (uuid.UUID, error) {
	orgID, err := targetOrganizationID(r)
	if err != nil {
		return uuid.Nil, errThreadsLifecycleMalformed
	}
	return orgID, nil
}

func (a *App) lockThreadsLifecycleConfig(
	tx *gorm.DB,
	orgID uuid.UUID,
) (threadsLifecycleConfig, error) {
	if !a.hasIntegrationEncryptionKey() {
		return threadsLifecycleConfig{}, errThreadsLifecycleNotConfigured
	}
	var row models.ProviderIntegration
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND provider = ?",
		orgID,
		integrationProviderThreads,
	).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return threadsLifecycleConfig{}, errThreadsLifecycleNotConfigured
		}
		return threadsLifecycleConfig{}, err
	}
	appID := stringJSONValue(row.Config, "app_id")
	redirectURI := stringJSONValue(row.Config, "redirect_uri")
	encryptedSecret, _ := row.CredentialData["app_secret"].(string)
	if appID == "" || row.ThreadsAppID == nil || strings.TrimSpace(*row.ThreadsAppID) != appID ||
		redirectURI == "" || validateIntegrationRedirectURI(redirectURI) != nil ||
		!appcrypto.IsEncrypted(strings.TrimSpace(encryptedSecret)) {
		return threadsLifecycleConfig{}, errThreadsLifecycleNotConfigured
	}
	appSecret, err := appcrypto.Decrypt(encryptedSecret, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(appSecret) == "" {
		return threadsLifecycleConfig{}, errThreadsLifecycleNotConfigured
	}
	return threadsLifecycleConfig{
		IntegrationID: row.ID,
		AppID:         appID,
		RedirectURI:   redirectURI,
		AppSecret:     strings.TrimSpace(appSecret),
		Config:        cloneJSONB(row.Config),
	}, nil
}

func verifyThreadsSignedRequest(
	signedRequest, appSecret string,
	now time.Time,
) (verifiedThreadsSignedRequest, error) {
	parts := strings.Split(signedRequest, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.TrimSpace(appSecret) == "" {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleMalformed
	}
	signature, err := decodeThreadsSignedRequestSegment(parts[0])
	if err != nil || len(signature) != sha256.Size {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleMalformed
	}
	payloadJSON, err := decodeThreadsSignedRequestSegment(parts[1])
	if err != nil || len(payloadJSON) == 0 || len(payloadJSON) > threadsLifecycleMaxRequestBytes {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleMalformed
	}
	expectedMAC := hmac.New(sha256.New, []byte(appSecret))
	_, _ = expectedMAC.Write([]byte(parts[1]))
	if !hmac.Equal(signature, expectedMAC.Sum(nil)) {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleUnauthorized
	}

	var payload threadsSignedRequestPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleMalformed
	}
	if !strings.EqualFold(strings.TrimSpace(payload.Algorithm), "HMAC-SHA256") {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleUnauthorized
	}
	userID := strings.TrimSpace(string(payload.UserID))
	if !validThreadsOAuthID(userID) {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleUnauthorized
	}
	issuedAt, err := parseThreadsSignedRequestTimestamp(payload.IssuedAt)
	if err != nil || issuedAt <= 0 || time.Unix(issuedAt, 0).After(now.Add(threadsLifecycleClockSkew)) {
		return verifiedThreadsSignedRequest{}, errThreadsLifecycleUnauthorized
	}
	if strings.TrimSpace(payload.Expires.String()) != "" {
		expires, err := parseThreadsSignedRequestTimestamp(payload.Expires)
		if err != nil || expires < 0 {
			return verifiedThreadsSignedRequest{}, errThreadsLifecycleUnauthorized
		}
	}
	payloadHash := sha256.Sum256([]byte(parts[1]))
	return verifiedThreadsSignedRequest{
		UserID:   userID,
		IssuedAt: issuedAt,
		Hash:     hex.EncodeToString(payloadHash[:]),
	}, nil
}

func parseThreadsSignedRequestTimestamp(value json.Number) (int64, error) {
	text := strings.TrimSpace(value.String())
	if text == "" {
		return 0, errThreadsLifecycleMalformed
	}
	return value.Int64()
}

func decodeThreadsSignedRequestSegment(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

func lockMatchingThreadsLifecycleAccounts(
	tx *gorm.DB,
	orgID uuid.UUID,
	verified verifiedThreadsSignedRequest,
) ([]models.ChannelAccount, error) {
	var accounts []models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel = ? AND provider = ? AND external_account_id = ?",
		orgID,
		models.ChannelThreads,
		channelapi.ThreadsProvider,
		verified.UserID,
	).Find(&accounts).Error; err != nil {
		return nil, err
	}
	if len(accounts) > 1 {
		return nil, errThreadsLifecycleUnauthorized
	}
	if len(accounts) == 1 && accounts[0].ConnectedAt != nil {
		issuedAt := time.Unix(verified.IssuedAt, 0).UTC()
		// Meta's issued_at has whole-second precision, while ConnectedAt is
		// stored with sub-second precision. Treat the callback as belonging to
		// an older authorization only when the current connection is outside
		// that entire one-second timestamp bucket. The callback is still
		// acknowledged and any deletion request is still recorded.
		if !accounts[0].ConnectedAt.UTC().Before(
			issuedAt.Add(threadsLifecycleTimestampBucket),
		) {
			return []models.ChannelAccount{}, nil
		}
	}
	return accounts, nil
}

func markThreadsLifecycleDisconnected(
	tx *gorm.DB,
	orgID uuid.UUID,
	config threadsLifecycleConfig,
	accounts []models.ChannelAccount,
	now time.Time,
	errorCode, message string,
	eventHash string,
) error {
	updatedConfig := appendThreadsLifecycleEventHash(config.Config, eventHash)
	updates := map[string]any{
		// UpdatedAt is part of the OAuth fingerprint, so this always fences
		// an authorization flow that began before the lifecycle callback.
		"updated_at": now,
		"config":     updatedConfig,
	}
	if len(accounts) > 0 {
		updates["last_successful_at"] = nil
		updates["last_error_code"] = errorCode
		updates["last_error_message"] = message
		updates["validation_token"] = ""
	}
	if err := tx.Model(&models.ProviderIntegration{}).Where(
		"id = ? AND organization_id = ? AND provider = ?",
		config.IntegrationID,
		orgID,
		integrationProviderThreads,
	).Updates(updates).Error; err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}
	if err := disconnectLockedThreadsChannelAccounts(
		tx,
		orgID,
		nil,
		now,
		accounts,
		[]models.ChannelCredentialKind{models.ChannelCredentialKindOAuth},
	); err != nil {
		return err
	}
	accountIDs := make([]uuid.UUID, 0, len(accounts))
	for index := range accounts {
		account := accounts[index]
		accountIDs = append(accountIDs, account.ID)
		if err := tx.Model(&models.ChannelAccount{}).Where(
			"id = ? AND organization_id = ?",
			account.ID,
			orgID,
		).Updates(map[string]any{
			"name": fmt.Sprintf(
				"Disconnected Threads account %s",
				strings.ToUpper(strings.ReplaceAll(account.ID.String(), "-", "")[:8]),
			),
			"config": models.JSONB{
				"outbound_enabled": false,
			},
			"metadata": models.JSONB{
				"app_id":          config.AppID,
				"lifecycle_state": errorCode,
				"lifecycle_at":    now.Format(time.RFC3339),
			},
			"last_error":    message,
			"last_error_at": now,
			"updated_by_id": nil,
			"updated_at":    now,
		}).Error; err != nil {
			return err
		}
	}
	return tx.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id IN ? AND kind = ?",
		orgID,
		accountIDs,
		models.ChannelCredentialKindOAuth,
	).Updates(map[string]any{
		"credential_blob": models.JSONB{},
		"metadata": models.JSONB{
			"lifecycle_state": errorCode,
			"lifecycle_at":    now.Format(time.RFC3339),
		},
		"status":           models.ChannelCredentialStatusRevoked,
		"revoked_at":       now,
		"rotated_at":       now,
		"validation_error": message,
		"updated_at":       now,
	}).Error
}

func findOrCreateThreadsDeletionRequest(
	tx *gorm.DB,
	orgID uuid.UUID,
	appID string,
	verified verifiedThreadsSignedRequest,
	now time.Time,
) (models.PrivacyRequest, bool, error) {
	eventHash := threadsLifecycleEventHash(
		"data_deletion",
		appID,
		verified.UserID,
		verified.Hash,
	)
	var request models.PrivacyRequest
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND type = ? AND received_channel = ? AND verification_method = ? AND verification_token_hash = ?",
		orgID,
		models.PrivacyRequestTypeDeletion,
		threadsLifecycleReceivedChannel,
		threadsLifecycleVerification,
		eventHash,
	).First(&request).Error
	if err == nil {
		return request, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.PrivacyRequest{}, false, err
	}

	requestID := uuid.New()
	confirmationCode := "THDEL" + strings.ToUpper(strings.ReplaceAll(requestID.String(), "-", ""))
	request = models.PrivacyRequest{
		BaseModel:             models.BaseModel{ID: requestID},
		OrganizationID:        orgID,
		RequestNumber:         confirmationCode,
		Type:                  models.PrivacyRequestTypeDeletion,
		Status:                models.PrivacyRequestStatusInProgress,
		SubjectType:           threadsLifecycleSubjectType,
		SubjectKey:            verified.UserID,
		ReceivedChannel:       threadsLifecycleReceivedChannel,
		RequesterProfile:      models.JSONB{},
		RequestDetails:        models.JSONB{"source": "meta_threads"},
		VerificationMethod:    threadsLifecycleVerification,
		VerificationTokenHash: eventHash,
		ReceivedAt:            now,
		DueAt:                 now.Add(30 * 24 * time.Hour),
		VerifiedAt:            &now,
	}
	if err := tx.Create(&request).Error; err != nil {
		return models.PrivacyRequest{}, false, err
	}
	event := models.PrivacyRequestEvent{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		PrivacyRequestID: request.ID,
		EventType:        "request_created",
		ToStatus:         request.Status,
		Message:          "Meta Threads data deletion request verified and initiated",
		Details:          models.JSONB{"verification": threadsLifecycleVerification},
		OccurredAt:       now,
	}
	if err := tx.Create(&event).Error; err != nil {
		return models.PrivacyRequest{}, false, err
	}
	return request, true, nil
}

func threadsLifecycleEventHash(kind, appID, userID, requestHash string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s",
		kind,
		appID,
		userID,
		requestHash,
	)))
	return hex.EncodeToString(digest[:])
}

func threadsLifecycleEventSeen(config models.JSONB, eventHash string) bool {
	if config == nil || eventHash == "" {
		return false
	}
	switch values := config[threadsLifecycleEventHashesKey].(type) {
	case []string:
		for _, value := range values {
			if value == eventHash {
				return true
			}
		}
	case []any:
		for _, value := range values {
			text, _ := value.(string)
			if text == eventHash {
				return true
			}
		}
	}
	return false
}

func appendThreadsLifecycleEventHash(config models.JSONB, eventHash string) models.JSONB {
	updated := cloneJSONB(config)
	values := make([]string, 0, threadsLifecycleEventHashLimit)
	switch stored := updated[threadsLifecycleEventHashesKey].(type) {
	case []string:
		values = append(values, stored...)
	case []any:
		for _, value := range stored {
			if text, ok := value.(string); ok && text != "" {
				values = append(values, text)
			}
		}
	}
	for _, value := range values {
		if value == eventHash {
			updated[threadsLifecycleEventHashesKey] = values
			return updated
		}
	}
	values = append(values, eventHash)
	if len(values) > threadsLifecycleEventHashLimit {
		values = values[len(values)-threadsLifecycleEventHashLimit:]
	}
	updated[threadsLifecycleEventHashesKey] = values
	return updated
}

func (a *App) threadsDeletionStatusURL(
	redirectURI string,
	orgID uuid.UUID,
	confirmationCode string,
) (string, error) {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		!validThreadsDeletionConfirmationCode(confirmationCode) {
		return "", errThreadsLifecycleNotConfigured
	}
	basePath := ""
	redirectPath := strings.TrimRight(parsed.Path, "/")
	const oauthCallbackPath = "/api/integrations/threads/callback"
	if strings.HasSuffix(redirectPath, oauthCallbackPath) {
		basePath = strings.TrimSuffix(redirectPath, oauthCallbackPath)
	} else if a != nil && a.Config != nil {
		basePath = strings.TrimRight(
			sanitizeRedirectPath(strings.TrimSpace(a.Config.Server.BasePath)),
			"/",
		)
	}
	parsed.Path = fmt.Sprintf(
		"%s/api/integrations/threads/%s/data-deletion/status/%s",
		basePath,
		orgID.String(),
		confirmationCode,
	)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

func validThreadsDeletionConfirmationCode(value string) bool {
	if len(value) != len("THDEL")+32 || !strings.HasPrefix(value, "THDEL") {
		return false
	}
	_, err := hex.DecodeString(value[len("THDEL"):])
	return err == nil
}

func threadsDeletionDisplayStatus(
	status models.PrivacyRequestStatus,
	decisionReason string,
) (string, string) {
	reason := strings.TrimSpace(decisionReason)
	switch status {
	case models.PrivacyRequestStatusCompleted:
		return "Completed", ""
	case models.PrivacyRequestStatusDenied:
		if reason == "" {
			reason = "The request could not be fulfilled. Contact ReReply support with the confirmation code for the recorded justification."
		}
		return "Denied", reason
	case models.PrivacyRequestStatusCanceled:
		if reason == "" {
			reason = "The request was canceled. Contact ReReply support with the confirmation code for details."
		}
		return "Canceled", reason
	case models.PrivacyRequestStatusExpired:
		if reason == "" {
			reason = "The request expired before completion. Contact ReReply support with the confirmation code to reopen it."
		}
		return "Expired", reason
	default:
		return "Processing", ""
	}
}

func (a *App) sendThreadsLifecycleHandlerError(
	r *fastglue.Request,
	orgID uuid.UUID,
	action string,
	err error,
) error {
	switch {
	case errors.Is(err, errThreadsLifecycleMalformed):
		return sendThreadsLifecycleError(r, fasthttp.StatusBadRequest)
	case errors.Is(err, errThreadsLifecycleUnauthorized):
		return sendThreadsLifecycleError(r, fasthttp.StatusUnauthorized)
	case errors.Is(err, errThreadsLifecycleNotConfigured), errors.Is(err, gorm.ErrRecordNotFound):
		return sendThreadsLifecycleError(r, fasthttp.StatusNotFound)
	default:
		a.Log.Error(
			"Threads lifecycle callback failed",
			"error", err,
			"organization_id", orgID,
			"action", action,
		)
		return sendThreadsLifecycleError(r, fasthttp.StatusServiceUnavailable)
	}
}

func setThreadsLifecycleResponseHeaders(r *fastglue.Request) {
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set("Pragma", "no-cache")
	r.RequestCtx.Response.Header.Set("Referrer-Policy", "no-referrer")
	r.RequestCtx.Response.Header.Set("X-Content-Type-Options", "nosniff")
	r.RequestCtx.Response.Header.Set("X-Robots-Tag", "noindex, nofollow")
}

func sendThreadsLifecycleError(r *fastglue.Request, status int) error {
	return sendThreadsLifecycleJSON(r, status, map[string]string{"error": "invalid_request"})
}

func sendThreadsLifecycleJSON(r *fastglue.Request, status int, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.RequestCtx.Response.Header.SetContentType("application/json; charset=utf-8")
	r.RequestCtx.Response.SetStatusCode(status)
	r.RequestCtx.Response.SetBody(body)
	return nil
}

func sendThreadsLifecycleText(r *fastglue.Request, status int, body string) error {
	r.RequestCtx.Response.Header.SetContentType("text/plain; charset=utf-8")
	r.RequestCtx.Response.SetStatusCode(status)
	r.RequestCtx.Response.SetBodyString(body)
	return nil
}
