package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	ThreadsProvider            = "threads"
	defaultThreadsAPIBaseURL   = "https://graph.threads.net/v1.0"
	defaultThreadsHTTPTimeout  = 30 * time.Second
	defaultThreadsResponseSize = int64(1 << 20)
	maxThreadsTextRunes        = 500
)

var (
	ErrThreadsWebhookUnsupported   = errors.New("threads webhook operation is not configured")
	ErrThreadsOperationUnsupported = errors.New("threads direct messages, standalone posts, media, and read receipts are not supported")
	ErrThreadsCredentialMissing    = errors.New("threads OAuth credential is not configured")
)

var requiredThreadsScopes = [...]string{
	"threads_basic",
	"threads_read_replies",
	"threads_manage_replies",
	"threads_content_publish",
	"threads_manage_mentions",
}

// RequiredThreadsScopes returns the complete permission set needed by the
// public-replies-and-mentions adapter.
func RequiredThreadsScopes() []string {
	result := make([]string, len(requiredThreadsScopes))
	copy(result, requiredThreadsScopes[:])
	return result
}

// ThreadsPermissionSnapshot contains only non-secret debug-token metadata.
type ThreadsPermissionSnapshot struct {
	UserID              string
	Scopes              []string
	ExpiresAt           *time.Time
	DataAccessExpiresAt *time.Time
	CheckedAt           time.Time
}

// ThreadsAdapter is the first-party public-engagement adapter. It deliberately
// exposes only exact-target text replies; direct messages and standalone posts
// have no representation in this adapter.
type ThreadsAdapter struct {
	client             *http.Client
	encryptionKey      string
	apiBaseURL         string
	now                func() time.Time
	webhookAppSecret   string
	webhookVerifyToken string
}

// NewThreadsWebhookAdapter adds the two app-level values required to verify
// Meta's callback handshake and HMAC-signed event deliveries.
func NewThreadsWebhookAdapter(
	client *http.Client,
	encryptionKey, appSecret, verifyToken string,
) *ThreadsAdapter {
	adapter := NewThreadsAdapter(client, encryptionKey)
	adapter.webhookAppSecret = strings.TrimSpace(appSecret)
	adapter.webhookVerifyToken = strings.TrimSpace(verifyToken)
	return adapter
}

func NewThreadsAdapter(client *http.Client, encryptionKey string) *ThreadsAdapter {
	if client == nil {
		client = &http.Client{Timeout: defaultThreadsHTTPTimeout}
	} else {
		copy := *client
		client = &copy
		if client.Timeout <= 0 {
			client.Timeout = defaultThreadsHTTPTimeout
		}
	}
	// OAuth/provider calls must never carry credentials across a redirect.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &ThreadsAdapter{
		client:        client,
		encryptionKey: encryptionKey,
		apiBaseURL:    defaultThreadsAPIBaseURL,
		now:           time.Now,
	}
}

func (a *ThreadsAdapter) Channel() models.Channel { return models.ChannelThreads }
func (a *ThreadsAdapter) Provider() string        { return ThreadsProvider }

func (a *ThreadsAdapter) Capabilities(_ *models.ChannelAccount) Capabilities {
	return Capabilities{Text: true, Replies: true}
}

func (a *ThreadsAdapter) RouteHint(_ http.Header, body []byte) (WebhookRouteHint, error) {
	events, err := decodeThreadsWebhookEnvelopes(body)
	if err != nil || len(events) == 0 {
		return WebhookRouteHint{}, errors.New("threads webhook payload is invalid")
	}
	appID := strings.TrimSpace(events[0].AppID)
	if appID == "" {
		return WebhookRouteHint{}, errors.New("threads webhook app ID is missing")
	}
	for index := 1; index < len(events); index++ {
		if strings.TrimSpace(events[index].AppID) != appID {
			return WebhookRouteHint{}, errors.New("threads webhook batch contains multiple apps")
		}
	}
	return WebhookRouteHint{
		ExternalObjectID: strings.TrimSpace(string(events[0].TargetID)),
		Provider:         ThreadsProvider,
		Metadata: map[string]any{
			"app_id": appID,
			"topic":  events[0].Topic,
		},
	}, nil
}

