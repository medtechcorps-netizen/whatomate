package gmailrelay

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
)

type GmailOutboundAPI interface {
	GetThread(context.Context, string) (GmailThread, error)
	SearchMessages(context.Context, string, int) (GmailMessageList, error)
	SendRaw(context.Context, string, []byte) (GmailSendResponse, error)
}

type outboundDelivery struct {
	response   GmailSendResponse
	messageID  string
	reconciled bool
}

type outboundDeliveryError struct {
	err           error
	sendAttempted bool
}

func (e *outboundDeliveryError) Error() string {
	if e == nil || e.err == nil {
		return "Gmail outbound delivery failed"
	}
	return e.err.Error()
}

func (e *outboundDeliveryError) Unwrap() error { return e.err }

type threadReplyContext struct {
	threadID   string
	recipient  Mailbox
	subject    string
	inReplyTo  string
	references []string
}

func deliverOutboundReply(
	ctx context.Context,
	config *Config,
	gmail GmailOutboundAPI,
	message channelapi.OutboundMessage,
	now time.Time,
) (outboundDelivery, error) {
	replyContext, err := resolveThreadReplyContext(ctx, config, gmail, message)
	if err != nil {
		return outboundDelivery{}, &outboundDeliveryError{err: err}
	}
	messageID, err := outboundMessageID(config.Mailbox, message.IdempotencyKey)
	if err != nil {
		return outboundDelivery{}, &outboundDeliveryError{err: err}
	}
	if found, err := findSentMessage(ctx, gmail, replyContext.threadID, messageID); err != nil {
		return outboundDelivery{}, &outboundDeliveryError{err: err}
	} else if found.ID != "" {
		return outboundDelivery{response: found, messageID: messageID, reconciled: true}, nil
	}

	_, domain, _ := strings.Cut(config.Mailbox, "@")
	raw, err := BuildGmailReply(ReplyOptions{
		From:            Mailbox{Address: config.Mailbox},
		To:              []Mailbox{replyContext.recipient},
		Subject:         replyContext.subject,
		Date:            now.UTC(),
		MessageID:       messageID,
		MessageIDDomain: domain,
		InReplyTo:       replyContext.inReplyTo,
		References:      replyContext.references,
		Text:            message.Parts[0].Text,
	})
	if err != nil {
		return outboundDelivery{}, &outboundDeliveryError{err: err}
	}
	response, err := gmail.SendRaw(ctx, replyContext.threadID, raw)
	if err != nil {
		return outboundDelivery{}, &outboundDeliveryError{err: err, sendAttempted: true}
	}
	return outboundDelivery{response: response, messageID: messageID}, nil
}

func reconcileOutboundReply(
	ctx context.Context,
	config *Config,
	gmail GmailOutboundAPI,
	message channelapi.OutboundMessage,
) (GmailSendResponse, bool, error) {
	messageID, err := outboundMessageID(config.Mailbox, message.IdempotencyKey)
	if err != nil {
		return GmailSendResponse{}, false, err
	}
	found, err := findSentMessage(ctx, gmail, message.Conversation.ExternalID, messageID)
	return found, found.ID != "", err
}

func outboundMessageID(mailbox, idempotencyKey string) (string, error) {
	_, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(mailbox)), "@")
	if !ok {
		return "", errors.New("configured Gmail mailbox is invalid")
	}
	return DeterministicMessageID(mailbox+"\n"+idempotencyKey, domain)
}

func findSentMessage(
	ctx context.Context,
	gmail GmailOutboundAPI,
	expectedThreadID, rfcMessageID string,
) (GmailSendResponse, error) {
	searchID := strings.Trim(strings.TrimSpace(rfcMessageID), "<>")
	result, err := gmail.SearchMessages(ctx, "in:sent rfc822msgid:"+searchID, 10)
	if err != nil {
		return GmailSendResponse{}, err
	}
	for _, found := range result.Messages {
		if found.ThreadID == expectedThreadID {
			return GmailSendResponse(found), nil
		}
	}
	if len(result.Messages) > 0 {
		return GmailSendResponse{}, errors.New("deterministic Gmail Message-ID belongs to a different thread")
	}
	return GmailSendResponse{}, nil
}

