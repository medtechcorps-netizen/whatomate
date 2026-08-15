package metarelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
)

type metaWebhook struct {
	Object string      `json:"object"`
	Entry  []metaEntry `json:"entry"`
}

type metaEntry struct {
	ID        string          `json:"id"`
	Time      int64           `json:"time"`
	Messaging []metaMessaging `json:"messaging"`
}

type metaMessaging struct {
	Sender    metaIdentity `json:"sender"`
	Recipient metaIdentity `json:"recipient"`
	Timestamp int64        `json:"timestamp"`
	Message   *metaMessage `json:"message,omitempty"`
}

type metaIdentity struct {
	ID string `json:"id"`
}

type metaMessage struct {
	MID     string       `json:"mid"`
	Text    string       `json:"text"`
	IsEcho  bool         `json:"is_echo"`
	IsSelf  bool         `json:"is_self"`
	ReplyTo *metaReplyTo `json:"reply_to,omitempty"`
}

type metaReplyTo struct {
	MID string `json:"mid"`
}

type canonicalInboundEnvelope struct {
	ExternalAccountID string                    `json:"external_account_id"`
	Events            []channelapi.InboundEvent `json:"events"`
}

// NormalizeInbound turns one signed Meta delivery into canonical, account-
// grouped ReReply jobs. Echoes and unsupported non-text events produce no job,
// but every entry must still resolve to an explicitly configured account bound
// to the webhook app whose route received the delivery.
func NormalizeInbound(config *Config, webhookApp WebhookApp, raw []byte) ([]InboundJob, error) {
	return normalizeInboundWithLimit(
		config,
		webhookApp,
		raw,
		channelapi.RelayCanonicalWebhookMaxBodyBytes,
	)
}

func normalizeInboundWithLimit(
	config *Config,
	webhookApp WebhookApp,
	raw []byte,
	maxCanonicalBodyBytes int,
) ([]InboundJob, error) {
	return normalizeInboundResolvedWithLimit(
		context.Background(), config, nil, webhookApp, raw, maxCanonicalBodyBytes,
	)
}