func (a *ThreadsAdapter) VerifyWebhook(_ *models.ChannelAccount, headers http.Header, body []byte) error {
	secret := strings.TrimSpace(a.webhookAppSecret)
	provided := strings.TrimSpace(headers.Get("X-Hub-Signature-256"))
	if secret == "" || provided == "" || !strings.HasPrefix(provided, "sha256=") {
		return errors.New("threads webhook signature is missing")
	}
	providedDigest, err := hex.DecodeString(strings.TrimPrefix(provided, "sha256="))
	if err != nil || len(providedDigest) != sha256.Size {
		return errors.New("threads webhook signature is invalid")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	if !hmac.Equal(providedDigest, mac.Sum(nil)) {
		return errors.New("threads webhook signature is invalid")
	}
	return nil
}

// VerifyChallenge validates Meta's GET subscription handshake token.
func (a *ThreadsAdapter) VerifyChallenge(provided string) error {
	expected := []byte(strings.TrimSpace(a.webhookVerifyToken))
	actual := []byte(strings.TrimSpace(provided))
	if len(expected) < 16 || len(expected) != len(actual) || !hmac.Equal(expected, actual) {
		return errors.New("threads webhook verify token is invalid")
	}
	return nil
}

// ValidateWebhookAccount rejects signed Threads payloads that do not belong to
// the connected profile. Relay handlers call this before durably persisting the
// raw webhook; NormalizeWebhook repeats the same validation before emitting any
// canonical events.
func (a *ThreadsAdapter) ValidateWebhookAccount(account *models.ChannelAccount, body []byte) error {
	_, err := validatedThreadsWebhookEnvelopes(account, body)
	return err
}

func (a *ThreadsAdapter) NormalizeWebhook(
	_ context.Context,
	account *models.ChannelAccount,
	body []byte,
) ([]InboundEvent, error) {
	envelopes, err := validatedThreadsWebhookEnvelopes(account, body)
	if err != nil {
		return nil, err
	}
	accountUsername := strings.ToLower(threadsString(account.Metadata, "username"))
	now := a.now().UTC()
	events := make([]InboundEvent, 0, len(envelopes))
	for index := range envelopes {
		envelope := &envelopes[index]
		field := strings.ToLower(strings.TrimSpace(envelope.Values.Field))
		engagementType := ""
		switch field {
		case "replies":
			engagementType = "reply"
		case "mentions":
			engagementType = "mention"
		default:
			continue
		}
		value := &envelope.Values.Value
		mediaID := strings.TrimSpace(string(value.ID))
		username := strings.TrimSpace(value.Username)
		if mediaID == "" || username == "" ||
			(accountUsername != "" && strings.EqualFold(accountUsername, username)) {
			continue
		}
		sentAt := parseThreadsWebhookTime(value.Timestamp)
		if sentAt.IsZero() && envelope.Time > 0 {
			sentAt = time.Unix(envelope.Time, 0).UTC()
		}
		if sentAt.IsZero() || sentAt.After(now.Add(5*time.Minute)) {
			sentAt = now
		}
		senderID := strings.TrimSpace(string(value.OwnerID))
		if senderID == "" {
			senderID = "username:" + strings.ToLower(username)
		}
		rootID := strings.TrimSpace(string(value.RootPost.ID))
		repliedToID := strings.TrimSpace(string(value.RepliedTo.ID))
		metadata := map[string]any{
			"engagement_type": engagementType,
			"root_post_id":    rootID,
			"replied_to_id":   repliedToID,
			"permalink":       strings.TrimSpace(value.Permalink),
			"media_type":      strings.TrimSpace(value.MediaType),
			"topic":           strings.TrimSpace(envelope.Topic),
		}
		text := strings.TrimSpace(value.Text)
		if text == "" {
			text = map[string]string{
				"reply":   "Public Threads reply",
				"mention": "Public Threads mention",
			}[engagementType]
		}
		events = append(events, InboundEvent{
			DedupeKey:       "threads:" + field + ":" + mediaID,
			ProviderEventID: mediaID,
			Type:            NormalizedEventTypeMessage,
			OccurredAt:      sentAt,
			Message: &InboundMessage{
				ExternalMessageID: mediaID,
				Conversation: ConversationRef{
					ExternalID: mediaID,
					Subject:    "Threads " + engagementType + " from @" + username,
					Metadata:   metadata,
				},
				Sender: Participant{
					ExternalID:  senderID,
					Address:     username,
					DisplayName: "@" + username,
					Role:        models.ConversationParticipantRoleCustomer,
					Metadata: map[string]any{
						"username":            username,
						"profile_picture_url": strings.TrimSpace(value.ProfilePictureURL),
						"is_verified":         value.IsVerified,
					},
				},
				Direction:         models.DirectionIncoming,
				Parts:             []MessagePart{{Type: models.MessagePartTypeText, Text: text}},
				ReplyToExternalID: repliedToID,
				SentAt:            sentAt,
				ReceivedAt:        now,
				Metadata:          metadata,
			},
			Payload: map[string]any{
				"subscription_id": strings.TrimSpace(envelope.SubscriptionID),
			},
		})
	}
	return events, nil
}

func validatedThreadsWebhookEnvelopes(
	account *models.ChannelAccount,
	body []byte,
) ([]threadsWebhookEnvelope, error) {
	if account == nil || account.Channel != models.ChannelThreads ||
		!strings.EqualFold(account.Provider, ThreadsProvider) {
		return nil, errors.New("a first-party Threads account is required")
	}
	envelopes, err := decodeThreadsWebhookEnvelopes(body)
	if err != nil {
		return nil, err
	}
	if len(envelopes) > 1000 {
		return nil, errors.New("threads webhook batch is too large")
	}
	expectedAppID := threadsString(account.Metadata, "app_id")
	if expectedAppID == "" {
		return nil, errors.New("threads account app binding is missing")
	}
	accountExternalID := strings.TrimSpace(account.ExternalAccountID)
	if accountExternalID == "" {
		return nil, errors.New("threads connected account identity is missing")
	}
	for index := range envelopes {
		envelope := &envelopes[index]
		if !hmac.Equal([]byte(expectedAppID), []byte(strings.TrimSpace(envelope.AppID))) {
			return nil, errors.New("threads webhook app does not match the connected account")
		}
		switch strings.ToLower(strings.TrimSpace(envelope.Values.Field)) {
		case "replies":
			rootOwnerID := strings.TrimSpace(string(envelope.Values.Value.RootPost.OwnerID))
			if rootOwnerID != accountExternalID {
				return nil, errors.New("threads reply root post belongs to a different connected account")
			}
		case "mentions":
			if strings.TrimSpace(string(envelope.TargetID)) != accountExternalID {
				return nil, errors.New("threads mention targets a different connected account")
			}
		}
	}
	return envelopes, nil
}

func (a *ThreadsAdapter) Send(
	ctx context.Context,
	account *models.ChannelAccount,
	message OutboundMessage,
) (SendResult, error) {
	state, err := a.PrepareSend(ctx, account, message)
	if err != nil {
		return SendResult{}, err
	}
	return a.PublishPrepared(ctx, account, message, state)
}

// PrepareSend creates an unpublished Threads reply container. The outbox
// persists this opaque state before crossing its dispatch fence, so a worker
// restart can resume the same container without creating a duplicate reply.
func (a *ThreadsAdapter) PrepareSend(
	ctx context.Context,
	account *models.ChannelAccount,
	message OutboundMessage,
) (models.JSONB, error) {
	target, text, err := a.validatePublicReply(account, message)
	if err != nil {
		return nil, err
	}
	accessToken, err := a.accessToken(account)
	if err != nil {
		return nil, err
	}
	createValues := url.Values{
		"media_type":  {"TEXT"},
		"text":        {text},
		"reply_to_id": {target},
	}
	var container struct {
		ID string `json:"id"`
	}
	createHeaders, err := a.doJSON(ctx, http.MethodPost, "/me/threads", createValues, accessToken, &container)
	if err != nil {
		return nil, err
	}
	container.ID = strings.TrimSpace(container.ID)
	if container.ID == "" || len(container.ID) > 512 {
		return nil, threadsProviderError("create_reply", "invalid_response", "Threads returned no valid reply container ID", false, nil)
	}
	return models.JSONB{
		"creation_id": container.ID,
		"reply_to_id": target,
		"request_id":  firstThreadsHeader(createHeaders, "x-fb-request-id", "x-fb-trace-id"),
	}, nil
}

// PublishPrepared publishes only the exact container and target persisted by
// PrepareSend. It never creates a replacement container.
func (a *ThreadsAdapter) PublishPrepared(
	ctx context.Context,
	account *models.ChannelAccount,
	message OutboundMessage,
	state models.JSONB,
) (SendResult, error) {
	target, _, err := a.validatePublicReply(account, message)
	if err != nil {
		return SendResult{}, err
	}
	containerID := threadsString(state, "creation_id")
	preparedTarget := threadsString(state, "reply_to_id")
	if containerID == "" || len(containerID) > 512 || preparedTarget == "" || preparedTarget != target {
		return SendResult{}, threadsProviderError("publish_reply", "prepared_state_invalid", "Prepared Threads reply state does not match the selected target", false, nil)
	}
	accessToken, err := a.accessToken(account)
	if err != nil {
		return SendResult{}, err
	}
	publishValues := url.Values{"creation_id": {containerID}}
	var published struct {
		ID string `json:"id"`
	}
	publishHeaders, err := a.doJSON(
		ctx,
		http.MethodPost,
		"/me/threads_publish",
		publishValues,
		accessToken,
		&published,
	)
	if err != nil {
		return SendResult{}, err
	}
	published.ID = strings.TrimSpace(published.ID)
	if published.ID == "" {
		return SendResult{}, threadsProviderError("publish_reply", "invalid_response", "Threads returned no published reply ID", false, nil)
	}
	requestID := firstThreadsHeader(publishHeaders, "x-fb-request-id", "x-fb-trace-id")
	if requestID == "" {
		requestID = threadsString(state, "request_id")
	}
	return SendResult{
		ProviderMessageIDs:     []string{published.ID},
		ExternalConversationID: target,
		ProviderRequestID:      requestID,
		Status:                 models.MessageStatusSent,
		AcceptedAt:             a.now().UTC(),
		Raw:                    map[string]any{"container_id": containerID},
	}, nil
}

func (a *ThreadsAdapter) validatePublicReply(
	account *models.ChannelAccount,
	message OutboundMessage,
) (string, string, error) {
	if account == nil || account.Channel != models.ChannelThreads ||
		!strings.EqualFold(account.Provider, ThreadsProvider) {
		return "", "", threadsProviderError("send", "invalid_account", "a first-party Threads account is required", false, nil)
	}
	if !threadsBool(account.Config, "outbound_enabled") ||
		!strings.EqualFold(threadsString(account.Config, "engagement_mode"), "public_replies_mentions") {
		return "", "", threadsProviderError("send", "outbound_disabled", "Threads public replies are not enabled", false, nil)
	}
	target := strings.TrimSpace(message.ReplyToExternalID)
	if target == "" {
		return "", "", threadsProviderError("send", "reply_target_required", "Threads replies require an existing public reply or mention target", false, nil)
	}
	if strings.TrimSpace(message.IdempotencyKey) == "" {
		return "", "", threadsProviderError("send", "idempotency_key_required", "Threads reply requires an idempotency key", false, nil)
	}
	if len(message.Parts) != 1 || message.Parts[0].Type != models.MessagePartTypeText {
		return "", "", threadsProviderError("send", "text_only", "Threads public replies support exactly one text part", false, nil)
	}
	text := strings.TrimSpace(message.Parts[0].Text)
	if text == "" || utf8.RuneCountInString(text) > maxThreadsTextRunes {
		return "", "", threadsProviderError("send", "invalid_text", "Threads reply text must contain 1 to 500 characters", false, nil)
	}
	return target, text, nil
}

func (a *ThreadsAdapter) MarkRead(context.Context, *models.ChannelAccount, ConversationRef, []string) error {
	return ErrThreadsOperationUnsupported
}

func (a *ThreadsAdapter) FetchMedia(context.Context, *models.ChannelAccount, MediaRef) (FetchedMedia, error) {
	return FetchedMedia{}, ErrThreadsOperationUnsupported
}

func (a *ThreadsAdapter) ValidateAccount(ctx context.Context, account *models.ChannelAccount) (AccountValidationResult, error) {
	result := AccountValidationResult{
		Capabilities: a.Capabilities(account),
		CheckedAt:    a.now().UTC(),
	}
	if account == nil || strings.TrimSpace(account.ExternalAccountID) == "" {
		return result, threadsProviderError("validate_account", "invalid_account", "Threads account identity is missing", false, nil)
	}
	accessToken, err := a.accessToken(account)
	if err != nil {
		return result, err
	}
	var profile struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	_, err = a.doJSON(ctx, http.MethodGet, "/me", url.Values{
		"fields": {"id,username,name"},
	}, accessToken, &profile)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(profile.ID) == "" || profile.ID != strings.TrimSpace(account.ExternalAccountID) {
		return result, threadsProviderError("validate_account", "account_mismatch", "Threads authorization belongs to a different account", false, nil)
	}
	permissions, err := a.InspectTokenPermissions(ctx, accessToken, account.ExternalAccountID)
	if err != nil {
		return result, err
	}
	result.Valid = true
	result.Status = string(models.ChannelAccountStatusActive)
	result.Metadata = map[string]any{
		"id":       profile.ID,
		"username": profile.Username,
		"name":     profile.Name,
		"scopes":   permissions.Scopes,
	}
	return result, nil
}

func (a *ThreadsAdapter) Subscribe(context.Context, *models.ChannelAccount) error {
	// Threads enrollment is app-level in the Meta use-case dashboard; the
	// authorized install user is enrolled by OAuth and no subscribed_apps edge
	// exists for Threads.
	return nil
}

func (a *ThreadsAdapter) RefreshCredentials(ctx context.Context, account *models.ChannelAccount) (CredentialRefreshResult, error) {
	accessToken, err := a.accessToken(account)
	if err != nil {
		return CredentialRefreshResult{}, err
	}
	var refreshed struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	_, err = a.doJSON(ctx, http.MethodGet, "/refresh_access_token", url.Values{
		"grant_type":   {"th_refresh_token"},
		"access_token": {accessToken},
	}, accessToken, &refreshed)
	if err != nil {
		return CredentialRefreshResult{}, err
	}
	refreshed.AccessToken = strings.TrimSpace(refreshed.AccessToken)
	if refreshed.AccessToken == "" || refreshed.ExpiresIn <= 0 {
		return CredentialRefreshResult{}, threadsProviderError("refresh_token", "invalid_response", "Threads returned an invalid refreshed credential", false, nil)
	}
	permissions, err := a.InspectTokenPermissions(ctx, refreshed.AccessToken, account.ExternalAccountID)
	if err != nil {
		return CredentialRefreshResult{}, err
	}
	if strings.TrimSpace(a.encryptionKey) == "" {
		return CredentialRefreshResult{}, threadsProviderError("refresh_token", "encryption_unavailable", "Threads credential encryption is unavailable", false, nil)
	}
	encrypted, err := appcrypto.Encrypt(refreshed.AccessToken, a.encryptionKey)
	if err != nil || !appcrypto.IsEncrypted(encrypted) {
		return CredentialRefreshResult{}, threadsProviderError("refresh_token", "encryption_failed", "Threads credential could not be protected", false, err)
	}
	now := a.now().UTC()
	expiresAt := now.Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	for _, permissionExpiry := range []*time.Time{permissions.ExpiresAt, permissions.DataAccessExpiresAt} {
		if permissionExpiry != nil && permissionExpiry.Before(expiresAt) {
			expiresAt = permissionExpiry.UTC()
		}
	}
	refreshMetadata := models.JSONB{
		"token_type":             "long_lived",
		"granted_scopes":         permissions.Scopes,
		"permissions_checked_at": permissions.CheckedAt.Format(time.RFC3339Nano),
	}
	if permissions.DataAccessExpiresAt != nil {
		refreshMetadata["data_access_expires_at"] = permissions.DataAccessExpiresAt.UTC().Format(time.RFC3339)
	}
	return CredentialRefreshResult{
		CredentialBlob: models.JSONB{"access_token": encrypted},
		KeyVersion:     "app:v1",
		ExpiresAt:      &expiresAt,
		RefreshedAt:    now,
		Metadata:       refreshMetadata,
	}, nil
}

// InspectTokenPermissions uses Meta's Threads debug_token endpoint. The token
// is supplied as both the same-app tester token and the token being inspected;
// dedicated workspace apps authorize their owning Threads profile directly.
func (a *ThreadsAdapter) InspectTokenPermissions(
	ctx context.Context,
	accessToken, expectedUserID string,
) (ThreadsPermissionSnapshot, error) {
	accessToken = strings.TrimSpace(accessToken)
	expectedUserID = strings.TrimSpace(expectedUserID)
	if accessToken == "" || expectedUserID == "" {
		return ThreadsPermissionSnapshot{}, threadsProviderError("debug_token", "invalid_request", "Threads token identity is missing", false, nil)
	}
	var response struct {
		Data struct {
			Type                string           `json:"type"`
			IsValid             bool             `json:"is_valid"`
			UserID              threadsWebhookID `json:"user_id"`
			Scopes              []string         `json:"scopes"`
			ExpiresAt           int64            `json:"expires_at"`
			DataAccessExpiresAt int64            `json:"data_access_expires_at"`
		} `json:"data"`
	}
	_, err := a.doJSON(ctx, http.MethodGet, "/debug_token", url.Values{
		"access_token": {accessToken},
		"input_token":  {accessToken},
	}, accessToken, &response)
	if err != nil {
		return ThreadsPermissionSnapshot{}, threadsProviderError("debug_token", "permission_check_failed", "Threads permissions could not be verified", true, err)
	}
	if !response.Data.IsValid || !strings.EqualFold(strings.TrimSpace(response.Data.Type), "USER") ||
		strings.TrimSpace(string(response.Data.UserID)) != expectedUserID {
		return ThreadsPermissionSnapshot{}, threadsProviderError("debug_token", "token_invalid", "Threads authorization is invalid or belongs to a different account", false, nil)
	}
	granted := make(map[string]struct{}, len(response.Data.Scopes))
	for _, scope := range response.Data.Scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" {
			granted[scope] = struct{}{}
		}
	}
	var missing []string
	for _, required := range requiredThreadsScopes {
		if _, ok := granted[required]; !ok {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return ThreadsPermissionSnapshot{}, threadsProviderError(
			"debug_token",
			"permissions_missing",
			"Threads authorization is missing required permissions: "+strings.Join(missing, ", "),
			false,
			nil,
		)
	}
	now := a.now().UTC()
	toExpiry := func(unix int64) *time.Time {
		if unix <= 0 {
			return nil
		}
		value := time.Unix(unix, 0).UTC()
		return &value
	}
	expiresAt := toExpiry(response.Data.ExpiresAt)
	dataAccessExpiresAt := toExpiry(response.Data.DataAccessExpiresAt)
	if (expiresAt != nil && !expiresAt.After(now)) ||
		(dataAccessExpiresAt != nil && !dataAccessExpiresAt.After(now)) {
		return ThreadsPermissionSnapshot{}, threadsProviderError("debug_token", "token_expired", "Threads authorization has expired", false, nil)
	}
	verifiedScopes := make([]string, 0, len(granted))
	for scope := range granted {
		verifiedScopes = append(verifiedScopes, scope)
	}
	sort.Strings(verifiedScopes)
	return ThreadsPermissionSnapshot{
		UserID:              expectedUserID,
		Scopes:              verifiedScopes,
		ExpiresAt:           expiresAt,
		DataAccessExpiresAt: dataAccessExpiresAt,
		CheckedAt:           now,
	}, nil
}

