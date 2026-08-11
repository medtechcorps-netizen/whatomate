package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaMessengerReviewReplyMode           = models.StagingMessengerReviewManualReplyMode
	metaMessengerReviewReplyMaxRunes       = 2000
	metaMessengerReviewReplyGraphTimeout   = 30 * time.Second
	metaMessengerReviewReplyOperationLease = 45 * time.Second
	metaMessengerReviewReplyAttestationTTL = 2 * time.Minute
	metaMessengerReviewReplyBodyLimit      = 16 << 10
	metaMessengerReviewProviderMIDMaxBytes = 255

	metaMessengerReviewReplyOperationIDKey                = "review_manual_send_operation_id"
	metaMessengerReviewReplyOperationStateKey             = "review_manual_send_operation_state"
	metaMessengerReviewReplyOperationExpiryKey            = "review_manual_send_provider_deadline_at"
	metaMessengerReviewReplyOperationGenerationKey        = "review_manual_send_generation"
	metaMessengerReviewReplyOperationCredentialIDKey      = "review_manual_send_oauth_credential_id"
	metaMessengerReviewReplyOperationCredentialVersionKey = "review_manual_send_oauth_credential_version"
	metaMessengerReviewReplyOperationConversationKey      = "review_manual_send_conversation_id"
	metaMessengerReviewReplyOperationUserKey              = "review_manual_send_user_id"
	metaMessengerReviewReplyLastOperationIDKey            = "review_manual_send_last_operation_id"
	metaMessengerReviewReplyLastOperationStateKey         = "review_manual_send_last_operation_state"
	metaMessengerReviewReplyLastSettledAtKey              = "review_manual_send_last_settled_at"

	metaMessengerReviewReplyStateDispatching = "dispatching"
	metaMessengerReviewReplyStateSent        = "sent"
	metaMessengerReviewReplyStateRejected    = "rejected"
	metaMessengerReviewReplyStateAmbiguous   = "ambiguous"
)

var (
	errMetaMessengerReviewReplyUnavailable  = errors.New("staging Messenger review reply is unavailable")
	errMetaMessengerReviewReplyIneligible   = errors.New("staging Messenger review reply is not eligible")
	errMetaMessengerReviewReplyInFlight     = errors.New("staging Messenger review reply is already in flight")
	errMetaMessengerReviewReplyCollision    = errors.New("staging Messenger review reply idempotency collision")
	errMetaMessengerReviewReplyTerminal     = errors.New("staging Messenger review reply is terminal")
	errMetaMessengerReviewReplyDrainPending = errors.New("staging Messenger review reply is still draining")
)

type metaMessengerReviewReplyConstraints struct {
	TextOnly                   bool `json:"text_only"`
	MaxLength                  int  `json:"max_length"`
	ManualConfirmationRequired bool `json:"manual_confirmation_required"`
	AIDisabled                 bool `json:"ai_disabled"`
	MarkReadDisabled           bool `json:"mark_read_disabled"`
}

type metaMessengerReviewReplyEligibilityResponse struct {
	Eligible       bool                                `json:"eligible"`
	ReasonCode     string                              `json:"reason_code"`
	Reason         string                              `json:"reason,omitempty"`
	AttestationID  string                              `json:"attestation_id,omitempty"`
	ExpiresAt      string                              `json:"expires_at,omitempty"`
	PageID         string                              `json:"page_id,omitempty"`
	RecipientLabel string                              `json:"recipient_label,omitempty"`
	Constraints    metaMessengerReviewReplyConstraints `json:"constraints"`
}

type metaMessengerReviewReplyRequest struct {
	AttestationID      string `json:"attestation_id"`
	IdempotencyKey     string `json:"idempotency_key"`
	Text               string `json:"text"`
	ManualConfirmation bool   `json:"manual_confirmation"`
}

type metaMessengerReviewReplyAuditReceipt struct {
	ID             uuid.UUID `json:"id"`
	SentAt         time.Time `json:"sent_at"`
	PageID         string    `json:"page_id"`
	RecipientLabel string    `json:"recipient_label"`
}

type metaMessengerReviewReplyResponse struct {
	Message    models.Message                       `json:"message"`
	Parts      []models.MessagePart                 `json:"parts"`
	Audit      metaMessengerReviewReplyAuditReceipt `json:"audit"`
	Idempotent bool                                 `json:"idempotent"`
}

type metaMessengerReviewReplyBinding struct {
	Tuple                  metareview.ProvisionTuple
	OrganizationID         uuid.UUID
	UserID                 uuid.UUID
	Account                models.ChannelAccount
	Conversation           models.InboxConversation
	Identity               models.ContactIdentity
	LatestInbound          models.Message
	RecipientID            string
	ServiceWindowEndsAt    time.Time
	AttestationExpiresAt   time.Time
	SessionDigest          [sha256.Size]byte
	AttestationID          string
	EncryptedAccessToken   string
	EncryptedTokenDigest   [sha256.Size]byte
	OAuthCredentialID      uuid.UUID
	OAuthCredentialVersion int
}

type metaMessengerReviewReplyAttempt struct {
	Binding      metaMessengerReviewReplyBinding
	Message      models.Message
	Parts        []models.MessagePart
	Outbox       models.OutboxJob
	Idempotent   bool
	TerminalCode string
}

type metaMessengerReviewGraphReplyResult struct {
	Sent        bool
	Ambiguous   bool
	StatusCode  int
	ErrorCode   string
	RecipientID string
	MessageID   string
}

type metaMessengerReviewGraphReplyResponse struct {
	RecipientID string `json:"recipient_id"`
	MessageID   string `json:"message_id"`
}

func metaMessengerReviewReplyConstraintsValue() metaMessengerReviewReplyConstraints {
	return metaMessengerReviewReplyConstraints{
		TextOnly:                   true,
		MaxLength:                  metaMessengerReviewReplyMaxRunes,
		ManualConfirmationRequired: true,
		AIDisabled:                 true,
		MarkReadDisabled:           true,
	}
}

func requireMetaMessengerReviewHumanSession(r *fastglue.Request, mutating bool) error {
	if r == nil || r.RequestCtx == nil ||
		len(r.RequestCtx.Request.Header.Peek("Authorization")) != 0 ||
		len(r.RequestCtx.Request.Header.Peek("X-API-Key")) != 0 ||
		len(r.RequestCtx.Request.Header.Cookie("whm_access")) == 0 {
		return errors.New("cookie-authenticated reviewer session is required")
	}
	if !mutating {
		return nil
	}
	csrfCookie := string(r.RequestCtx.Request.Header.Cookie("whm_csrf"))
	csrfHeader := string(r.RequestCtx.Request.Header.Peek("X-CSRF-Token"))
	if csrfCookie == "" || csrfHeader == "" ||
		len(csrfCookie) != len(csrfHeader) ||
		subtle.ConstantTimeCompare([]byte(csrfCookie), []byte(csrfHeader)) != 1 {
		return errors.New("reviewer CSRF confirmation is required")
	}
	return nil
}

func metaMessengerReviewSessionDigest(r *fastglue.Request) [sha256.Size]byte {
	if r == nil || r.RequestCtx == nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(r.RequestCtx.Request.Header.Cookie("whm_access"))
}

