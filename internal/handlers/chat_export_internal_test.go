package handlers

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestChatExportPureResponseCloseReleasesArchiveOnce(t *testing.T) {
	archive, err := newChatExportArchive(time.Now(), chatExportRequest{Scope: "all"})
	require.NoError(t, err)
	path, spool := archive.File.Name(), archive.JSONL.Name()
	var releases atomic.Int32
	response := &chatExportDownload{Reader: archive.File, archive: archive, cleanup: func() { releases.Add(1) }}
	response.expireAfter(time.Hour)
	require.NoError(t, response.Close())
	require.NoError(t, response.Close())
	require.Equal(t, int32(1), releases.Load())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(spool)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestChatExportPureResponseExpiryRemovesRetainedArchive(t *testing.T) {
	archive, err := newChatExportArchive(time.Now(), chatExportRequest{Scope: "all"})
	require.NoError(t, err)
	released := make(chan struct{})
	response := &chatExportDownload{Reader: archive.File, archive: archive, cleanup: func() { close(released) }}
	response.expireAfter(time.Millisecond)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("stream expiry did not release its archive")
	}
	_, err = os.Stat(archive.File.Name())
	require.ErrorIs(t, err, os.ErrNotExist)
}

// The real Redis fixture is explicitly scheduled separately from Pure tests.
// It touches only unique owner-bound control keys and never flushes a database.
func TestChatExportRedisCancellationWinsFinalPreparationRace(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL required for the real Redis transition test")
	}
	options, err := redis.ParseURL(url)
	require.NoError(t, err)
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	key := chatExportJobKey(uuid.New(), uuid.New(), uuid.New())
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = client.Del(cleanup, key).Err()
	})
	require.NoError(t, client.Set(ctx, key, "running", time.Minute).Err())
	// Reauthorization has completed. Cancellation arrives before the final
	// delivery claim, deliberately without waiting for the 500 ms watcher.
	accepted, err := client.Eval(ctx, chatExportCancelScript, []string{key}).Int()
	require.NoError(t, err)
	require.Equal(t, 1, accepted)
	claimed, err := client.Eval(ctx, chatExportTransitionScript, []string{key}, "running", "delivering").Int()
	require.NoError(t, err)
	require.Zero(t, claimed)
	_, err = client.Eval(ctx, chatExportTransitionScript, []string{key}, "running", "finished").Result()
	require.NoError(t, err)
	state, err := client.Get(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "cancelled", state)
	// The inverse ordering truthfully declines cancellation once bytes belong
	// to the response, and cleanup cannot overwrite that delivery state.
	require.NoError(t, client.Set(ctx, key, "running", time.Minute).Err())
	claimed, err = client.Eval(ctx, chatExportTransitionScript, []string{key}, "running", "delivering").Int()
	require.NoError(t, err)
	require.Equal(t, 1, claimed)
	accepted, err = client.Eval(ctx, chatExportCancelScript, []string{key}).Int()
	require.NoError(t, err)
	require.Zero(t, accepted)
	_, err = client.Eval(ctx, chatExportTransitionScript, []string{key}, "running", "finished").Result()
	require.NoError(t, err)
	state, err = client.Get(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "delivering", state)
}