func (a *ThreadsAdapter) accessToken(account *models.ChannelAccount) (string, error) {
	if account == nil {
		return "", ErrThreadsCredentialMissing
	}
	now := a.now().UTC()
	for _, index := range CredentialIndexesByPriority(account.Credentials) {
		credential := &account.Credentials[index]
		if credential.Kind != models.ChannelCredentialKindOAuth || !CredentialIsCurrent(credential, now) {
			continue
		}
		stored, _ := credential.CredentialBlob["access_token"].(string)
		stored = strings.TrimSpace(stored)
		if stored == "" {
			continue
		}
		if appcrypto.IsEncrypted(stored) && strings.TrimSpace(a.encryptionKey) == "" {
			return "", threadsProviderError("credential", "encryption_unavailable", "Threads credential encryption is unavailable", false, nil)
		}
		plaintext, err := appcrypto.Decrypt(stored, a.encryptionKey)
		if err != nil || strings.TrimSpace(plaintext) == "" {
			return "", threadsProviderError("credential", "credential_invalid", "Threads credential could not be decrypted", false, err)
		}
		return strings.TrimSpace(plaintext), nil
	}
	return "", ErrThreadsCredentialMissing
}

func (a *ThreadsAdapter) doJSON(
	ctx context.Context,
	method, path string,
	values url.Values,
	accessToken string,
	destination any,
) (http.Header, error) {
	base := strings.TrimRight(strings.TrimSpace(a.apiBaseURL), "/")
	if base == "" {
		base = defaultThreadsAPIBaseURL
	}
	target, err := url.Parse(base + path)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, threadsProviderError("request", "endpoint_invalid", "Threads API endpoint is invalid", false, err)
	}
	encodedValues := values.Encode()
	var requestBody io.Reader
	if method == http.MethodGet || method == http.MethodHead {
		target.RawQuery = encodedValues
	} else if encodedValues != "" {
		requestBody = strings.NewReader(encodedValues)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), requestBody)
	if err != nil {
		return nil, threadsProviderError("request", "request_invalid", "Threads request could not be prepared", false, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("User-Agent", "ReReply-Threads/1.0")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, threadsProviderError("request", "network_error", "Threads API could not be reached", true, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, defaultThreadsResponseSize+1))
	if readErr != nil || int64(len(body)) > defaultThreadsResponseSize {
		return response.Header, threadsProviderError("request", "invalid_response", "Threads returned an invalid response", true, readErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response.Header, threadsHTTPError(response.StatusCode, body)
	}
	if destination == nil || len(body) == 0 {
		return response.Header, nil
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return response.Header, threadsProviderError("request", "invalid_response", "Threads returned malformed JSON", true, err)
	}
	return response.Header, nil
}

func threadsHTTPError(status int, body []byte) error {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	code := strings.TrimSpace(fmt.Sprint(payload.Error.Code))
	if code == "<nil>" || code == "" {
		code = fmt.Sprintf("http_%d", status)
	}
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = fmt.Sprintf("Threads API returned HTTP %d", status)
	}
	retryable := status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	return threadsProviderError("request", code, message, retryable, nil)
}