func (a *App) requirePinnedMetaMessengerReviewerTx(
	tx *gorm.DB,
	organizationID, userID uuid.UUID,
) error {
	if a == nil || a.Config == nil || tx == nil ||
		a.Config.App.Environment != "staging" ||
		!a.Config.MetaMessengerReviewRelay.Enabled ||
		!a.Config.MetaMessengerReviewRelay.ReviewerOutboundEnabled ||
		a.Config.MetaMessengerReviewRelay.Mode != metareview.Mode {
		return errMetaMessengerReviewReplyUnavailable
	}
	configuredUserID, err := uuid.Parse(a.Config.MetaMessengerReviewRelay.ReviewerUserID)
	if err != nil || configuredUserID == uuid.Nil || configuredUserID.String() !=
		a.Config.MetaMessengerReviewRelay.ReviewerUserID || configuredUserID != userID {
		return errMetaMessengerReviewReplyUnavailable
	}
	configuredRoleID, err := uuid.Parse(a.Config.MetaMessengerReviewRelay.ReviewerRoleID)
	if err != nil || configuredRoleID == uuid.Nil || configuredRoleID.String() !=
		a.Config.MetaMessengerReviewRelay.ReviewerRoleID {
		return errMetaMessengerReviewReplyUnavailable
	}
	var user models.User
	if err := tx.Where("id = ? AND is_active = ?", userID, true).First(&user).Error; err != nil {
		return errMetaMessengerReviewReplyUnavailable
	}
	if user.IsSuperAdmin {
		return errMetaMessengerReviewReplyUnavailable
	}
	var membership models.UserOrganization
	if err := tx.Where(
		"user_id = ? AND organization_id = ? AND role_id = ?",
		userID,
		organizationID,
		configuredRoleID,
	).First(&membership).Error; err != nil {
		return errMetaMessengerReviewReplyUnavailable
	}
	var role models.CustomRole
	if err := tx.Preload("Permissions").Where(
		"id = ? AND organization_id = ?",
		configuredRoleID,
		organizationID,
	).First(&role).Error; err != nil {
		return errMetaMessengerReviewReplyUnavailable
	}
	if role.IsSystem || role.IsDefault || len(role.Permissions) != 3 {
		return errMetaMessengerReviewReplyUnavailable
	}
	requiredReads := map[string]bool{
		models.ResourceChannelAccounts: false,
		models.ResourceContacts:        false,
		models.ResourceConversations:   false,
	}
	for _, permission := range role.Permissions {
		if permission.Action != models.ActionRead {
			return errMetaMessengerReviewReplyUnavailable
		}
		if _, permitted := requiredReads[permission.Resource]; !permitted {
			return errMetaMessengerReviewReplyUnavailable
		}
		requiredReads[permission.Resource] = true
	}
	for _, present := range requiredReads {
		if !present {
			return errMetaMessengerReviewReplyUnavailable
		}
	}
	return nil
}

func maskedMetaMessengerReviewRecipient(recipientID string) string {
	recipientID = strings.TrimSpace(recipientID)
	if len(recipientID) <= 4 {
		return strings.Repeat("•", len(recipientID))
	}
	return "••••" + recipientID[len(recipientID)-4:]
}

func metaMessengerReviewRecipientDigest(recipientID string) string {
	digest := sha256.Sum256([]byte(
		"rereply-meta-review-recipient-audit:v1\n" + strings.TrimSpace(recipientID),
	))
	return hex.EncodeToString(digest[:])
}

func metaMessengerReviewCapabilityDigest(value string) string {
	digest := sha256.Sum256([]byte(
		"rereply-meta-review-capability-audit:v1\n" + strings.TrimSpace(value),
	))
	return hex.EncodeToString(digest[:])
}

// GetMetaMessengerReviewReply reports server-derived eligibility for the exact
// deployment-pinned staging review conversation. The returned attestation is
// only a UI confirmation aid; POST re-evaluates every authorization and policy
// fact under database locks.
func (a *App) GetMetaMessengerReviewReply(r *fastglue.Request) error {
	setMetaMessengerNoStoreHeaders(r)
	if err := requireMetaMessengerReviewHumanSession(r, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "A signed-in reviewer browser session is required", nil, "")
	}
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}

	response := metaMessengerReviewReplyEligibilityResponse{
		Eligible:    false,
		ReasonCode:  "unavailable",
		Constraints: metaMessengerReviewReplyConstraintsValue(),
	}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Messenger review reply not found", nil, "")
	}
	var authErr error
	err = database.WithTenantReadCommitted(root.DB.WithContext(requestContext(r)), orgID, func(tx *gorm.DB) error {
		scoped := root.scopedApp(tx, orgID)
		authorizedOrgID, authorizedUserID, permissionErr := scoped.requireAuth(
			r,
			models.ResourceConversations,
			models.ActionRead,
		)
		authErr = permissionErr
		if authErr != nil {
			return nil
		}
		if authorizedOrgID != orgID || authorizedUserID != userID {
			return errMetaMessengerReviewReplyUnavailable
		}
		if reviewerErr := root.requirePinnedMetaMessengerReviewerTx(tx, orgID, userID); reviewerErr != nil {
			return reviewerErr
		}
		binding, reasonCode, reason, loadErr := root.loadMetaMessengerReviewReplyBindingTx(
			tx,
			orgID,
			userID,
			conversationID,
			time.Now().UTC(),
			false,
		)
		if loadErr != nil {
			return loadErr
		}
		binding.SessionDigest = metaMessengerReviewSessionDigest(r)
		response.PageID = binding.Tuple.PageID
		response.RecipientLabel = maskedMetaMessengerReviewRecipient(binding.RecipientID)
		response.ReasonCode = reasonCode
		response.Reason = reason
		if reasonCode == "eligible" {
			attestation, expiresAt, attestationErr := root.newMetaMessengerReviewReplyAttestation(
				binding,
				time.Now().UTC(),
			)
			if attestationErr != nil {
				return attestationErr
			}
			response.Eligible = true
			response.AttestationID = attestation
			response.ExpiresAt = expiresAt.Format(time.RFC3339Nano)
			response.Reason = ""
		}
		return nil
	})
	if authErr != nil {
		return nil
	}
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) ||
			errors.Is(err, errMetaMessengerReviewReplyUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Messenger review reply not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger review reply eligibility is unavailable", nil, "")
	}
	return r.SendEnvelope(response)
}