func normalizeInboundResolvedWithLimit(
	ctx context.Context,
	config *Config,
	resolver RegistryResolver,
	webhookApp WebhookApp,
	raw []byte,
	maxCanonicalBodyBytes int,
) ([]InboundJob, error) {
	if config == nil {
		return nil, errorsWrap(ErrInvalidMetaPayload, "configuration is missing")
	}
	if maxCanonicalBodyBytes <= 0 ||
		maxCanonicalBodyBytes > channelapi.RelayCanonicalWebhookMaxBodyBytes {
		return nil, errorsWrap(ErrInvalidMetaPayload, "canonical body limit is invalid")
	}
	switch webhookApp {
	case WebhookAppMessenger, WebhookAppManagedMessenger, WebhookAppInstagramLogin,
		WebhookAppManagedInstagram:
	default:
		return nil, errorsWrap(ErrInvalidMetaPayload, "webhook app is invalid")
	}
	var payload metaWebhook
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errorsWrap(ErrInvalidMetaPayload, "JSON cannot be decoded")
	}

	var channel models.Channel
	switch strings.ToLower(strings.TrimSpace(payload.Object)) {
	case "page":
		channel = models.ChannelMessenger
	case "instagram":
		channel = models.ChannelInstagram
	default:
		return nil, ErrUnsupportedObject
	}
	if len(payload.Entry) == 0 {
		return nil, errorsWrap(ErrInvalidMetaPayload, "entry is required")
	}

	eventsByAccount := make(map[string][]channelapi.InboundEvent)
	resolvedAccounts := make(map[string]*AccountConfig)
	accountOrder := make([]string, 0, len(payload.Entry))
	seenAccount := make(map[string]bool)

	for entryIndex, entry := range payload.Entry {
		entry.ID = strings.TrimSpace(entry.ID)
		account, ok := config.account(channel, entry.ID)
		if !ok && resolver != nil {
			// Managed Instagram privacy/control-plane fences are evaluated for every
			// provider delivery. A cached lease may outlive a committed deauth,
			// deletion, or quarantine, so this inbound path deliberately bypasses the
			// registry cache. Static lookup remains first and unchanged.
			useCache := webhookApp != WebhookAppManagedInstagram
			resolved, resolveErr := resolver.Resolve(
				ctx, channel, entry.ID, metaregistry.ResolvePurposeInbound, useCache,
			)
			if resolveErr == nil {
				account, ok = resolved, true
			} else if errors.Is(resolveErr, ErrRegistryStale) {
				return nil, fmt.Errorf("%w: registry binding", ErrRegistryStale)
			} else if errors.Is(resolveErr, ErrRegistryUnavailable) {
				return nil, fmt.Errorf("%w: registry lookup", ErrRegistryUnavailable)
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: object=%s entry=%q", ErrUnknownAccount, payload.Object, entry.ID)
		}
		if account.webhookApp() != webhookApp {
			return nil, fmt.Errorf(
				"%w: object=%s entry=%q",
				ErrWebhookAppMismatch,
				payload.Object,
				entry.ID,
			)
		}
		if prior := resolvedAccounts[account.Key]; prior != nil &&
			(prior.Channel != account.Channel || prior.ExternalAccountID != account.ExternalAccountID ||
				registryFence(prior) != registryFence(account)) {
			return nil, fmt.Errorf("%w: account key collision", ErrInvalidMetaPayload)
		}
		resolvedAccounts[account.Key] = account
		if !seenAccount[account.Key] {
			accountOrder = append(accountOrder, account.Key)
			seenAccount[account.Key] = true
		}

		for messageIndex, messaging := range entry.Messaging {
			if messaging.Message == nil {
				continue
			}
			message := messaging.Message
			if message.IsEcho || message.IsSelf {
				continue
			}
			if strings.TrimSpace(message.Text) == "" {
				// Phase 1 intentionally ignores attachments, reactions,
				// postbacks, read receipts, and all other non-text events.
				continue
			}
			mid := strings.TrimSpace(message.MID)
			senderID := strings.TrimSpace(messaging.Sender.ID)
			recipientID := strings.TrimSpace(messaging.Recipient.ID)
			if mid == "" || senderID == "" || recipientID == "" {
				return nil, fmt.Errorf(
					"%w: entry %d messaging %d lacks mid, sender, or recipient",
					ErrInvalidMetaPayload,
					entryIndex,
					messageIndex,
				)
			}
			if recipientID != entry.ID {
				return nil, fmt.Errorf(
					"%w: entry %d messaging %d recipient does not match account",
					ErrInvalidMetaPayload,
					entryIndex,
					messageIndex,
				)
			}
			occurredAt, err := metaTimestamp(messaging.Timestamp, entry.Time)
			if err != nil {
				return nil, fmt.Errorf(
					"%w: entry %d messaging %d has no valid timestamp",
					ErrInvalidMetaPayload,
					entryIndex,
					messageIndex,
				)
			}

			replyTo := ""
			if message.ReplyTo != nil {
				replyTo = strings.TrimSpace(message.ReplyTo.MID)
			}
			dedupeKey := strings.Join([]string{"meta", string(channel), entry.ID, "message", mid}, ":")
			eventsByAccount[account.Key] = append(eventsByAccount[account.Key], channelapi.InboundEvent{
				DedupeKey:       dedupeKey,
				ProviderEventID: mid,
				Type:            channelapi.NormalizedEventTypeMessage,
				OccurredAt:      occurredAt,
				Message: &channelapi.InboundMessage{
					ExternalMessageID: mid,
					Conversation: channelapi.ConversationRef{
						ExternalID: senderID,
					},
					Sender: channelapi.Participant{
						ExternalID: senderID,
						Address:    senderID,
						Role:       models.ConversationParticipantRoleCustomer,
					},
					Direction: models.DirectionIncoming,
					Parts: []channelapi.MessagePart{{
						Type: models.MessagePartTypeText,
						Text: message.Text,
					}},
					ReplyToExternalID: replyTo,
					SentAt:            occurredAt,
					ReceivedAt:        occurredAt,
				},
			})
		}
	}

	jobs := make([]InboundJob, 0, len(eventsByAccount))
	for _, accountKey := range accountOrder {
		events := eventsByAccount[accountKey]
		if len(events) == 0 {
			continue
		}
		account := resolvedAccounts[accountKey]
		if account == nil {
			return nil, fmt.Errorf("%w: account mapping changed during normalization", ErrUnknownAccount)
		}
		accountJobs, err := canonicalInboundJobs(
			account,
			events,
			maxCanonicalBodyBytes,
		)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, accountJobs...)
	}
	return jobs, nil
}