func TestChatExportPureScopeValidation(t *testing.T) {
	key := "whatsapp:" + uuid.NewString() + ":6163636f756e74"
	tests := []struct {
		name  string
		req   chatExportRequest
		valid bool
	}{
		{"all", chatExportRequest{Scope: "all"}, true},
		{"selected", chatExportRequest{Scope: "selected", Selected: []string{key}}, true},
		{"empty selection", chatExportRequest{Scope: "selected"}, false},
		{"duplicate", chatExportRequest{Scope: "selected", Selected: []string{key, key}}, false},
		{"forged key", chatExportRequest{Scope: "selected", Selected: []string{"whatsapp:x:../secrets"}}, false},
		{"invalid scope", chatExportRequest{Scope: "loaded_page"}, false},
		{"invalid account", chatExportRequest{Scope: "all", Account: "channel:other"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.req.validate()
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	req := chatExportRequest{Scope: "all", Search: "old", Tags: []string{"old"}, Unread: true, Assigned: "me", Account: "whatsapp:6163636f756e74"}
	require.NoError(t, req.validate())
	require.Empty(t, req.Search)
	require.Empty(t, req.Tags)
	require.False(t, req.Unread)
	require.Empty(t, req.Assigned)
	require.NotEmpty(t, req.Account)
}

func TestChatExportPureDeliveryLabels(t *testing.T) {
	for _, test := range []struct {
		row  chatExportRow
		want string
	}{
		{chatExportRow{Direction: "incoming", Status: "read"}, "received"},
		{chatExportRow{Direction: "outgoing", Status: "failed", Forwarded: true}, "failed"},
		{chatExportRow{Direction: "outgoing", Status: "pending", ForwardPhase: "provider"}, "unconfirmed: may have been sent"},
		{chatExportRow{Direction: "outgoing", Status: "pending", ForwardPhase: "queued"}, "queued: delivery not confirmed"},
		{chatExportRow{Direction: "outgoing", Status: "pending", AIPhase: "uncertain"}, "unconfirmed: may have been sent"},
		{chatExportRow{Direction: "outgoing", Status: "pending"}, "pending: delivery not confirmed"},
		{chatExportRow{Direction: "outgoing", Status: "delivered"}, "delivered"},
	} {
		require.Equal(t, test.want, chatExportDelivery(test.row))
	}
}

func TestChatExportPureLegacyIngestionTimeIsNotInvented(t *testing.T) {
	at := time.Now().UTC()
	record := chatExportRecord(chatExportRow{CreatedAt: at, EffectiveIngestedAt: at}, nil)
	require.Nil(t, record.IngestedAt)
	require.Equal(t, "unavailable_provider_time_used_for_ordering", record.IngestionTimeSource)
	record = chatExportRecord(chatExportRow{CreatedAt: at, EffectiveIngestedAt: at, IngestedAt: &at}, nil)
	require.NotNil(t, record.IngestedAt)
	require.Equal(t, "stored", record.IngestionTimeSource)
	require.True(t, record.IngestedAt.Equal(at))
}

func TestChatExportPureArchiveCompleteAndRedacted(t *testing.T) {
	snapshot := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	archive, err := newChatExportArchive(snapshot, chatExportRequest{Scope: "all"})
	require.NoError(t, err)
	defer archive.cleanup()
	contact := uuid.New()
	ids := map[uuid.UUID]bool{}
	for i := 0; i < 245; i++ {
		id := uuid.New()
		ids[id] = true
		row := chatExportRow{ID: id, ContactID: contact, GroupKey: "inbox:" + contact.String(), AccountKey: "channel:" + contact.String(), ContactName: "../../patient", AccountName: "Clinical inbox", Channel: "email", Direction: "outgoing", Status: "pending", CreatedAt: snapshot, EffectiveIngestedAt: snapshot, Content: "Hello 世界\nSecond line", TemplateName: "welcome", ForwardPhase: "provider"}
		if i >= 121 {
			row.GroupKey = "whatsapp:" + contact.String() + ":6163636f756e74"
		}
		parts := []models.MessagePart{{Type: models.MessagePartTypeDocument, Filename: "../report.pdf", MimeType: "application/pdf", Caption: "Résumé", MediaURL: "https://private.example/a?signature=SECRET", StorageKey: "tenant/private/key", Payload: models.JSONB{"access_token": "SECRET"}}}
		require.NoError(t, archive.add(row, parts, false))
	}
	require.NoError(t, archive.finish())
	reader, err := zip.OpenReader(archive.File.Name())
	require.NoError(t, err)
	defer reader.Close()
	var manifest chatExportManifest
	var records []chatExportMessage
	transcripts := 0
	for _, file := range reader.File {
		require.NotContains(t, file.Name, "..")
		content, err := file.Open()
		require.NoError(t, err)
		bytes, err := io.ReadAll(content)
		require.NoError(t, err)
		require.NoError(t, content.Close())
		require.NotContains(t, string(bytes), "SECRET")
		require.NotContains(t, string(bytes), "tenant/private/key")
		require.NotContains(t, string(bytes), "signature=")
		switch file.Name {
		case "manifest.json":
			require.NoError(t, json.Unmarshal(bytes, &manifest))
		case "messages.jsonl":
			for _, line := range strings.Split(strings.TrimSpace(string(bytes)), "\n") {
				var record chatExportMessage
				require.NoError(t, json.Unmarshal([]byte(line), &record))
				records = append(records, record)
			}
		default:
			transcripts++
			require.Contains(t, string(bytes), "Hello 世界\nSecond line")
			require.Contains(t, string(bytes), "may have been sent")
		}
	}
	require.True(t, manifest.Complete)
	require.Equal(t, 245, manifest.Messages)
	require.Equal(t, 2, manifest.Conversations)
	require.Equal(t, 2, transcripts)
	require.Len(t, records, 245)
	for _, record := range records {
		require.True(t, ids[record.ID])
		delete(ids, record.ID)
		require.True(t, record.Attachments[0].AttachmentPresent)
	}
	require.Empty(t, ids)
}

func TestChatExportPureLimitAndCleanup(t *testing.T) {
	archive, err := newChatExportArchive(time.Now(), chatExportRequest{Scope: "all"})
	require.NoError(t, err)
	path := archive.File.Name()
	spool := archive.JSONL.Name()
	archive.Bytes = chatExportMaxBytes
	err = archive.add(chatExportRow{ID: uuid.New(), ContactID: uuid.New(), GroupKey: "test", Content: "one more byte"}, nil, false)
	require.ErrorIs(t, err, errChatExportTooLarge)
	require.False(t, archive.Manifest.Complete)
	archive.cleanup()
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(spool)
	require.True(t, os.IsNotExist(err))
}

func TestChatExportPureCancelledBeforeReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var app App
	require.ErrorIs(t, app.buildChatExport(ctx, uuid.New(), uuid.New(), chatExportRequest{}, chatExportAccess{}, nil), context.Canceled)
}

func TestChatExportPureQueryFencesAndKeyset(t *testing.T) {
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: "host=127.0.0.1 user=unused dbname=unused", PreferSimpleProtocol: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	require.NoError(t, err)
	app := App{DB: db}
	org, user := uuid.New(), uuid.New()
	key := "inbox:" + uuid.NewString()
	req := chatExportRequest{Scope: "selected", Selected: []string{key}, Account: "channel:" + uuid.NewString()}
	q := app.chatExportQuery(org, user, req, chatExportAccess{CurrentConversationOnly: true}).Select(chatExportRowSelect).Where("("+chatExportGroupSQL+", COALESCE(m.ingested_at,m.created_at), m.id) > (?, ?, ?)", key, time.Now(), uuid.New()).Order(chatExportGroupSQL + ", COALESCE(m.ingested_at,m.created_at), m.id").Limit(chatExportBatchSize).Find(&[]chatExportRow{})
	require.NoError(t, q.Error)
	statement := q.Statement.SQL.String()
	for _, needle := range []string{"c.organization_id = m.organization_id", "ic.contact_id = m.contact_id", "ca.organization_id = m.organization_id", "m.organization_id =", "m.deleted_at IS NULL", "atx.agent_id =", "m.created_at >= GREATEST", "m.id) >", "LIMIT"} {
		require.Contains(t, statement, needle)
	}
	require.NotContains(t, strings.ToUpper(statement), " OFFSET ")
	require.NotContains(t, statement, "m.*")
	require.NotContains(t, statement, "m.metadata,")
	require.Contains(t, statement, "meta_legacy")
}