// SendMetaMessengerReviewReply performs one explicitly confirmed, text-only
// customer-service RESPONSE. It does not use the generic relay adapter,
// outbound HMAC credentials, the ordinary outbox worker, AI, automation,
// media, default outgoing selection, or provider read receipts.
func (a *App) SendMetaMessengerReviewReply(r *fastglue.Request) error {
	setMetaMessengerNoStoreHeaders(r)
	if err := requireMetaMessengerReviewHumanSession(r, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "A signed-in reviewer browser session with CSRF confirmation is required", nil, "")
	}
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	conversationID, err := parsePathUUID(r, "id", "conversation")
	if err != nil {
		return nil
	}
	request, err := decodeMetaMessengerReviewReplyRequest(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	deliveryStartedAt := time.Now().UTC()
	deliveryBaseDeadline := deliveryStartedAt.Add(
		metaMessengerReviewReplyDeliveryBudget(a.HTTPClient),
	)
	deliveryBaseCtx, deliveryBaseCancel := context.WithDeadline(
		context.WithoutCancel(requestContext(r)),
		deliveryBaseDeadline,
	)
	defer deliveryBaseCancel()

	attempt, authErr, err := a.beginMetaMessengerReviewReply(
		deliveryBaseCtx,
		r,
		orgID,
		userID,
		conversationID,
		request,
	)
	if authErr != nil {
		return nil
	}
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound),
			errors.Is(err, errMetaMessengerReviewReplyUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Messenger review reply not found", nil, "")
		case errors.Is(err, errMetaMessengerReviewReplyIneligible):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "This Messenger conversation is no longer inside the verified 24-hour reply window", nil, "")
		case errors.Is(err, errMetaMessengerReviewReplyCollision):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Idempotency key was already used for a different review reply", nil, "")
		case errors.Is(err, errMetaMessengerReviewReplyInFlight):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "A Messenger review reply is already being delivered", nil, "")
		case errors.Is(err, errMetaMessengerReviewReplyTerminal):
			if attempt.TerminalCode == "delivery_outcome_ambiguous" {
				return r.SendErrorEnvelope(fasthttp.StatusConflict, "The earlier delivery outcome is ambiguous; do not retry with a new key", nil, "")
			}
			return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "The earlier review reply was not accepted by Meta", nil, "")
		default:
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Messenger review reply could not be prepared", nil, "")
		}
	}
	if attempt.Idempotent {
		return r.SendEnvelope(metaMessengerReviewReplyResponse{
			Message: sanitizedMetaMessengerReviewReplyMessage(attempt.Message),
			Parts:   attempt.Parts,
			Audit: metaMessengerReviewReplyAuditReceipt{
				ID:             attempt.Outbox.ID,
				SentAt:         valueOrZeroTime(attempt.Outbox.SentAt),
				PageID:         attempt.Binding.Tuple.PageID,
				RecipientLabel: maskedMetaMessengerReviewRecipient(attempt.Binding.RecipientID),
			},
			Idempotent: true,
		})
	}

	deliveryDeadline, deadlineErr := metaMessengerReviewReplyDeliveryDeadline(
		attempt,
		deliveryBaseDeadline,
	)
	if deadlineErr != nil {
		_, _ = a.settleMetaMessengerReviewReply(
			orgID,
			userID,
			attempt,
			metaMessengerReviewGraphReplyResult{ErrorCode: "preflight_deadline_invalid"},
		)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The verified review reply delivery deadline is unavailable; no provider call was made", nil, "")
	}
	deliveryCtx, deliveryCancel := context.WithDeadline(deliveryBaseCtx, deliveryDeadline)
	defer deliveryCancel()
	if metaMessengerReviewReplyDeadlineElapsed(deliveryCtx, deliveryDeadline) {
		_, _ = a.settleMetaMessengerReviewReply(
			orgID,
			userID,
			attempt,
			metaMessengerReviewGraphReplyResult{ErrorCode: "preflight_deadline_expired"},
		)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The verified review reply delivery deadline expired; no provider call was made", nil, "")
	}

	refreshedBinding, revalidationErr := a.revalidateMetaMessengerReviewReplyBeforeGraph(
		deliveryCtx,
		orgID,
		userID,
		attempt,
	)
	if revalidationErr != nil {
		_, _ = a.settleMetaMessengerReviewReply(
			orgID,
			userID,
			attempt,
			metaMessengerReviewGraphReplyResult{ErrorCode: "preflight_revalidation_failed"},
		)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The verified review reply binding changed before delivery; no provider call was made", nil, "")
	}
	attempt.Binding = refreshedBinding
	if metaMessengerReviewReplyDeadlineElapsed(deliveryCtx, deliveryDeadline) {
		_, _ = a.settleMetaMessengerReviewReply(
			orgID,
			userID,
			attempt,
			metaMessengerReviewGraphReplyResult{ErrorCode: "preflight_deadline_expired"},
		)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The verified review reply delivery deadline expired; no provider call was made", nil, "")
	}
	pageToken, decryptErr := appcrypto.Decrypt(
		attempt.Binding.EncryptedAccessToken,
		a.integrationEncryptionKey(),
	)
	var result metaMessengerReviewGraphReplyResult
	if decryptErr != nil || strings.TrimSpace(pageToken) == "" ||
		strings.TrimSpace(pageToken) != pageToken {
		result = metaMessengerReviewGraphReplyResult{
			StatusCode: http.StatusUnauthorized,
			ErrorCode:  "credential_unavailable",
		}
	} else if metaMessengerReviewReplyDeadlineElapsed(deliveryCtx, deliveryDeadline) {
		result = metaMessengerReviewGraphReplyResult{
			ErrorCode: "preflight_deadline_expired",
		}
	} else {
		result = a.sendMetaMessengerReviewGraphReply(
			deliveryCtx,
			attempt.Binding.Tuple.PageID,
			attempt.Binding.RecipientID,
			request.Text,
			pageToken,
		)
	}

	settled, settleErr := a.settleMetaMessengerReviewReply(
		orgID,
		userID,
		attempt,
		result,
	)
	if settleErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The delivery outcome could not be safely settled; do not retry with a new key", nil, "")
	}
	if result.Ambiguous {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The delivery outcome is ambiguous; do not retry with a new key", nil, "")
	}
	if !result.Sent {
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta did not accept the review reply", nil, "")
	}
	return r.SendEnvelope(metaMessengerReviewReplyResponse{
		Message: sanitizedMetaMessengerReviewReplyMessage(settled.Message),
		Parts:   settled.Parts,
		Audit: metaMessengerReviewReplyAuditReceipt{
			ID:             settled.Outbox.ID,
			SentAt:         valueOrZeroTime(settled.Outbox.SentAt),
			PageID:         settled.Binding.Tuple.PageID,
			RecipientLabel: maskedMetaMessengerReviewRecipient(settled.Binding.RecipientID),
		},
		Idempotent: false,
	})
}

func metaMessengerReviewReplyDeliveryBudget(client *http.Client) time.Duration {
	budget := metaMessengerReviewReplyGraphTimeout
	if client != nil && client.Timeout > 0 && client.Timeout < budget {
		budget = client.Timeout
	}
	return budget
}

