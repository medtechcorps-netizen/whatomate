package gmailrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metarelay"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	maxInboundTextRunes = 10_000
	maxRecoveryPages    = 100
)

type GmailSyncAPI interface {
	Profile(context.Context) (GmailProfile, error)
	History(context.Context, string, string) (GmailHistoryPage, error)
	GetMessage(context.Context, string) (GmailRESTMessage, error)
	SearchMessagePage(context.Context, string, int, string) (GmailMessageList, error)
}

type InboundAcceptanceStore interface {
	AcceptInbound(context.Context, string, []metarelay.InboundJob) (bool, error)
}

type SyncerOption func(*Syncer)

func WithSyncerLogger(logger *slog.Logger) SyncerOption {
	return func(syncer *Syncer) {
		if logger != nil {
			syncer.logger = logger
		}
	}
}

type Syncer struct {
	config *Config
	gmail  GmailSyncAPI
	state  SyncStore
	queue  InboundAcceptanceStore
	logger *slog.Logger
	now    func() time.Time
}

func NewSyncer(
	config *Config,
	gmail GmailSyncAPI,
	state SyncStore,
	queue InboundAcceptanceStore,
	options ...SyncerOption,
) (*Syncer, error) {
	if config == nil || gmail == nil || state == nil || queue == nil {
		return nil, errors.New("Gmail syncer configuration is incomplete")
	}
	syncer := &Syncer{
		config: config,
		gmail:  gmail,
		state:  state,
		queue:  queue,
		logger: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		now:    func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		option(syncer)
	}
	return syncer, nil
}

func (s *Syncer) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.config.GmailPollInterval)
	defer ticker.Stop()
	for {
		if err := s.ProcessOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error("Gmail synchronization cycle failed", "component", "gmail_sync")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Syncer) ProcessOnce(ctx context.Context) error {
	if !s.config.Paired() {
		return nil
	}
	leaseToken, err := randomOAuthValue(24)
	if err != nil {
		return errors.New("generate Gmail sync lease")
	}
	leaseTTL := max(2*s.config.HTTPTimeout, s.config.GmailPollInterval+10*time.Second)
	acquired, err := s.state.TryAcquireSyncLease(ctx, leaseToken, leaseTTL)
	if err != nil || !acquired {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if releaseErr := s.state.ReleaseSyncLease(releaseCtx, leaseToken); releaseErr != nil {
			s.logger.Warn("Gmail synchronization lease release failed", "component", "gmail_sync")
		}
	}()

	cycleCtx, cancelCycle := context.WithCancel(ctx)
	heartbeatErrors := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		if heartbeatErr := s.maintainSyncLease(cycleCtx, leaseToken, leaseTTL); heartbeatErr != nil {
			heartbeatErrors <- heartbeatErr
			cancelCycle()
		}
	}()

	syncErr := s.processOnceWithLease(cycleCtx, leaseToken)
	cancelCycle()
	<-heartbeatDone
	select {
	case heartbeatErr := <-heartbeatErrors:
		return heartbeatErr
	default:
		return syncErr
	}
}

func (s *Syncer) processOnceWithLease(ctx context.Context, leaseToken string) error {
	cursor, err := s.state.GetHistoryCursor(ctx)
	if err != nil {
		return err
	}
	if cursor == "" {
		return s.establishBaseline(ctx, leaseToken)
	}
	return s.processHistory(ctx, cursor, leaseToken)
}

func (s *Syncer) maintainSyncLease(ctx context.Context, leaseToken string, leaseTTL time.Duration) error {
	interval := leaseTTL / 3
	if interval <= 0 {
		return errors.New("Gmail sync lease renewal interval is invalid")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			renewTimeout := min(5*time.Second, interval)
			renewCtx, cancelRenew := context.WithTimeout(ctx, renewTimeout)
			renewed, err := s.state.RenewSyncLease(renewCtx, leaseToken, leaseTTL)
			cancelRenew()
			if err != nil {
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					return nil
				}
				return err
			}
			if !renewed {
				return ErrSyncLeaseLost
			}
		}
	}
}

func (s *Syncer) establishBaseline(ctx context.Context, leaseToken string) error {
	profile, err := s.gmail.Profile(ctx)
	if err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(profile.EmailAddress)) != s.config.Mailbox {
		return ErrMailboxMismatch
	}
	return s.commitSuccessfulSync(ctx, leaseToken, profile.HistoryID)
}