// canonicalInboundJobs deterministically splits one account's ordered events
// into canonical bodies that are guaranteed to fit ReReply's public webhook
// request limit. The safety margin is shared with the receiving adapter so the
// producer and consumer cannot silently drift.
func canonicalInboundJobs(
	account *AccountConfig,
	events []channelapi.InboundEvent,
	maxBodyBytes int,
) ([]InboundJob, error) {
	if account == nil || strings.TrimSpace(account.Key) == "" {
		return nil, errorsWrap(ErrInvalidMetaPayload, "canonical account is missing")
	}
	if len(events) == 0 {
		return nil, nil
	}
	if maxBodyBytes <= 0 ||
		maxBodyBytes > channelapi.RelayCanonicalWebhookMaxBodyBytes {
		return nil, errorsWrap(ErrInvalidMetaPayload, "canonical body limit is invalid")
	}

	externalID, err := json.Marshal(account.ExternalAccountID)
	if err != nil {
		return nil, errorsWrap(ErrInvalidMetaPayload, "canonical account cannot be encoded")
	}
	prefix := make([]byte, 0, len(externalID)+40)
	prefix = append(prefix, `{"external_account_id":`...)
	prefix = append(prefix, externalID...)
	prefix = append(prefix, `,"events":[`...)
	suffix := []byte(`]}`)
	baseSize := len(prefix) + len(suffix)

	eventBodies := make([][]byte, 0, len(events))
	jobs := make([]InboundJob, 0, 1)
	currentSize := baseSize
	flush := func() {
		body := make([]byte, 0, currentSize)
		body = append(body, prefix...)
		for index, eventBody := range eventBodies {
			if index > 0 {
				body = append(body, ',')
			}
			body = append(body, eventBody...)
		}
		body = append(body, suffix...)
		jobs = append(jobs, InboundJob{
			SchemaVersion:     InboundJobSchemaVersion,
			ID:                digestHex(bytes.Join([][]byte{[]byte(account.Key), body}, []byte{'\n'})),
			AccountKey:        account.Key,
			Channel:           account.Channel,
			ExternalAccountID: account.ExternalAccountID,
			RegistryFence:     registryFence(account),
			Body:              body,
		})
		eventBodies = eventBodies[:0]
		currentSize = baseSize
	}

	for index := range events {
		eventBody, marshalErr := json.Marshal(events[index])
		if marshalErr != nil {
			return nil, errorsWrap(ErrInvalidMetaPayload, "canonical event cannot be encoded")
		}
		addedSize := len(eventBody)
		if len(eventBodies) > 0 {
			addedSize++
		}
		if currentSize+addedSize > maxBodyBytes && len(eventBodies) > 0 {
			flush()
			addedSize = len(eventBody)
		}
		if currentSize+addedSize > maxBodyBytes {
			return nil, fmt.Errorf(
				"%w: account=%q event=%d",
				ErrCanonicalJobTooLarge,
				account.Key,
				index,
			)
		}
		eventBodies = append(eventBodies, eventBody)
		currentSize += addedSize
	}
	if len(eventBodies) > 0 {
		flush()
	}
	return jobs, nil
}

func metaTimestamp(messageMilliseconds, entrySeconds int64) (time.Time, error) {
	if messageMilliseconds > 0 {
		return time.UnixMilli(messageMilliseconds).UTC(), nil
	}
	if entrySeconds > 0 {
		return time.Unix(entrySeconds, 0).UTC(), nil
	}
	return time.Time{}, ErrInvalidMetaPayload
}

func digestHex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func errorsWrap(base error, detail string) error {
	return fmt.Errorf("%w: %s", base, detail)
}