func metaMessengerReviewReplyDeliveryDeadline(
	attempt metaMessengerReviewReplyAttempt,
	baseDeadline time.Time,
) (time.Time, error) {
	if baseDeadline.IsZero() || attempt.Binding.AttestationExpiresAt.IsZero() ||
		attempt.Binding.ServiceWindowEndsAt.IsZero() {
		return time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	authorityExpiry, err := time.Parse(time.RFC3339Nano, attempt.Binding.Tuple.ExpiresAt)
	if err != nil || authorityExpiry.IsZero() {
		return time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	providerDeadline, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(attempt.Outbox.ProviderState, "provider_deadline_at"),
	)
	if err != nil || providerDeadline.IsZero() {
		return time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	deadline := baseDeadline.UTC()
	for _, candidate := range []time.Time{
		attempt.Binding.AttestationExpiresAt,
		attempt.Binding.ServiceWindowEndsAt,
		authorityExpiry,
		providerDeadline,
	} {
		candidate = candidate.UTC()
		if candidate.Before(deadline) {
			deadline = candidate
		}
	}
	return deadline, nil
}

func metaMessengerReviewReplyDeadlineElapsed(ctx context.Context, deadline time.Time) bool {
	return ctx == nil || ctx.Err() != nil || deadline.IsZero() ||
		!time.Now().UTC().Before(deadline.UTC())
}

func sanitizedMetaMessengerReviewReplyMessage(message models.Message) models.Message {
	// The legacy ConversationID carries the provider PSID for non-WhatsApp
	// channels. The reviewer response exposes only the internal conversation ID
	// plus the separately masked recipient label.
	message.ConversationID = ""
	message.Organization = nil
	message.Contact = nil
	message.ReplyToMessage = nil
	message.SentByUser = nil
	return message
}

func decodeMetaMessengerReviewReplyRequest(
	r *fastglue.Request,
) (metaMessengerReviewReplyRequest, error) {
	var request metaMessengerReviewReplyRequest
	if r == nil || r.RequestCtx == nil {
		return request, errors.New("invalid request body")
	}
	body := append([]byte(nil), r.RequestCtx.PostBody()...)
	if len(body) == 0 || len(body) > metaMessengerReviewReplyBodyLimit || !utf8.Valid(body) {
		return request, errors.New("invalid request body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, errors.New("invalid request body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return request, errors.New("invalid request body")
	}
	request.AttestationID = strings.TrimSpace(request.AttestationID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Text = strings.TrimSpace(request.Text)
	if !request.ManualConfirmation {
		return request, errors.New("manual confirmation is required")
	}
	if !validMetaMessengerReviewReplyAttestationText(request.AttestationID) {
		return request, errors.New("review reply attestation is invalid")
	}
	idempotencyID, err := uuid.Parse(request.IdempotencyKey)
	if err != nil || idempotencyID == uuid.Nil || idempotencyID.String() != request.IdempotencyKey {
		return request, errors.New("idempotency key must be a canonical UUID")
	}
	if request.Text == "" || !utf8.ValidString(request.Text) ||
		utf8.RuneCountInString(request.Text) > metaMessengerReviewReplyMaxRunes ||
		containsForbiddenReviewReplyControl(request.Text) {
		return request, fmt.Errorf("review reply text must contain 1-%d valid text characters", metaMessengerReviewReplyMaxRunes)
	}
	return request, nil
}

func containsForbiddenReviewReplyControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return true
		}
	}
	return false
}

func (a *App) beginMetaMessengerReviewReply(
	ctx context.Context,
	r *fastglue.Request,
	organizationID, userID, conversationID uuid.UUID,
	request metaMessengerReviewReplyRequest,
) (metaMessengerReviewReplyAttempt, error, error) {
	result := metaMessengerReviewReplyAttempt{}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return result, nil, errMetaMessengerReviewReplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var authErr error
	err := database.WithTenantReadCommitted(root.DB.WithContext(ctx), organizationID, func(tx *gorm.DB) error {
		scoped := root.scopedApp(tx, organizationID)
		authorizedOrgID, authorizedUserID, permissionErr := scoped.requireAuth(
			r,
			models.ResourceConversations,
			models.ActionRead,
		)
		authErr = permissionErr
		if authErr != nil {
			return nil
		}
		if authorizedOrgID != organizationID || authorizedUserID != userID {
			return errMetaMessengerReviewReplyUnavailable
		}
		if reviewerErr := root.requirePinnedMetaMessengerReviewerTx(
			tx,
			organizationID,
			userID,
		); reviewerErr != nil {
			return reviewerErr
		}
		now := time.Now().UTC()
		binding, reasonCode, _, loadErr := root.loadMetaMessengerReviewReplyBindingTx(
			tx,
			organizationID,
			userID,
			conversationID,
			now,
			true,
		)
		result.Binding = binding
		if loadErr != nil {
			return loadErr
		}
		binding.SessionDigest = metaMessengerReviewSessionDigest(r)
		attestationExpiry, attestationErr := root.verifyMetaMessengerReviewReplyAttestation(
			binding,
			request.AttestationID,
			now,
		)
		if reasonCode != "eligible" || attestationErr != nil {
			return errMetaMessengerReviewReplyIneligible
		}
		binding.AttestationID = request.AttestationID
		binding.AttestationExpiresAt = attestationExpiry
		result.Binding = binding

		active, expireErr := hasProtectedMetaMessengerReviewDispatchTx(
			tx,
			organizationID,
			binding.Account.ID,
			now,
		)
		if expireErr != nil {
			return expireErr
		}

		payloadDigest := metaMessengerReviewReplyDigest(binding, request.Text)
		idempotencyKey := metaMessengerReviewReplyIdempotencyKey(binding)
		var existing models.OutboxJob
		existingErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
			organizationID,
			binding.Account.ID,
			idempotencyKey,
		).First(&existing).Error
		if existingErr == nil {
			result.Outbox = existing
			if existing.ConversationID != conversationID ||
				existing.PayloadDigest != payloadDigest ||
				stringConfigValue(existing.ProviderState, "review_mode") != metaMessengerReviewReplyMode {
				return errMetaMessengerReviewReplyCollision
			}
			if existing.MessageID != nil {
				_ = tx.Where(
					"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
					*existing.MessageID,
					organizationID,
					conversationID,
				).First(&result.Message).Error
				_ = tx.Where(
					"organization_id = ? AND conversation_id = ? AND message_id = ?",
					organizationID,
					conversationID,
					*existing.MessageID,
				).Order("position").Find(&result.Parts).Error
			}
			switch existing.Status {
			case models.OutboxJobStatusSent:
				result.Idempotent = true
				return nil
			case models.OutboxJobStatusReviewDispatching:
				return errMetaMessengerReviewReplyInFlight
			default:
				result.TerminalCode = existing.LastErrorCode
				return errMetaMessengerReviewReplyTerminal
			}
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}
		if active {
			return errMetaMessengerReviewReplyInFlight
		}

		messageID := uuid.New()
		part := channelapi.MessagePart{Type: models.MessagePartTypeText, Text: request.Text}
		message := legacyMessageFromParts(
			organizationID,
			&binding.Conversation,
			messageID,
			models.DirectionOutgoing,
			[]channelapi.MessagePart{part},
			now,
		)
		message.SentByUserID = &userID
		message.Status = models.MessageStatusPending
		message.Metadata = models.JSONB{
			"channel":              models.ChannelMessenger,
			"provider":             channelapi.RelayProvider,
			"purpose":              models.ChannelPreferencePurposeService,
			"idempotency_key":      idempotencyKey,
			"client_request_nonce": request.IdempotencyKey,
			"payload_digest":       payloadDigest,
			"review_mode":          metaMessengerReviewReplyMode,
			"review_generation":    binding.Tuple.Generation,
			"manual_confirmation":  true,
		}
		parts := persistentMessageParts(
			organizationID,
			conversationID,
			messageID,
			[]channelapi.MessagePart{part},
		)
		operationExpiry := now.Add(metaMessengerReviewReplyOperationLease)
		workerFenceUntil, parseErr := time.Parse(time.RFC3339Nano, binding.Tuple.ExpiresAt)
		if parseErr != nil || !workerFenceUntil.After(operationExpiry) {
			return errMetaMessengerReviewReplyUnavailable
		}
		outbox := models.OutboxJob{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   organizationID,
			ChannelAccountID: binding.Account.ID,
			ConversationID:   conversationID,
			MessageID:        &messageID,
			IdempotencyKey:   idempotencyKey,
			PayloadDigest:    payloadDigest,
			Purpose:          models.ChannelPreferencePurposeService,
			Status:           models.OutboxJobStatusReviewDispatching,
			Priority:         binding.Conversation.Priority,
			AvailableAt:      now,
			// New workers exclude this marker permanently. Pinning LockedAt to
			// the deployment grant expiry also prevents an older rolling binary
			// from reclaiming it under the generic two-minute stale lease.
			LockedAt:      &workerFenceUntil,
			LockedBy:      "meta-review:" + userID.String(),
			AttemptCount:  1,
			MaxAttempts:   1,
			LastAttemptAt: &now,
			ProviderState: models.JSONB{
				"review_mode":              metaMessengerReviewReplyMode,
				"generation":               binding.Tuple.Generation,
				"oauth_credential_id":      binding.OAuthCredentialID.String(),
				"oauth_credential_version": strconv.Itoa(binding.OAuthCredentialVersion),
				"page_id":                  binding.Tuple.PageID,
				"recipient_label":          maskedMetaMessengerReviewRecipient(binding.RecipientID),
				"recipient_digest":         metaMessengerReviewRecipientDigest(binding.RecipientID),
				"provider_deadline_at":     operationExpiry.Format(time.RFC3339Nano),
				"attestation_digest":       metaMessengerReviewCapabilityDigest(request.AttestationID),
				"client_request_nonce":     request.IdempotencyKey,
				"sent_by_user_id":          userID.String(),
			},
			Payload: models.JSONB{
				"review_mode":         metaMessengerReviewReplyMode,
				"page_id":             binding.Tuple.PageID,
				"conversation_id":     binding.Conversation.ID.String(),
				"latest_inbound_id":   binding.LatestInbound.ID.String(),
				"manual_confirmation": true,
			},
		}
		if err := tx.Create(&message).Error; err != nil {
			return err
		}
		if err := tx.Create(&parts).Error; err != nil {
			return err
		}
		if err := tx.Create(&outbox).Error; err != nil {
			return err
		}

		metadata := cloneJSONB(binding.Account.Metadata)
		metadata[metaMessengerReviewReplyOperationIDKey] = outbox.ID.String()
		metadata[metaMessengerReviewReplyOperationStateKey] = metaMessengerReviewReplyStateDispatching
		metadata[metaMessengerReviewReplyOperationExpiryKey] = operationExpiry.Format(time.RFC3339Nano)
		metadata[metaMessengerReviewReplyOperationGenerationKey] = binding.Tuple.Generation
		metadata[metaMessengerReviewReplyOperationCredentialIDKey] = binding.OAuthCredentialID.String()
		metadata[metaMessengerReviewReplyOperationCredentialVersionKey] = strconv.Itoa(binding.OAuthCredentialVersion)
		metadata[metaMessengerReviewReplyOperationConversationKey] = conversationID.String()
		metadata[metaMessengerReviewReplyOperationUserKey] = userID.String()
		if err := tx.Model(&models.ChannelAccount{}).Where(
			"id = ? AND organization_id = ?",
			binding.Account.ID,
			organizationID,
		).Updates(map[string]any{
			"metadata":      metadata,
			"updated_by_id": userID,
			"updated_at":    now,
		}).Error; err != nil {
			return err
		}
		if err := audit.LogAudit(
			tx,
			organizationID,
			userID,
			audit.GetUserName(tx, userID),
			"meta_messenger_review_reply",
			message.ID,
			models.AuditActionCreated,
			nil,
			map[string]any{
				"operation_id":             outbox.ID,
				"conversation_id":          conversationID,
				"page_id":                  binding.Tuple.PageID,
				"recipient_label":          maskedMetaMessengerReviewRecipient(binding.RecipientID),
				"recipient_digest":         metaMessengerReviewRecipientDigest(binding.RecipientID),
				"review_generation":        binding.Tuple.Generation,
				"oauth_credential_id":      binding.OAuthCredentialID,
				"oauth_credential_version": binding.OAuthCredentialVersion,
				"state":                    metaMessengerReviewReplyStateDispatching,
				"manual_confirmation":      true,
			},
		); err != nil {
			return err
		}
		result.Message = message
		result.Parts = parts
		result.Outbox = outbox
		return nil
	})
	return result, authErr, err
}