func (s *Syncer) processHistory(ctx context.Context, cursor, leaseToken string) error {
	pageToken := ""
	latestCursor := cursor
	seen := make(map[string]GmailMessageRef)
	for {
		page, err := s.gmail.History(ctx, cursor, pageToken)
		if err != nil {
			var apiErr *GmailAPIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
				return s.recoverStaleCursor(ctx, leaseToken)
			}
			return err
		}
		for _, history := range page.History {
			for _, added := range history.MessagesAdded {
				if id := strings.TrimSpace(added.Message.ID); id != "" {
					seen[id] = added.Message
				}
			}
			for _, added := range history.LabelsAdded {
				if !hasGmailLabel(added.LabelIDs, "INBOX") {
					continue
				}
				if id := strings.TrimSpace(added.Message.ID); id != "" {
					seen[id] = added.Message
				}
			}
		}
		if strings.TrimSpace(page.HistoryID) != "" {
			latestCursor = strings.TrimSpace(page.HistoryID)
		}
		pageToken = strings.TrimSpace(page.NextPageToken)
		if pageToken == "" {
			break
		}
	}
	if err := s.processMessageRefs(ctx, mapValues(seen)); err != nil {
		return err
	}
	return s.commitSuccessfulSync(ctx, leaseToken, latestCursor)
}

func (s *Syncer) recoverStaleCursor(ctx context.Context, leaseToken string) error {
	// Capture the new baseline before searching. Messages that arrive after
	// this profile read remain discoverable through history.list next cycle.
	profile, err := s.gmail.Profile(ctx)
	if err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(profile.EmailAddress)) != s.config.Mailbox {
		return ErrMailboxMismatch
	}
	lastSync, err := s.state.GetLastSuccessfulSync(ctx)
	if err != nil {
		return err
	}
	if lastSync.IsZero() {
		lastSync = s.now().UTC().Add(-time.Hour)
	}
	query := "in:inbox after:" + strconv.FormatInt(lastSync.Add(-5*time.Minute).Unix(), 10)
	pageToken := ""
	seen := make(map[string]GmailMessageRef)
	for pageNumber := 0; pageNumber < maxRecoveryPages; pageNumber++ {
		page, pageErr := s.gmail.SearchMessagePage(ctx, query, 100, pageToken)
		if pageErr != nil {
			return pageErr
		}
		for _, message := range page.Messages {
			if id := strings.TrimSpace(message.ID); id != "" {
				seen[id] = message
			}
		}
		pageToken = strings.TrimSpace(page.NextPageToken)
		if pageToken == "" {
			break
		}
		if pageNumber == maxRecoveryPages-1 {
			return errors.New("Gmail recovery result exceeds safety limit")
		}
	}
	if err := s.processMessageRefs(ctx, mapValues(seen)); err != nil {
		return err
	}
	return s.commitSuccessfulSync(ctx, leaseToken, profile.HistoryID)
}

func (s *Syncer) commitSuccessfulSync(ctx context.Context, leaseToken, cursor string) error {
	at := s.now().UTC()
	if fenced, ok := s.state.(fencedSyncStore); ok {
		return fenced.CommitSuccessfulSync(ctx, leaseToken, cursor, at)
	}
	// Small test doubles may implement only the public SyncStore contract. The
	// production RedisStore always takes the fenced atomic path above.
	if err := s.state.SetHistoryCursor(ctx, cursor); err != nil {
		return err
	}
	return s.state.MarkSuccessfulSync(ctx, at)
}

func (s *Syncer) processMessageRefs(ctx context.Context, refs []GmailMessageRef) error {
	messages := make([]GmailRESTMessage, 0, len(refs))
	for _, ref := range refs {
		message, err := s.gmail.GetMessage(ctx, ref.ID)
		if err != nil {
			var apiErr *GmailAPIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
				s.logger.Warn(
					"Skipping Gmail message that disappeared before it could be synchronized",
					"component", "gmail_sync",
					"message_id", ref.ID,
				)
				continue
			}
			return err
		}
		if strings.TrimSpace(message.ID) == "" {
			message.ID = strings.TrimSpace(ref.ID)
		}
		if strings.TrimSpace(message.ThreadID) == "" {
			message.ThreadID = strings.TrimSpace(ref.ThreadID)
		}
		if !isInboundInboxMessage(message, s.config.Mailbox) {
			continue
		}
		messages = append(messages, message)
	}
	sort.SliceStable(messages, func(i, j int) bool {
		left, _ := strconv.ParseInt(messages[i].InternalDate, 10, 64)
		right, _ := strconv.ParseInt(messages[j].InternalDate, 10, 64)
		if left == right {
			return messages[i].ID < messages[j].ID
		}
		return left < right
	})
	for _, message := range messages {
		body, err := s.canonicalBody(message)
		if err != nil {
			s.logger.Warn(
				"Using safe fallback for Gmail message that could not be decoded",
				"component", "gmail_sync",
				"message_id", message.ID,
			)
			body, err = s.fallbackCanonicalBody(message)
			if err != nil {
				return err
			}
		}
		jobID := "gmail:message_added:" + message.ID
		_, err = s.queue.AcceptInbound(ctx, jobID, []metarelay.InboundJob{{
			ID:         jobID,
			AccountKey: s.config.Mailbox,
			Body:       body,
		}})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Syncer) canonicalBody(message GmailRESTMessage) ([]byte, error) {
	parsed, err := ParseGmailMessage(message, MIMEParseLimits{})
	if err != nil {
		return nil, fmt.Errorf("parse Gmail message %s: %w", message.ID, err)
	}
	text := strings.TrimSpace(parsed.Text)
	if text == "" && len(parsed.Attachments) > 0 {
		text = fmt.Sprintf("[Email contains %d attachment(s); attachment viewing is not enabled yet.]", len(parsed.Attachments))
	}
	if text == "" {
		text = "[Email has no readable text.]"
	}
	text = truncateRunes(text, maxInboundTextRunes)
	senderAddress := parsed.ReplyRecipient().Address
	eventKey := "gmail:message_added:" + message.ID
	event := channelapi.InboundEvent{
		DedupeKey:       eventKey,
		ProviderEventID: eventKey,
		Type:            channelapi.NormalizedEventTypeMessage,
		OccurredAt:      parsed.ReceivedAt,
		Message: &channelapi.InboundMessage{
			ExternalMessageID: message.ID,
			Conversation: channelapi.ConversationRef{
				ExternalID: message.ThreadID,
				Subject:    truncateRunes(parsed.Subject, 998),
			},
			Sender: channelapi.Participant{
				ExternalID:  parsed.From.Address,
				Address:     senderAddress,
				DisplayName: parsed.From.Name,
				Role:        models.ConversationParticipantRoleCustomer,
			},
			Direction: models.DirectionIncoming,
			Parts: []channelapi.MessagePart{{
				Type: models.MessagePartTypeText,
				Text: text,
			}},
			SentAt:     parsed.ReceivedAt,
			ReceivedAt: parsed.ReceivedAt,
		},
	}
	return s.marshalCanonicalEnvelope(event)
}

