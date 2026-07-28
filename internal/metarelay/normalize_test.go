package metarelay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
)

func TestNormalizeInboundTextAndIgnoresEchoes(t *testing.T) {
	config := newTestConfig(t)
	raw := []byte(`{
	  "object": "page",
	  "entry": [{
	    "id": "page-1",
	    "time": 1770000000,
	    "messaging": [
	      {
	        "sender": {"id": "customer-1"},
	        "recipient": {"id": "page-1"},
	        "timestamp": 1770000000123,
	        "message": {
	          "mid": "mid-1",
	          "text": "Hello relay",
	          "reply_to": {"mid": "prior-mid"}
	        }
	      },
	      {
	        "sender": {"id": "page-1"},
	        "recipient": {"id": "customer-1"},
	        "timestamp": 1770000001123,
	        "message": {"mid": "echo-mid", "text": "echo", "is_echo": true}
	      },
	      {
	        "sender": {"id": "customer-1"},
	        "recipient": {"id": "page-1"},
	        "timestamp": 1770000002123,
	        "message": {"mid": "attachment-mid"}
	      }
	    ]
	  }]
	}`)

	jobs, err := NormalizeInbound(config, WebhookAppMessenger, raw)
	if err != nil {
		t.Fatalf("normalize inbound: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].AccountKey != "messenger-page" || jobs[0].ID == "" {
		t.Fatalf("unexpected job identity: %+v", jobs[0])
	}
	var envelope canonicalInboundEnvelope
	if err := json.Unmarshal(jobs[0].Body, &envelope); err != nil {
		t.Fatalf("decode canonical envelope: %v", err)
	}
	if envelope.ExternalAccountID != "page-1" || len(envelope.Events) != 1 {
		t.Fatalf("unexpected canonical envelope: %+v", envelope)
	}
	event := envelope.Events[0]
	if event.ProviderEventID != "mid-1" ||
		event.Message == nil ||
		event.Message.Parts[0].Text != "Hello relay" ||
		event.Message.ReplyToExternalID != "prior-mid" {
		t.Fatalf("unexpected normalized event: %+v", event)
	}
	wantTime := time.UnixMilli(1770000000123).UTC()
	if !event.OccurredAt.Equal(wantTime) || !event.Message.SentAt.Equal(wantTime) {
		t.Fatalf("unexpected event time: %s", event.OccurredAt)
	}
}

func TestNormalizeInboundKnownEchoOnlyIsNoOp(t *testing.T) {
	config := newTestConfig(t)
	raw := []byte(`{
	  "object": "instagram",
	  "entry": [{
	    "id": "ig-direct-1",
	    "time": 1770000000,
	    "messaging": [{
	      "sender": {"id": "ig-direct-1"},
	      "recipient": {"id": "customer-1"},
	      "timestamp": 1770000000123,
	      "message": {"mid": "echo-mid", "text": "echo", "is_self": true}
	    }]
	  }]
	}`)
	jobs, err := NormalizeInbound(config, WebhookAppInstagramLogin, raw)
	if err != nil {
		t.Fatalf("normalize echo: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("echo generated %d jobs", len(jobs))
	}
}

func TestNormalizeInboundRejectsUnknownAndWrongWebhookApp(t *testing.T) {
	config := newTestConfig(t)

	unknown := []byte(`{
	  "object": "page",
	  "entry": [{"id": "unconfigured-page", "time": 1770000000, "messaging": []}]
	}`)
	if _, err := NormalizeInbound(config, WebhookAppMessenger, unknown); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("got %v, want ErrUnknownAccount", err)
	}

	instagramDirect := []byte(`{
	  "object": "instagram",
	  "entry": [{"id": "ig-direct-1", "time": 1770000000, "messaging": []}]
	}`)
	if _, err := NormalizeInbound(
		config,
		WebhookAppMessenger,
		instagramDirect,
	); !errors.Is(err, ErrWebhookAppMismatch) {
		t.Fatalf("Instagram Login account accepted on parent app: %v", err)
	}
	if _, err := NormalizeInbound(
		config,
		WebhookAppInstagramLogin,
		instagramDirect,
	); err != nil {
		t.Fatalf("Instagram Login account rejected on its app: %v", err)
	}

	instagramPage := []byte(`{
	  "object": "instagram",
	  "entry": [{"id": "ig-page-1", "time": 1770000000, "messaging": []}]
	}`)
	if _, err := NormalizeInbound(
		config,
		WebhookAppInstagramLogin,
		instagramPage,
	); !errors.Is(err, ErrWebhookAppMismatch) {
		t.Fatalf("Facebook Login account accepted on Instagram app: %v", err)
	}
	if _, err := NormalizeInbound(
		config,
		WebhookAppMessenger,
		instagramPage,
	); err != nil {
		t.Fatalf("Facebook Login Instagram account rejected on parent app: %v", err)
	}
}

func TestNormalizeInboundRejectsMismatchedRecipient(t *testing.T) {
	config := newTestConfig(t)
	raw := []byte(`{
	  "object": "page",
	  "entry": [{
	    "id": "page-1",
	    "time": 1770000000,
	    "messaging": [{
	      "sender": {"id": "customer-1"},
	      "recipient": {"id": "another-page"},
	      "timestamp": 1770000000123,
	      "message": {"mid": "mid-1", "text": "hello"}
	    }]
	  }]
	}`)
	if _, err := NormalizeInbound(
		config,
		WebhookAppMessenger,
		raw,
	); !errors.Is(err, ErrInvalidMetaPayload) {
		t.Fatalf("got %v, want ErrInvalidMetaPayload", err)
	}
}

func TestNormalizeInboundSplitsDeterministicallyWithinCanonicalLimit(t *testing.T) {
	config := newTestConfig(t)
	singleRaw := testMetaTextWebhookBody(t, 1, "bounded message")
	single, err := NormalizeInbound(config, WebhookAppMessenger, singleRaw)
	if err != nil || len(single) != 1 {
		t.Fatalf("normalize single event: jobs=%d err=%v", len(single), err)
	}
	limit := len(single[0].Body) + 16

	const eventCount = 8
	raw := testMetaTextWebhookBody(t, eventCount, "bounded message")
	first, err := normalizeInboundWithLimit(config, WebhookAppMessenger, raw, limit)
	if err != nil {
		t.Fatalf("normalize split batch: %v", err)
	}
	second, err := normalizeInboundWithLimit(config, WebhookAppMessenger, raw, limit)
	if err != nil {
		t.Fatalf("normalize repeated split batch: %v", err)
	}
	if len(first) <= 1 || len(first) != len(second) {
		t.Fatalf("split job counts = %d/%d, want matching multi-job batches", len(first), len(second))
	}

	var providerEventIDs []string
	for index := range first {
		if len(first[index].Body) > limit ||
			len(first[index].Body) > channelapi.RelayCanonicalWebhookMaxBodyBytes {
			t.Fatalf("job %d has %d bytes, limit %d", index, len(first[index].Body), limit)
		}
		if first[index].ID != second[index].ID ||
			!bytes.Equal(first[index].Body, second[index].Body) {
			t.Fatalf("job %d changed across identical normalization", index)
		}
		var envelope canonicalInboundEnvelope
		if err := json.Unmarshal(first[index].Body, &envelope); err != nil {
			t.Fatalf("decode job %d: %v", index, err)
		}
		for _, event := range envelope.Events {
			providerEventIDs = append(providerEventIDs, event.ProviderEventID)
		}
	}
	if len(providerEventIDs) != eventCount {
		t.Fatalf("got %d events after splitting, want %d", len(providerEventIDs), eventCount)
	}
	for index, eventID := range providerEventIDs {
		want := fmt.Sprintf("mid-%04d", index)
		if eventID != want {
			t.Fatalf("event %d = %q, want %q", index, eventID, want)
		}
	}
}

func TestNormalizeInboundRejectsSingleCanonicalEventAboveLimit(t *testing.T) {
	config := newTestConfig(t)
	raw := testMetaTextWebhookBody(t, 1, "single event")
	baseline, err := NormalizeInbound(config, WebhookAppMessenger, raw)
	if err != nil || len(baseline) != 1 {
		t.Fatalf("normalize baseline: jobs=%d err=%v", len(baseline), err)
	}
	_, err = normalizeInboundWithLimit(
		config,
		WebhookAppMessenger,
		raw,
		len(baseline[0].Body)-1,
	)
	if !errors.Is(err, ErrCanonicalJobTooLarge) {
		t.Fatalf("got %v, want ErrCanonicalJobTooLarge", err)
	}
}

func TestNormalizeInboundProductionLimitSplitsRawBodyAcceptedByWebhook(t *testing.T) {
	config := newTestConfig(t)
	const eventCount = 6000
	raw := testMetaTextWebhookBody(t, eventCount, "hello")
	if len(raw) > int(defaultInboundBodyLimit) {
		t.Fatalf(
			"test raw body is %d bytes and no longer exercises the accepted webhook range %d",
			len(raw),
			defaultInboundBodyLimit,
		)
	}
	jobs, err := NormalizeInbound(config, WebhookAppMessenger, raw)
	if err != nil {
		t.Fatalf("normalize production-sized batch: %v", err)
	}
	if len(jobs) <= 1 {
		t.Fatalf("got %d jobs, want a split canonical batch", len(jobs))
	}
	totalEvents := 0
	for index, job := range jobs {
		if len(job.Body) > channelapi.RelayCanonicalWebhookMaxBodyBytes {
			t.Fatalf(
				"job %d has %d bytes, canonical maximum is %d",
				index,
				len(job.Body),
				channelapi.RelayCanonicalWebhookMaxBodyBytes,
			)
		}
		var envelope canonicalInboundEnvelope
		if err := json.Unmarshal(job.Body, &envelope); err != nil {
			t.Fatalf("decode job %d: %v", index, err)
		}
		for eventIndex, event := range envelope.Events {
			want := fmt.Sprintf("mid-%04d", totalEvents+eventIndex)
			if event.ProviderEventID != want {
				t.Fatalf(
					"job %d event %d = %q, want %q",
					index,
					eventIndex,
					event.ProviderEventID,
					want,
				)
			}
		}
		totalEvents += len(envelope.Events)
	}
	if totalEvents != eventCount {
		t.Fatalf("got %d canonical events, want %d", totalEvents, eventCount)
	}
}

func testMetaTextWebhookBody(t *testing.T, count int, text string) []byte {
	t.Helper()
	messaging := make([]metaMessaging, count)
	for index := range messaging {
		messaging[index] = metaMessaging{
			Sender:    metaIdentity{ID: "customer-1"},
			Recipient: metaIdentity{ID: "page-1"},
			Timestamp: 1770000000123 + int64(index),
			Message: &metaMessage{
				MID:  fmt.Sprintf("mid-%04d", index),
				Text: text,
			},
		}
	}
	raw, err := json.Marshal(metaWebhook{
		Object: "page",
		Entry: []metaEntry{{
			ID:        "page-1",
			Time:      1770000000,
			Messaging: messaging,
		}},
	})
	if err != nil {
		t.Fatalf("encode Meta webhook: %v", err)
	}
	return raw
}