// revalidateMetaMessengerReviewReplyBeforeGraph is the second half of the
// delivery fence. If deprovisioning or a policy mutation won after the durable
// dispatch row was created, this locked check fails before the Page token is
// decrypted or any provider request begins. Once it succeeds, the permanent
// review dispatch marker prevents deprovision/worker recovery until settlement.
func (a *App) revalidateMetaMessengerReviewReplyBeforeGraph(
	ctx context.Context,
	organizationID, userID uuid.UUID,
	attempt metaMessengerReviewReplyAttempt,
) (metaMessengerReviewReplyBinding, error) {
	var refreshed metaMessengerReviewReplyBinding
	root := a.rootApp()
	if root == nil || root.DB == nil || attempt.Outbox.ID == uuid.Nil {
		return refreshed, errMetaMessengerReviewReplyUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := database.WithTenantReadCommitted(root.DB.WithContext(ctx), organizationID, func(tx *gorm.DB) error {
		if err := root.requirePinnedMetaMessengerReviewerTx(tx, organizationID, userID); err != nil {
			return err
		}
		now := time.Now().UTC()
		binding, reasonCode, _, err := root.loadMetaMessengerReviewReplyBindingTx(
			tx,
			organizationID,
			userID,
			attempt.Binding.Conversation.ID,
			now,
			true,
		)
		if err != nil {
			return err
		}
		if reasonCode != "eligible" || !attempt.Binding.AttestationExpiresAt.After(now) {
			return errMetaMessengerReviewReplyIneligible
		}
		var outbox models.OutboxJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND channel_account_id = ?",
			attempt.Outbox.ID,
			organizationID,
			binding.Account.ID,
		).First(&outbox).Error; err != nil {
			return err
		}
		operationExpiry, err := time.Parse(
			time.RFC3339Nano,
			stringConfigValue(outbox.ProviderState, "provider_deadline_at"),
		)
		if err != nil || !operationExpiry.After(now) ||
			outbox.Status != models.OutboxJobStatusReviewDispatching ||
			outbox.IdempotencyKey != metaMessengerReviewReplyIdempotencyKey(binding) ||
			outbox.PayloadDigest != attempt.Outbox.PayloadDigest ||
			stringConfigValue(outbox.ProviderState, "provider_deadline_at") !=
				stringConfigValue(attempt.Outbox.ProviderState, "provider_deadline_at") ||
			stringConfigValue(outbox.ProviderState, "review_mode") != metaMessengerReviewReplyMode ||
			stringConfigValue(outbox.ProviderState, "generation") != binding.Tuple.Generation ||
			stringConfigValue(outbox.ProviderState, "oauth_credential_id") != binding.OAuthCredentialID.String() ||
			stringConfigValue(outbox.ProviderState, "oauth_credential_version") != strconv.Itoa(binding.OAuthCredentialVersion) ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationIDKey) != outbox.ID.String() ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationStateKey) != metaMessengerReviewReplyStateDispatching ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationGenerationKey) != binding.Tuple.Generation ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationCredentialIDKey) != binding.OAuthCredentialID.String() ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationCredentialVersionKey) != strconv.Itoa(binding.OAuthCredentialVersion) ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationConversationKey) != binding.Conversation.ID.String() ||
			stringConfigValue(binding.Account.Metadata, metaMessengerReviewReplyOperationUserKey) != userID.String() ||
			binding.Tuple != attempt.Binding.Tuple ||
			binding.Account.ID != attempt.Binding.Account.ID ||
			binding.Conversation.ID != attempt.Binding.Conversation.ID ||
			binding.Identity.ID != attempt.Binding.Identity.ID ||
			binding.RecipientID != attempt.Binding.RecipientID ||
			binding.LatestInbound.ID != attempt.Binding.LatestInbound.ID ||
			binding.LatestInbound.WhatsAppMessageID != attempt.Binding.LatestInbound.WhatsAppMessageID ||
			!binding.LatestInbound.CreatedAt.UTC().Equal(attempt.Binding.LatestInbound.CreatedAt.UTC()) ||
			binding.OAuthCredentialID != attempt.Binding.OAuthCredentialID ||
			binding.OAuthCredentialVersion != attempt.Binding.OAuthCredentialVersion ||
			binding.EncryptedTokenDigest != attempt.Binding.EncryptedTokenDigest {
			return errMetaMessengerReviewReplyUnavailable
		}
		binding.SessionDigest = attempt.Binding.SessionDigest
		binding.AttestationID = attempt.Binding.AttestationID
		binding.AttestationExpiresAt = attempt.Binding.AttestationExpiresAt
		refreshed = binding
		return nil
	})
	return refreshed, err
}

