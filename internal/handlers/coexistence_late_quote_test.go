package handlers

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestCoexistenceLateQuoteFromSignedEcho(t *testing.T) {
	app := webhookTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, func(account *models.WhatsAppAccount) {
		account.IsSMB = true
	})
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	replyAt := base.Add(2 * time.Minute)
	originalWAMID := "wamid.synthetic-quote-original-" + uuid.NewString()
	replyWAMID := "wamid.synthetic-quote-reply-" + uuid.NewString()
	otherWAMID := "wamid.synthetic-quote-unrelated-" + uuid.NewString()
	const originalText = "Synthetic original arrives late"
	const replyText = "Synthetic reply text"
	const otherText = "Synthetic unrelated message"
	sendEcho := func(wamid, content, target string, occurredAt time.Time) {
		t.Helper()
		event := map[string]any{
			"id": wamid, "type": "text", "timestamp": strconv.FormatInt(occurredAt.Unix(), 10),
			"from": "15550101001", "to": contact.PhoneNumber,
			"text": map[string]string{"body": content},
		}
		if target != "" {
			event["context"] = map[string]string{"id": target}
		}
		body, err := json.Marshal(map[string]any{
			"object": "whatsapp_business_account",
			"entry": []any{map[string]any{"id": account.BusinessID, "changes": []any{map[string]any{
				"field": "smb_message_echoes",
				"value": map[string]any{
					"metadata":       map[string]string{"phone_number_id": account.PhoneID, "display_phone_number": "15550101001"},
					"message_echoes": []any{event},
				},
			}}}},
		})
		require.NoError(t, err)
		sendSignedWebhook(t, app, body)
		app.WaitForBackgroundTasks()
	}

	sendEcho(replyWAMID, replyText, originalWAMID, replyAt)
	reply := requireSingleWAMIDMessage(t, app, *account, replyWAMID)
	require.Equal(t, contact.ID, reply.ContactID)
	require.Nil(t, reply.ReplyToMessageID)
	require.Zero(t, countWAMIDMessages(t, app, *account, originalWAMID))
	pending, ok := reply.Metadata[models.MessageCoexistencePendingQuoteMetadataKey].(string)
	require.True(t, ok, "the missing target must leave an encrypted pending reference")
	require.True(t, appcrypto.IsEncrypted(pending))
	require.NotContains(t, pending, originalWAMID)
	key := app.coexistenceQuoteEncryptionKey()
	_, opened := openPendingCoexistenceQuote(&reply, key)
	require.True(t, opened)
	wrongRow := reply
	wrongRow.ID = uuid.New()
	_, opened = openPendingCoexistenceQuote(&wrongRow, key)
	require.False(t, opened, "a ciphertext copied to a different row cannot select an original")
	_, opened = openPendingCoexistenceQuote(&reply, "")
	require.False(t, opened, "an empty key never enables plaintext fallback")
	plain := reply
	plain.Metadata = cloneMessageMetadata(reply.Metadata)
	plain.Metadata[models.MessageCoexistencePendingQuoteMetadataKey] = originalWAMID
	_, opened = openPendingCoexistenceQuote(&plain, key)
	require.False(t, opened, "unencrypted values are not quote references")

	publicJSON, err := json.Marshal(reply)
	require.NoError(t, err)
	require.Contains(t, string(publicJSON), replyText)
	require.NotContains(t, string(publicJSON), models.MessageCoexistencePendingQuoteMetadataKey)
	require.NotContains(t, string(publicJSON), pending)
	require.NotContains(t, string(publicJSON), originalWAMID)

	readTranscript := func(userID, beforeID uuid.UUID) (MessageResponse, string) {
		t.Helper()
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, userID)
		testutil.SetPathParam(req, "id", contact.ID.String())
		testutil.SetQueryParam(req, "account", account.Name)
		testutil.SetQueryParam(req, "acknowledge", "false")
		if beforeID != uuid.Nil {
			testutil.SetQueryParam(req, "before_id", beforeID.String())
		}
		require.NoError(t, app.WithCommittedTenantApp(org.ID, func(scoped *App) error {
			var beforeTimeout, afterTimeout string
			if err := scoped.DB.Raw("SHOW statement_timeout").Scan(&beforeTimeout).Error; err != nil {
				return err
			}
			if err := scoped.GetMessages(req); err != nil {
				return err
			}
			if err := scoped.DB.Raw("SHOW statement_timeout").Scan(&afterTimeout).Error; err != nil {
				return err
			}
			require.Equal(t, beforeTimeout, afterTimeout, "the optional preview must restore the tenant transaction's timeout")
			return nil
		}))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		wire := string(testutil.GetResponseBody(req))
		require.NotContains(t, wire, models.MessageCoexistencePendingQuoteMetadataKey)
		require.NotContains(t, wire, pending)
		var data struct {
			Messages []MessageResponse `json:"messages"`
		}
		testutil.ParseEnvelopeResponse(t, req, &data)
		for _, message := range data.Messages {
			if message.ID == reply.ID {
				require.Equal(t, replyWAMID, message.WAMID)
				require.Equal(t, map[string]any{"body": replyText}, message.Content)
				return message, wire
			}
		}
		t.Fatal("the authorized transcript omitted the synthetic reply")
		return MessageResponse{}, ""
	}
	requireUnresolved := func(message MessageResponse) {
		t.Helper()
		require.True(t, message.QuotePending)
		require.False(t, message.IsReply)
		require.Nil(t, message.ReplyToMessageID)
		require.Nil(t, message.ReplyToMessage)
	}
	readQuotes := func(userID uuid.UUID, accountName, ids string, status int) []transcriptQuoteUpdate {
		t.Helper()
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, userID)
		req.RequestCtx.Request.Header.Set("X-Organization-ID", org.ID.String())
		testutil.SetPathParam(req, "id", contact.ID.String())
		testutil.SetQueryParam(req, "account", accountName)
		testutil.SetQueryParam(req, "quote_ids", ids)
		testutil.SetQueryParam(req, "acknowledge", "false")
		require.NoError(t, app.WithCommittedTenantApp(org.ID, func(scoped *App) error {
			return scoped.GetMessages(req)
		}))
		require.Equal(t, status, testutil.GetResponseStatusCode(req))
		require.Equal(t, "private, no-store", string(req.RequestCtx.Response.Header.Peek("Cache-Control")))
		if status != fasthttp.StatusOK {
			return nil
		}
		wire := string(testutil.GetResponseBody(req))
		for _, secret := range []string{replyText, replyWAMID, originalWAMID, pending, models.MessageCoexistencePendingQuoteMetadataKey} {
			require.NotContains(t, wire, secret)
		}
		var data struct {
			QuoteUpdates []transcriptQuoteUpdate `json:"quote_updates"`
		}
		testutil.ParseEnvelopeResponse(t, req, &data)
		require.NotNil(t, data.QuoteUpdates)
		return data.QuoteUpdates
	}
	childID := reply.ID.String()
	require.Empty(t, readQuotes(admin.ID, account.Name, childID, fasthttp.StatusOK))
	readQuotes(admin.ID, account.Name, childID+","+childID, fasthttp.StatusBadRequest)

	current, wire := readTranscript(admin.ID, uuid.Nil)
	requireUnresolved(current)
	require.NotContains(t, wire, originalWAMID)

	sendEcho(otherWAMID, otherText, "", base.Add(3*time.Minute))
	for _, target := range []string{"", otherWAMID} {
		sendEcho(replyWAMID, replyText, target, replyAt)
		stored := requireSingleWAMIDMessage(t, app, *account, replyWAMID)
		require.Equal(t, reply.ID, stored.ID)
		require.Equal(t, pending, stored.Metadata[models.MessageCoexistencePendingQuoteMetadataKey])
		require.Nil(t, stored.ReplyToMessageID)
	}
	current, _ = readTranscript(admin.ID, uuid.Nil)
	requireUnresolved(current)

	sendEcho(originalWAMID, originalText, "", base)
	original := requireSingleWAMIDMessage(t, app, *account, originalWAMID)
	require.Equal(t, contact.ID, original.ContactID)
	preview := &ReplyPreview{
		ID: original.ID.String(), Content: map[string]any{"body": originalText},
		MessageType: models.MessageTypeText, Direction: models.DirectionOutgoing, ScopedContext: true,
	}
	require.Equal(t, []transcriptQuoteUpdate{{
		ID: reply.ID, ReplyToMessageID: original.ID, ReplyToMessage: preview,
	}}, readQuotes(admin.ID, account.Name, childID+","+uuid.NewString(), fasthttp.StatusOK))
	require.Empty(t, readQuotes(admin.ID, "other-account-"+uuid.NewString(), childID, fasthttp.StatusOK))
	for _, cursor := range []uuid.UUID{uuid.Nil, original.ID} {
		message, _ := readTranscript(admin.ID, cursor)
		require.True(t, message.IsReply)
		require.False(t, message.QuotePending)
		require.NotNil(t, message.ReplyToMessageID)
		require.Equal(t, original.ID.String(), *message.ReplyToMessageID)
		require.Equal(t, preview, message.ReplyToMessage)
	}

	agentRole := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "late-quote-current-only", []string{
		models.ResourceChat + ":" + models.ActionRead,
	})
	agent := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&agentRole.ID))
	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		OrganizationID: org.ID,
		AgentAssignment: models.AgentAssignmentConfig{
			CurrentConversationOnly: true,
		},
	}).Error)
	require.NoError(t, app.DB.Create(&models.AgentTransfer{
		OrganizationID: org.ID, ContactID: contact.ID, WhatsAppAccount: account.Name,
		PhoneNumber: contact.PhoneNumber, Status: models.TransferStatusActive,
		Source: models.TransferSourceManual, AgentID: &agent.ID, TransferredAt: base.Add(time.Minute),
	}).Error)

	current, wire = readTranscript(agent.ID, uuid.Nil)
	requireUnresolved(current)
	require.NotContains(t, wire, originalText)
	require.NotContains(t, wire, originalWAMID)
	require.NotContains(t, wire, original.ID.String())
	require.Empty(t, readQuotes(agent.ID, account.Name, childID, fasthttp.StatusOK))
	require.Nil(t, requireSingleWAMIDMessage(t, app, *account, replyWAMID).ReplyToMessageID,
		"reading the preview must not mutate the underlying message")
}