func threadsProviderError(operation, code, message string, retryable bool, cause error) error {
	return &ProviderError{
		Operation: operation,
		Provider:  ThreadsProvider,
		Code:      code,
		Message:   message,
		Retryable: retryable,
		Cause:     cause,
	}
}

func threadsString(config models.JSONB, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func threadsBool(config models.JSONB, key string) bool {
	value, _ := config[key].(bool)
	return value
}

func firstThreadsHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

type threadsWebhookEnvelope struct {
	AppID          string           `json:"app_id"`
	Topic          string           `json:"topic"`
	TargetID       threadsWebhookID `json:"target_id"`
	Time           int64            `json:"time"`
	SubscriptionID string           `json:"subscription_id"`
	Values         struct {
		Field string              `json:"field"`
		Value threadsWebhookValue `json:"value"`
	} `json:"values"`
}

type threadsWebhookValue struct {
	ID                threadsWebhookID `json:"id"`
	OwnerID           threadsWebhookID `json:"owner_id"`
	Username          string           `json:"username"`
	Text              string           `json:"text"`
	MediaType         string           `json:"media_type"`
	Permalink         string           `json:"permalink"`
	Timestamp         string           `json:"timestamp"`
	ProfilePictureURL string           `json:"profile_picture_url"`
	IsVerified        bool             `json:"is_verified"`
	RepliedTo         struct {
		ID threadsWebhookID `json:"id"`
	} `json:"replied_to"`
	RootPost struct {
		ID       threadsWebhookID `json:"id"`
		OwnerID  threadsWebhookID `json:"owner_id"`
		Username string           `json:"username"`
	} `json:"root_post"`
}

type threadsWebhookID string

func (id *threadsWebhookID) UnmarshalJSON(value []byte) error {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == "null" {
		*id = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if !validThreadsNumericID(text) {
			return errors.New("threads webhook ID is invalid")
		}
		*id = threadsWebhookID(text)
		return nil
	}
	if !validThreadsNumericID(trimmed) {
		return errors.New("threads webhook ID is invalid")
	}
	*id = threadsWebhookID(trimmed)
	return nil
}

func validThreadsNumericID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func decodeThreadsWebhookEnvelopes(body []byte) ([]threadsWebhookEnvelope, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("threads webhook body is empty")
	}
	var envelopes []threadsWebhookEnvelope
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &envelopes); err != nil {
			return nil, errors.New("threads webhook body is invalid")
		}
	} else {
		var envelope threadsWebhookEnvelope
		if err := json.Unmarshal(trimmed, &envelope); err != nil {
			return nil, errors.New("threads webhook body is invalid")
		}
		envelopes = []threadsWebhookEnvelope{envelope}
	}
	if len(envelopes) == 0 {
		return nil, errors.New("threads webhook contains no events")
	}
	return envelopes, nil
}

func parseThreadsWebhookTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05-0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