func (a *App) loadMetaMessengerReviewReplyBindingTx(
	tx *gorm.DB,
	organizationID, userID, conversationID uuid.UUID,
	now time.Time,
	lock bool,
) (metaMessengerReviewReplyBinding, string, string, error) {
	binding := metaMessengerReviewReplyBinding{}
	if a == nil || tx == nil || organizationID == uuid.Nil || userID == uuid.Nil ||
		conversationID == uuid.Nil || now.IsZero() {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}
	_, tuple, err := a.metaMessengerReviewSettings(now)
	if err != nil || tuple.OrganizationID != organizationID.String() {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}
	accountID, err := uuid.Parse(tuple.ChannelAccountID)
	if err != nil || accountID == uuid.Nil || accountID.String() != tuple.ChannelAccountID {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}

	query := tx
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var account models.ChannelAccount
	if err := query.Where(
		"id = ? AND organization_id = ?",
		accountID,
		organizationID,
	).First(&account).Error; err != nil {
		return binding, "unavailable", "", err
	}
	if !a.readyMetaMessengerReviewAccount(&account) {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}

	query = tx
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var conversation models.InboxConversation
	if err := query.Where(
		"id = ? AND organization_id = ? AND channel_account_id = ? AND channel = ?",
		conversationID,
		organizationID,
		accountID,
		models.ChannelMessenger,
	).First(&conversation).Error; err != nil {
		return binding, "unavailable", "", err
	}
	conversation.ChannelAccount = &account
	if conversation.Status != models.InboxConversationStatusOpen {
		return binding, "conversation_not_open", "Only an open inbound Messenger conversation can be used for App Review", nil
	}
	if conversation.ContactIdentityID == nil || *conversation.ContactIdentityID == uuid.Nil {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}

	query = tx
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var identity models.ContactIdentity
	if err := query.Where(
		"id = ? AND organization_id = ? AND channel_account_id = ? AND contact_id = ? AND channel = ?",
		*conversation.ContactIdentityID,
		organizationID,
		accountID,
		conversation.ContactID,
		models.ChannelMessenger,
	).First(&identity).Error; err != nil {
		return binding, "unavailable", "", err
	}
	recipientID := strings.TrimSpace(identity.ExternalID)
	if !validMetaMessengerReviewRecipientID(recipientID) ||
		conversation.ExternalConversationID != recipientID {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}
	var customerParticipants []models.ConversationParticipant
	if err := tx.Where(
		"organization_id = ? AND conversation_id = ? AND role = ? AND left_at IS NULL",
		organizationID,
		conversationID,
		models.ConversationParticipantRoleCustomer,
	).Find(&customerParticipants).Error; err != nil {
		return binding, "unavailable", "", err
	}
	if len(customerParticipants) != 1 ||
		customerParticipants[0].ContactIdentityID == nil ||
		*customerParticipants[0].ContactIdentityID != identity.ID ||
		strings.TrimSpace(customerParticipants[0].ExternalID) != recipientID {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}
	if lock {
		// Consent changes acquire this same contact-row lock. Whichever
		// transaction commits first becomes authoritative for this one
		// bounded provider attempt.
		if err := database.LockContactPolicyScope(
			tx,
			organizationID,
			conversation.ContactID,
		); err != nil {
			return binding, "unavailable", "", err
		}
	}
	entitled, err := channelapi.HasDurableOmnichannelEntitlement(tx, organizationID, now)
	if err != nil {
		return binding, "unavailable", "", err
	}
	if !entitled {
		return binding, "subscription_inactive", "The omnichannel entitlement is not active", nil
	}
	consentAllowed, consentReason, err := channelapi.OutboundConsentAllowed(
		tx,
		organizationID,
		conversation.ContactID,
		accountID,
		models.ChannelMessenger,
		models.ChannelPreferencePurposeService,
		now,
	)
	if err != nil {
		return binding, "unavailable", "", err
	}
	if !consentAllowed {
		return binding, "consent_blocked", "Outbound service consent does not permit this reply (" + consentReason + ")", nil
	}

	var latestInbound models.Message
	if err := tx.Where(
		"organization_id = ? AND inbox_conversation_id = ? AND direction = ? AND status = ?",
		organizationID,
		conversationID,
		models.DirectionIncoming,
		models.MessageStatusReceived,
	).Order("created_at DESC, id DESC").First(&latestInbound).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return binding, "no_fresh_inbound", "A verified inbound Messenger message is required before replying", nil
		}
		return binding, "unavailable", "", err
	}
	if strings.TrimSpace(latestInbound.WhatsAppMessageID) == "" ||
		latestInbound.ContactID != conversation.ContactID ||
		latestInbound.CreatedAt.IsZero() || latestInbound.CreatedAt.After(now) ||
		stringConfigValue(latestInbound.Metadata, "channel") != string(models.ChannelMessenger) ||
		stringConfigValue(latestInbound.Metadata, "provider") != channelapi.RelayProvider {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}
	expectedWindowEnd := latestInbound.CreatedAt.UTC().Add(channelapi.MetaCustomerServiceWindow)
	if conversation.LastInboundAt == nil ||
		!conversation.LastInboundAt.UTC().Equal(latestInbound.CreatedAt.UTC()) ||
		conversation.ServiceWindowEndsAt == nil ||
		!conversation.ServiceWindowEndsAt.UTC().Equal(expectedWindowEnd) ||
		!expectedWindowEnd.After(now.Add(metaMessengerReviewReplyOperationLease)) {
		return binding, "service_window_expired", "The verified 24-hour Messenger customer-service window is closed", nil
	}
	authorityExpiry, err := time.Parse(time.RFC3339Nano, tuple.ExpiresAt)
	if err != nil || !authorityExpiry.After(now.Add(metaMessengerReviewReplyOperationLease)) {
		return binding, "grant_expiring", "The staging review grant is no longer valid for a complete delivery attempt", nil
	}
	if authorityExpiry.Before(expectedWindowEnd) {
		expectedWindowEnd = authorityExpiry
	}

	query = tx
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var credentials []models.ChannelCredential
	if err := query.Where(
		"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
		organizationID,
		accountID,
		models.ChannelCredentialKindOAuth,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
		now.Add(metaMessengerReviewReplyOperationLease),
	).Order("version DESC, id ASC").Find(&credentials).Error; err != nil {
		return binding, "unavailable", "", err
	}
	if len(credentials) != 1 {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}
	credential := credentials[0]
	encryptedToken, tokenOK := credential.CredentialBlob["access_token"].(string)
	if credential.OrganizationID != organizationID ||
		credential.ChannelAccountID != accountID ||
		credential.Kind != models.ChannelCredentialKindOAuth ||
		credential.Version <= 0 || credential.KeyVersion != "app:v1" ||
		!tokenOK || strings.TrimSpace(encryptedToken) != encryptedToken ||
		!appcrypto.IsEncrypted(encryptedToken) ||
		stringConfigValue(credential.Metadata, "app_id") != tuple.MetaAppID ||
		stringConfigValue(credential.Metadata, "page_id") != tuple.PageID ||
		stringConfigValue(credential.Metadata, "meta_business_id") != tuple.MetaBusinessID ||
		stringConfigValue(credential.Metadata, "token_type") != "page" {
		return binding, "unavailable", "", errMetaMessengerReviewReplyUnavailable
	}

	binding = metaMessengerReviewReplyBinding{
		Tuple:                  tuple,
		OrganizationID:         organizationID,
		UserID:                 userID,
		Account:                account,
		Conversation:           conversation,
		Identity:               identity,
		LatestInbound:          latestInbound,
		RecipientID:            recipientID,
		ServiceWindowEndsAt:    expectedWindowEnd,
		EncryptedAccessToken:   encryptedToken,
		EncryptedTokenDigest:   sha256.Sum256([]byte(encryptedToken)),
		OAuthCredentialID:      credential.ID,
		OAuthCredentialVersion: credential.Version,
	}
	if !lock {
		var existing models.OutboxJob
		err := tx.Where(
			"organization_id = ? AND channel_account_id = ? AND idempotency_key = ?",
			organizationID,
			accountID,
			metaMessengerReviewReplyIdempotencyKey(binding),
		).First(&existing).Error
		switch {
		case err == nil && existing.Status == models.OutboxJobStatusSent:
			return binding, "already_sent", "A manual review reply was already sent for the latest inbound message", nil
		case err == nil && existing.Status == models.OutboxJobStatusReviewDispatching:
			return binding, "in_flight", "A manual review reply is currently being delivered", nil
		case err == nil:
			return binding, "prior_attempt_terminal", "The prior provider attempt is terminal and cannot be retried", nil
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return binding, "unavailable", "", err
		}
	}
	return binding, "eligible", "", nil
}

func (a *App) newMetaMessengerReviewReplyAttestation(
	binding metaMessengerReviewReplyBinding,
	now time.Time,
) (string, time.Time, error) {
	if a == nil || a.Config == nil || now.IsZero() || strings.TrimSpace(
		a.Config.MetaMessengerReviewRelay.BrokerAuthSecret,
	) == "" {
		return "", time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	expiresAt := now.UTC().Add(metaMessengerReviewReplyAttestationTTL)
	if binding.ServiceWindowEndsAt.Before(expiresAt) {
		expiresAt = binding.ServiceWindowEndsAt.UTC()
	}
	authorityExpiry, err := time.Parse(time.RFC3339Nano, binding.Tuple.ExpiresAt)
	if err != nil {
		return "", time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	if authorityExpiry.Before(expiresAt) {
		expiresAt = authorityExpiry.UTC()
	}
	// Whole-second expiry makes the serialized token canonical. Keep enough
	// lifetime to complete the bounded dispatch after POST revalidation.
	expiresAt = time.Unix(expiresAt.Unix(), 0).UTC()
	if !expiresAt.After(now.UTC().Add(metaMessengerReviewReplyOperationLease)) {
		return "", time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	mac := a.metaMessengerReviewReplyAttestationMAC(binding, expiresAt.Unix(), nonce)
	if len(mac) != sha256.Size {
		return "", time.Time{}, errMetaMessengerReviewReplyUnavailable
	}
	token := strings.Join([]string{
		"v1",
		strconv.FormatInt(expiresAt.Unix(), 10),
		base64.RawURLEncoding.EncodeToString(nonce),
		base64.RawURLEncoding.EncodeToString(mac),
	}, ".")
	return token, expiresAt, nil
}

func (a *App) verifyMetaMessengerReviewReplyAttestation(
	binding metaMessengerReviewReplyBinding,
	token string,
	now time.Time,
) (time.Time, error) {
	expiresUnix, nonce, providedMAC, ok := parseMetaMessengerReviewReplyAttestation(token)
	if !ok || a == nil || a.Config == nil || now.IsZero() {
		return time.Time{}, errMetaMessengerReviewReplyIneligible
	}
	expiresAt := time.Unix(expiresUnix, 0).UTC()
	now = now.UTC()
	if !expiresAt.After(now) || expiresAt.After(now.Add(metaMessengerReviewReplyAttestationTTL)) ||
		expiresAt.After(binding.ServiceWindowEndsAt.UTC()) {
		return time.Time{}, errMetaMessengerReviewReplyIneligible
	}
	authorityExpiry, err := time.Parse(time.RFC3339Nano, binding.Tuple.ExpiresAt)
	if err != nil || expiresAt.After(authorityExpiry.UTC()) {
		return time.Time{}, errMetaMessengerReviewReplyIneligible
	}
	expectedMAC := a.metaMessengerReviewReplyAttestationMAC(binding, expiresUnix, nonce)
	if len(expectedMAC) != len(providedMAC) ||
		subtle.ConstantTimeCompare(expectedMAC, providedMAC) != 1 {
		return time.Time{}, errMetaMessengerReviewReplyIneligible
	}
	return expiresAt, nil
}

func (a *App) metaMessengerReviewReplyAttestationMAC(
	binding metaMessengerReviewReplyBinding,
	expiresUnix int64,
	nonce []byte,
) []byte {
	if a == nil || a.Config == nil || len(nonce) != 16 {
		return nil
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"rereply-meta-review-manual-reply-attestation:v1",
		binding.Tuple.OrganizationID,
		binding.Tuple.MetaBusinessID,
		binding.Tuple.PageID,
		binding.Tuple.MetaAppID,
		binding.Tuple.ChannelAccountID,
		binding.Tuple.Generation,
		binding.Tuple.ExpiresAt,
		a.Config.MetaMessengerReviewRelay.ReviewerUserID,
		a.Config.MetaMessengerReviewRelay.ReviewerRoleID,
		binding.UserID.String(),
		hex.EncodeToString(binding.SessionDigest[:]),
		binding.Conversation.ID.String(),
		binding.RecipientID,
		binding.LatestInbound.ID.String(),
		binding.LatestInbound.WhatsAppMessageID,
		binding.LatestInbound.CreatedAt.UTC().Format(time.RFC3339Nano),
		binding.ServiceWindowEndsAt.UTC().Format(time.RFC3339Nano),
		binding.OAuthCredentialID.String(),
		strconv.Itoa(binding.OAuthCredentialVersion),
		hex.EncodeToString(binding.EncryptedTokenDigest[:]),
		strconv.FormatInt(expiresUnix, 10),
		base64.RawURLEncoding.EncodeToString(nonce),
	}, "\n")))
	mac := hmac.New(sha256.New, []byte(a.Config.MetaMessengerReviewRelay.BrokerAuthSecret))
	_, _ = mac.Write([]byte("rereply-meta-review-manual-reply-attestation-mac:v1\n"))
	_, _ = mac.Write(digest[:])
	return mac.Sum(nil)
}