func (s *Syncer) fallbackCanonicalBody(message GmailRESTMessage) ([]byte, error) {
	messageID := truncateRunes(strings.TrimSpace(message.ID), 255)
	if messageID == "" {
		return nil, errors.New("fallback Gmail event has no message ID")
	}
	threadID := truncateRunes(strings.TrimSpace(message.ThreadID), 512)
	if threadID == "" {
		threadID = "gmail-message:" + messageID
	}
	receivedAt := time.Unix(0, 0).UTC()
	if milliseconds, err := strconv.ParseInt(strings.TrimSpace(message.InternalDate), 10, 64); err == nil {
		receivedAt = time.UnixMilli(milliseconds).UTC()
	}
	eventKey := "gmail:message_added:" + message.ID
	event := channelapi.InboundEvent{
		DedupeKey:       eventKey,
		ProviderEventID: eventKey,
		Type:            channelapi.NormalizedEventTypeMessage,
		OccurredAt:      receivedAt,
		Message: &channelapi.InboundMessage{
			ExternalMessageID: messageID,
			Conversation: channelapi.ConversationRef{
				ExternalID: threadID,
				Subject:    "[Unreadable email]",
			},
			Sender: channelapi.Participant{
				ExternalID:  truncateRunes("gmail-unparsed:"+messageID, 255),
				DisplayName: "Unreadable sender",
				Role:        models.ConversationParticipantRoleCustomer,
			},
			Direction: models.DirectionIncoming,
			Parts: []channelapi.MessagePart{{
				Type: models.MessagePartTypeText,
				Text: "[Email could not be decoded safely. Open Gmail to view the original message.]",
			}},
			SentAt:     receivedAt,
			ReceivedAt: receivedAt,
		},
	}
	return s.marshalCanonicalEnvelope(event)
}

func (s *Syncer) marshalCanonicalEnvelope(event channelapi.InboundEvent) ([]byte, error) {
	envelope := struct {
		ExternalAccountID string                    `json:"external_account_id"`
		Events            []channelapi.InboundEvent `json:"events"`
	}{
		ExternalAccountID: s.config.Mailbox,
		Events:            []channelapi.InboundEvent{event},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, errors.New("encode canonical Gmail event")
	}
	if len(body) > channelapi.RelayCanonicalWebhookMaxBodyBytes {
		return nil, errors.New("canonical Gmail event exceeds ReReply body limit")
	}
	return body, nil
}

func isInboundInboxMessage(message GmailRESTMessage, mailbox string) bool {
	hasInbox := false
	for _, label := range message.LabelIDs {
		switch strings.ToUpper(strings.TrimSpace(label)) {
		case "INBOX":
			hasInbox = true
		case "SENT", "DRAFT", "SPAM", "TRASH":
			return false
		}
	}
	if !hasInbox {
		return false
	}
	if message.Payload == nil {
		return true
	}
	from, err := parseMailboxHeader(headerValue(message.Payload.Headers, "From"))
	return err != nil || !strings.EqualFold(from.Address, mailbox)
}

func hasGmailLabel(labels []string, wanted string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), wanted) {
			return true
		}
	}
	return false
}

func mapValues(values map[string]GmailMessageRef) []GmailMessageRef {
	result := make([]GmailMessageRef, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

// discardWriter avoids importing io only to construct a silent default logger.
type discardWriter struct{}

func (discardWriter) Write(value []byte) (int, error) { return len(value), nil }