func resolveThreadReplyContext(
	ctx context.Context,
	config *Config,
	gmail GmailOutboundAPI,
	message channelapi.OutboundMessage,
) (threadReplyContext, error) {
	threadID := strings.TrimSpace(message.Conversation.ExternalID)
	thread, err := gmail.GetThread(ctx, threadID)
	if err != nil {
		return threadReplyContext{}, err
	}
	if strings.TrimSpace(thread.ID) != threadID {
		return threadReplyContext{}, errors.New("gmail returned a different thread")
	}

	type parsedThreadMessage struct {
		raw      GmailRESTMessage
		parsed   ParsedGmailMessage
		internal int64
	}
	parsedMessages := make([]parsedThreadMessage, 0, len(thread.Messages))
	for _, raw := range thread.Messages {
		if hasAnyLabel(raw.LabelIDs, "DRAFT", "TRASH", "SPAM") {
			continue
		}
		parsed, parseErr := ParseGmailMessage(raw, MIMEParseLimits{})
		if parseErr != nil {
			continue
		}
		internal, _ := strconv.ParseInt(raw.InternalDate, 10, 64)
		parsedMessages = append(parsedMessages, parsedThreadMessage{raw: raw, parsed: parsed, internal: internal})
	}
	if len(parsedMessages) == 0 {
		return threadReplyContext{}, errors.New("gmail thread has no readable messages")
	}
	sort.SliceStable(parsedMessages, func(i, j int) bool {
		if parsedMessages[i].internal == parsedMessages[j].internal {
			return parsedMessages[i].raw.ID < parsedMessages[j].raw.ID
		}
		return parsedMessages[i].internal < parsedMessages[j].internal
	})

	target := &parsedMessages[len(parsedMessages)-1]
	if replyID := strings.TrimSpace(message.ReplyToExternalID); replyID != "" {
		target = nil
		for index := range parsedMessages {
			if parsedMessages[index].raw.ID == replyID {
				target = &parsedMessages[index]
				break
			}
		}
		if target == nil {
			return threadReplyContext{}, errors.New("reply target does not belong to the Gmail thread")
		}
	}
	if strings.TrimSpace(target.parsed.MessageID) == "" {
		return threadReplyContext{}, errors.New("gmail reply target has no RFC Message-ID")
	}

	var latestCustomer *parsedThreadMessage
	for index := range parsedMessages {
		candidate := &parsedMessages[index]
		if !strings.EqualFold(candidate.parsed.From.Address, config.Mailbox) {
			latestCustomer = candidate
		}
	}
	if latestCustomer == nil {
		return threadReplyContext{}, errors.New("gmail thread has no customer sender")
	}
	recipient := latestCustomer.parsed.ReplyRecipient()
	requested, err := normalizeRecipient(message.Recipient)
	if err != nil {
		return threadReplyContext{}, err
	}
	if !strings.EqualFold(requested.Address, recipient.Address) {
		return threadReplyContext{}, errors.New("outbound recipient does not match the Gmail thread")
	}

	subject := strings.TrimSpace(target.parsed.Subject)
	if subject == "" {
		for index := len(parsedMessages) - 1; index >= 0; index-- {
			if value := strings.TrimSpace(parsedMessages[index].parsed.Subject); value != "" {
				subject = value
				break
			}
		}
	}
	if subject == "" {
		return threadReplyContext{}, errors.New("gmail thread has no subject")
	}
	references := append([]string(nil), target.parsed.References...)
	references = append(references, target.parsed.MessageID)
	return threadReplyContext{
		threadID:   threadID,
		recipient:  recipient,
		subject:    subject,
		inReplyTo:  target.parsed.MessageID,
		references: references,
	}, nil
}

func hasAnyLabel(labels []string, blocked ...string) bool {
	for _, label := range labels {
		for _, value := range blocked {
			if strings.EqualFold(strings.TrimSpace(label), value) {
				return true
			}
		}
	}
	return false
}

func normalizeRecipient(participant channelapi.Participant) (Mailbox, error) {
	value := strings.TrimSpace(participant.Address)
	if value == "" {
		value = strings.TrimSpace(participant.ExternalID)
	}
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return Mailbox{}, errors.New("outbound recipient address is required")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address == "" {
		return Mailbox{}, fmt.Errorf("outbound recipient address is invalid")
	}
	return Mailbox{Name: strings.TrimSpace(participant.DisplayName), Address: strings.ToLower(parsed.Address)}, nil
}