func parseMetaMessengerReviewReplyAttestation(value string) (int64, []byte, []byte, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return 0, nil, nil, false
	}
	expiresUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || expiresUnix <= 0 || strconv.FormatInt(expiresUnix, 10) != parts[1] {
		return 0, nil, nil, false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(nonce) != 16 || base64.RawURLEncoding.EncodeToString(nonce) != parts[2] {
		return 0, nil, nil, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(mac) != sha256.Size || base64.RawURLEncoding.EncodeToString(mac) != parts[3] {
		return 0, nil, nil, false
	}
	return expiresUnix, nonce, mac, true
}

func validMetaMessengerReviewReplyAttestationText(value string) bool {
	_, _, _, ok := parseMetaMessengerReviewReplyAttestation(value)
	return ok
}

func validMetaMessengerReviewRecipientID(value string) bool {
	if value == "" || len(value) > 64 || value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func metaMessengerReviewReplyDigest(
	binding metaMessengerReviewReplyBinding,
	text string,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"rereply-meta-review-manual-reply-payload:v1",
		binding.Tuple.OrganizationID,
		binding.Tuple.PageID,
		binding.Tuple.Generation,
		binding.Conversation.ID.String(),
		binding.RecipientID,
		binding.LatestInbound.ID.String(),
		binding.LatestInbound.WhatsAppMessageID,
		text,
	}, "\n")))
	return hex.EncodeToString(digest[:])
}

// metaMessengerReviewReplyIdempotencyKey deliberately does not include the
// browser-supplied nonce or attestation. There may be only one provider
// attempt for a given exact deployment generation and latest verified inbound
// message. Re-mounting the UI or minting a new client nonce therefore cannot
// create a second Graph call.
func metaMessengerReviewReplyIdempotencyKey(
	binding metaMessengerReviewReplyBinding,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"rereply-meta-review-manual-reply-idempotency:v1",
		binding.Tuple.OrganizationID,
		binding.Tuple.PageID,
		binding.Tuple.ChannelAccountID,
		binding.Tuple.Generation,
		binding.Conversation.ID.String(),
		binding.LatestInbound.ID.String(),
		binding.LatestInbound.WhatsAppMessageID,
	}, "\n")))
	return models.StagingMessengerReviewIdempotencyKeyPrefix + hex.EncodeToString(digest[:])
}

func (a *App) sendMetaMessengerReviewGraphReply(
	ctx context.Context,
	pageID, recipientID, text, pageToken string,
) metaMessengerReviewGraphReplyResult {
	result := metaMessengerReviewGraphReplyResult{}
	if a == nil || ctx == nil || !validMetaMessengerReviewRecipientID(recipientID) ||
		strings.TrimSpace(pageID) == "" || strings.TrimSpace(pageToken) == "" {
		result.ErrorCode = "invalid_delivery_binding"
		return result
	}
	if ctx.Err() != nil {
		result.ErrorCode = "preflight_deadline_expired"
		return result
	}
	target, err := a.metaMessengerGraphEndpoint(url.PathEscape(pageID) + "/messages")
	if err != nil {
		result.ErrorCode = "invalid_graph_endpoint"
		return result
	}
	payload, err := json.Marshal(map[string]any{
		"recipient":      map[string]string{"id": recipientID},
		"messaging_type": "RESPONSE",
		"message":        map[string]string{"text": text},
	})
	if err != nil {
		result.ErrorCode = "invalid_message"
		return result
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		target.String(),
		bytes.NewReader(payload),
	)
	if err != nil {
		result.ErrorCode = "request_prepare_failed"
		return result
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+pageToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Messenger-App-Review/1.0")
	client := oauthHTTPClientWithoutRedirects(a.HTTPClient)
	if client.Timeout <= 0 || client.Timeout > metaMessengerReviewReplyGraphTimeout {
		client.Timeout = metaMessengerReviewReplyGraphTimeout
	}
	if ctx.Err() != nil {
		result.ErrorCode = "preflight_deadline_expired"
		return result
	}
	response, err := client.Do(request)
	if err != nil {
		result.Ambiguous = true
		result.ErrorCode = "delivery_outcome_ambiguous"
		return result
	}
	defer func() { _ = response.Body.Close() }()
	result.StatusCode = response.StatusCode
	body, err := io.ReadAll(io.LimitReader(response.Body, metaMessengerGraphMaxResponse+1))
	if err != nil || int64(len(body)) > metaMessengerGraphMaxResponse || !utf8.Valid(body) {
		result.Ambiguous = true
		result.ErrorCode = "delivery_outcome_ambiguous"
		return result
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if (response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest) ||
			response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusConflict ||
			response.StatusCode == http.StatusTooEarly ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError {
			result.Ambiguous = true
			result.ErrorCode = "delivery_outcome_ambiguous"
			return result
		}
		result.ErrorCode = "meta_rejected_" + strconv.Itoa(response.StatusCode)
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var graphResponse metaMessengerReviewGraphReplyResponse
	if err := decoder.Decode(&graphResponse); err != nil {
		result.Ambiguous = true
		result.ErrorCode = "delivery_outcome_ambiguous"
		return result
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		result.Ambiguous = true
		result.ErrorCode = "delivery_outcome_ambiguous"
		return result
	}
	if graphResponse.RecipientID != recipientID ||
		!validMetaMessengerReviewProviderMessageID(graphResponse.MessageID) {
		result.Ambiguous = true
		result.ErrorCode = "delivery_outcome_ambiguous"
		return result
	}
	result.Sent = true
	result.RecipientID = graphResponse.RecipientID
	result.MessageID = graphResponse.MessageID
	return result
}

func validMetaMessengerReviewProviderMessageID(value string) bool {
	if value == "" || len(value) > metaMessengerReviewProviderMIDMaxBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (a *App) settleMetaMessengerReviewReply(
	organizationID, userID uuid.UUID,
	attempt metaMessengerReviewReplyAttempt,
	result metaMessengerReviewGraphReplyResult,
) (metaMessengerReviewReplyAttempt, error) {
	root := a.rootApp()
	if root == nil || root.DB == nil || attempt.Outbox.ID == uuid.Nil ||
		attempt.Message.ID == uuid.Nil {
		return attempt, errMetaMessengerReviewReplyUnavailable
	}
	now := time.Now().UTC()
	err := database.WithTenantReadCommitted(root.DB, organizationID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ?",
			attempt.Binding.Account.ID,
			organizationID,
		).First(&account).Error; err != nil {
			return err
		}
		var outbox models.OutboxJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND channel_account_id = ?",
			attempt.Outbox.ID,
			organizationID,
			attempt.Binding.Account.ID,
		).First(&outbox).Error; err != nil {
			return err
		}
		if outbox.Status != models.OutboxJobStatusReviewDispatching ||
			outbox.PayloadDigest != attempt.Outbox.PayloadDigest ||
			stringConfigValue(outbox.ProviderState, "review_mode") != metaMessengerReviewReplyMode ||
			stringConfigValue(outbox.ProviderState, "generation") != attempt.Binding.Tuple.Generation ||
			stringConfigValue(outbox.ProviderState, "oauth_credential_id") != attempt.Binding.OAuthCredentialID.String() ||
			stringConfigValue(outbox.ProviderState, "oauth_credential_version") != strconv.Itoa(attempt.Binding.OAuthCredentialVersion) ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationIDKey) != outbox.ID.String() ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationStateKey) != metaMessengerReviewReplyStateDispatching ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationGenerationKey) != attempt.Binding.Tuple.Generation ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationCredentialIDKey) != attempt.Binding.OAuthCredentialID.String() ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationCredentialVersionKey) != strconv.Itoa(attempt.Binding.OAuthCredentialVersion) ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationConversationKey) != attempt.Binding.Conversation.ID.String() ||
			stringConfigValue(account.Metadata, metaMessengerReviewReplyOperationUserKey) != userID.String() {
			return errMetaMessengerReviewReplyUnavailable
		}

		state := metaMessengerReviewReplyStateRejected
		messageStatus := models.MessageStatusFailed
		outboxStatus := models.OutboxJobStatusFailed
		errorCode := result.ErrorCode
		errorMessage := "Meta did not accept the staging review reply"
		providerState := cloneJSONB(outbox.ProviderState)
		providerState["provider_http_status"] = result.StatusCode
		providerState["settled_at"] = now.Format(time.RFC3339Nano)
		if result.Sent {
			state = metaMessengerReviewReplyStateSent
			messageStatus = models.MessageStatusSent
			outboxStatus = models.OutboxJobStatusSent
			errorCode = ""
			errorMessage = ""
			providerState["provider_message_id"] = result.MessageID
			providerState["provider_recipient_label"] = maskedMetaMessengerReviewRecipient(result.RecipientID)
			providerState["provider_recipient_digest"] = metaMessengerReviewRecipientDigest(result.RecipientID)
		} else if result.Ambiguous {
			state = metaMessengerReviewReplyStateAmbiguous
			errorCode = "delivery_outcome_ambiguous"
			errorMessage = "Meta delivery outcome is ambiguous; automatic retry is forbidden"
		}
		outboxUpdates := map[string]any{
			"status":          outboxStatus,
			"last_error_code": errorCode,
			"last_error":      errorMessage,
			"locked_at":       nil,
			"locked_by":       "",
			"provider_state":  providerState,
			"updated_at":      now,
		}
		if result.Sent {
			outboxUpdates["sent_at"] = now
		} else {
			outboxUpdates["failed_at"] = now
		}
		if err := tx.Model(&models.OutboxJob{}).Where(
			"id = ? AND organization_id = ? AND status = ?",
			outbox.ID,
			organizationID,
			models.OutboxJobStatusReviewDispatching,
		).Updates(outboxUpdates).Error; err != nil {
			return err
		}
		messageUpdates := map[string]any{
			"status":        messageStatus,
			"error_message": errorMessage,
			"updated_at":    now,
		}
		if result.Sent {
			messageUpdates["whats_app_message_id"] = result.MessageID
		}
		if err := tx.Model(&models.Message{}).Where(
			"id = ? AND organization_id = ? AND inbox_conversation_id = ?",
			attempt.Message.ID,
			organizationID,
			attempt.Binding.Conversation.ID,
		).Updates(messageUpdates).Error; err != nil {
			return err
		}
		metadata := cloneJSONB(account.Metadata)
		delete(metadata, metaMessengerReviewReplyOperationIDKey)
		delete(metadata, metaMessengerReviewReplyOperationStateKey)
		delete(metadata, metaMessengerReviewReplyOperationExpiryKey)
		delete(metadata, metaMessengerReviewReplyOperationGenerationKey)
		delete(metadata, metaMessengerReviewReplyOperationCredentialIDKey)
		delete(metadata, metaMessengerReviewReplyOperationCredentialVersionKey)
		delete(metadata, metaMessengerReviewReplyOperationConversationKey)
		delete(metadata, metaMessengerReviewReplyOperationUserKey)
		metadata[metaMessengerReviewReplyLastOperationIDKey] = outbox.ID.String()
		metadata[metaMessengerReviewReplyLastOperationStateKey] = state
		metadata[metaMessengerReviewReplyLastSettledAtKey] = now.Format(time.RFC3339Nano)
		accountUpdates := map[string]any{
			"metadata":      metadata,
			"updated_by_id": userID,
			"updated_at":    now,
		}
		if result.Sent {
			accountUpdates["last_outbound_at"] = now
		}
		if err := tx.Model(&models.ChannelAccount{}).Where(
			"id = ? AND organization_id = ?",
			account.ID,
			organizationID,
		).Updates(accountUpdates).Error; err != nil {
			return err
		}
		if result.Sent {
			if err := tx.Model(&models.InboxConversation{}).Where(
				"id = ? AND organization_id = ? AND channel_account_id = ?",
				attempt.Binding.Conversation.ID,
				organizationID,
				account.ID,
			).Updates(map[string]any{
				"last_message_at":      now,
				"last_outbound_at":     now,
				"last_message_preview": attempt.Message.Content,
				"updated_at":           now,
			}).Error; err != nil {
				return err
			}
		}
		if err := audit.LogAudit(
			tx,
			organizationID,
			userID,
			audit.GetUserName(tx, userID),
			"meta_messenger_review_reply",
			attempt.Message.ID,
			models.AuditActionUpdated,
			map[string]any{"state": metaMessengerReviewReplyStateDispatching},
			map[string]any{
				"operation_id":        outbox.ID,
				"state":               state,
				"provider_message_id": result.MessageID,
				"settled_at":          now,
			},
		); err != nil {
			return err
		}
		if err := tx.Where(
			"id = ? AND organization_id = ?",
			attempt.Outbox.ID,
			organizationID,
		).First(&attempt.Outbox).Error; err != nil {
			return err
		}
		if err := tx.Where(
			"id = ? AND organization_id = ?",
			attempt.Message.ID,
			organizationID,
		).First(&attempt.Message).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return attempt, err
	}
	return attempt, nil
}

// hasProtectedMetaMessengerReviewDispatchTx never expires a protected review operation.
// Any nonterminal row carrying the dedicated status, server-derived key
// prefix, or provider-state marker is a permanent fence until the dedicated
// handler deterministically settles it. A crashed attempt therefore requires
// a future explicit, audited operator reconciliation; wall-clock age can never
// authorize deprovisioning or another provider call.
func hasProtectedMetaMessengerReviewDispatchTx(
	tx *gorm.DB,
	organizationID, channelAccountID uuid.UUID,
	now time.Time,
) (bool, error) {
	if tx == nil || organizationID == uuid.Nil || channelAccountID == uuid.Nil || now.IsZero() {
		return false, errMetaMessengerReviewReplyUnavailable
	}
	var jobs []models.OutboxJob
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ? AND status IN ?",
		organizationID,
		channelAccountID,
		[]models.OutboxJobStatus{
			models.OutboxJobStatusPending,
			models.OutboxJobStatusRetrying,
			models.OutboxJobStatusProcessing,
			models.OutboxJobStatusDispatching,
			models.OutboxJobStatusReviewDispatching,
		},
	).Order("created_at, id").Find(&jobs).Error; err != nil {
		return false, err
	}
	for index := range jobs {
		job := jobs[index]
		if job.Status == models.OutboxJobStatusReviewDispatching ||
			strings.HasPrefix(job.IdempotencyKey, models.StagingMessengerReviewIdempotencyKeyPrefix) ||
			stringConfigValue(job.ProviderState, "review_mode") == metaMessengerReviewReplyMode ||
			job.Status == models.OutboxJobStatusDispatching {
			return true, nil
		}
	}
	return false, nil
}

func valueOrZeroTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}
