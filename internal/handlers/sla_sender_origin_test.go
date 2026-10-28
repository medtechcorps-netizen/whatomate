package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/bcrypt"
)

func TestSenderOriginConstructorNeverUsesLegacyFallbackForNewUserSend(t *testing.T) {
	app := &App{}
	userID := uuid.New()
	request := OutgoingMessageRequest{Account: &models.WhatsAppAccount{OrganizationID: uuid.New(), Name: "synthetic"},
		Contact: &models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}}, Type: models.MessageTypeText, Content: "Synthetic"}
	for _, origin := range []string{"", "unknown", "USER_JWT", " user_jwt", "user_jwt", "api_key", "unattributed"} {
		t.Run(fmt.Sprintf("origin=%q", origin), func(t *testing.T) {
			message := app.createOutgoingMessage(request, MessageSendOptions{SentByUserID: &userID, senderOrigin: origin})
			want := "unattributed"
			if origin == "user_jwt" || origin == "api_key" {
				want = origin
			}
			assert.Equal(t, want, message.Metadata[senderOriginMetadataKey])
		})
	}
	bot := app.createOutgoingMessage(request, MessageSendOptions{})
	assert.Nil(t, bot.SentByUserID)
	assert.NotContains(t, bot.Metadata, senderOriginMetadataKey, "do not retrospectively invent a user origin for bot sends")
}

// Use SQL, rather than the map decoder, to distinguish SQL NULL, JSON null,
// absent members and malformed-but-valid JSON shapes in the actual predicate.
func slaOriginSetJSON(t *testing.T, app *App, messageID uuid.UUID, raw *string) {
	t.Helper()
	if raw == nil {
		require.NoError(t, app.DB.Exec("UPDATE messages SET metadata = NULL WHERE id = ?", messageID).Error)
	} else {
		require.NoError(t, app.DB.Exec("UPDATE messages SET metadata = ?::jsonb WHERE id = ?", *raw, messageID).Error)
	}
}

func TestSLAOriginMetadataCompatibilityMatrix(t *testing.T) {
	app, org, createTransfer, _ := slaAfterCommitFixture(t)
	agent := testutil.CreateTestUser(t, app.DB, org.ID)
	other := testutil.CreateTestUser(t, app.DB, org.ID)
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	cases := []struct {
		name, raw         string
		sqlNull, eligible bool
	}{
		{"sql_null_legacy", "", true, true},
		{"empty_object_legacy", `{}`, false, true},
		{"unrelated_member_legacy", `{"template_name":"unrelated-metadata"}`, false, true},
		{"user_jwt", `{"sender_origin_v1":"user_jwt"}`, false, true},
		{"api_key", `{"sender_origin_v1":"api_key"}`, false, false},
		{"unattributed", `{"sender_origin_v1":"unattributed"}`, false, false},
		{"unknown", `{"sender_origin_v1":"human"}`, false, false},
		{"case_mismatch", `{"sender_origin_v1":"USER_JWT"}`, false, false},
		{"explicit_null", `{"sender_origin_v1":null}`, false, false},
		{"number", `{"sender_origin_v1":1}`, false, false},
		{"boolean", `{"sender_origin_v1":true}`, false, false},
		{"array", `{"sender_origin_v1":["user_jwt"]}`, false, false},
		{"object", `{"sender_origin_v1":{"method":"user_jwt"}}`, false, false},
		{"top_null", `null`, false, false},
		{"top_array", `["user_jwt"]`, false, false},
		{"top_string", `"user_jwt"`, false, false},
		{"top_number", `1`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transfer := createTransfer(models.SLATracking{EscalationLevel: 1, EscalatedAt: &since})
			slaEligibilityAssign(t, app, transfer, agent.ID)
			message := slaEligibilityMessage(t, app, transfer, since.Add(time.Minute), models.MessageStatusSent, models.MessageTypeText)
			raw := &tc.raw
			if tc.sqlNull {
				raw = nil
			}
			slaOriginSetJSON(t, app, message.ID, raw)
			for _, sender := range []struct {
				name     string
				id       *uuid.UUID
				deadline bool
			}{
				{"assigned", &agent.ID, tc.eligible}, {"other", &other.ID, false}, {"none", nil, false},
			} {
				t.Run(sender.name, func(t *testing.T) {
					require.NoError(t, app.DB.Model(&models.Message{}).Where("id = ?", message.ID).Update("sent_by_user_id", sender.id).Error)
					responded, err := NewSLAProcessor(app, time.Minute).agentRespondedSince(*transfer, since)
					require.NoError(t, err)
					assert.Equal(t, sender.deadline, responded)
					warning, err := slaWarningNoticeStillCurrent(app.DB, *transfer)
					require.NoError(t, err)
					assert.Equal(t, !(tc.eligible && sender.id != nil), warning)
				})
			}
		})
	}
}

func TestSLAOriginCannotBypassStatusTypeTimeOrTenantScope(t *testing.T) {
	app, org, createTransfer, _ := slaAfterCommitFixture(t)
	agent := testutil.CreateTestUser(t, app.DB, org.ID)
	foreign := testutil.CreateTestOrganization(t, app.DB)
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, origin := range []string{"user_jwt", "api_key"} {
		for _, tc := range []struct {
			status models.MessageStatus
			kind   models.MessageType
			valid  bool
		}{
			{models.MessageStatusSent, models.MessageTypeText, true},
			{models.MessageStatusDelivered, models.MessageTypeDocument, true},
			{models.MessageStatusRead, models.MessageTypeImage, true},
			{models.MessageStatusPending, models.MessageTypeText, false},
			{models.MessageStatusFailed, models.MessageTypeText, false},
			{models.MessageStatusReceived, models.MessageTypeText, false},
			{models.MessageStatusSent, models.MessageTypeTemplate, false},
		} {
			t.Run(origin+"/"+string(tc.status)+"/"+string(tc.kind), func(t *testing.T) {
				transfer := createTransfer(models.SLATracking{EscalationLevel: 1, EscalatedAt: &since})
				slaEligibilityAssign(t, app, transfer, agent.ID)
				message := slaEligibilityMessage(t, app, transfer, since.Add(time.Minute), tc.status, tc.kind)
				raw := fmt.Sprintf(`{"sender_origin_v1":%q}`, origin)
				slaOriginSetJSON(t, app, message.ID, &raw)
				want := tc.valid && origin == "user_jwt"
				got, err := NewSLAProcessor(app, time.Minute).agentRespondedSince(*transfer, since)
				require.NoError(t, err)
				assert.Equal(t, want, got)
				warning, err := slaWarningNoticeStillCurrent(app.DB, *transfer)
				require.NoError(t, err)
				assert.Equal(t, !want, warning)
				// A permissive origin must never escape the pre-existing AND scope.
				for _, selected := range []models.AgentTransfer{
					{OrganizationID: foreign.ID, ContactID: transfer.ContactID, AgentID: &agent.ID},
					{OrganizationID: org.ID, ContactID: uuid.New(), AgentID: &agent.ID},
				} {
					got, err := NewSLAProcessor(app, time.Minute).agentRespondedSince(selected, since)
					require.NoError(t, err)
					assert.False(t, got)
				}
				for _, at := range []time.Time{since.Add(-time.Second), since} {
					require.NoError(t, app.DB.Model(message).Update("created_at", at).Error)
					got, err := NewSLAProcessor(app, time.Minute).agentRespondedSince(*transfer, since)
					require.NoError(t, err)
					assert.False(t, got)
					warning, err := slaWarningNoticeStillCurrent(app.DB, *transfer)
					require.NoError(t, err)
					assert.True(t, warning)
				}
				require.NoError(t, app.DB.Model(message).Update("created_at", since.Add(time.Minute)).Error)
				require.NoError(t, app.DB.Delete(message).Error)
				got, err = NewSLAProcessor(app, time.Minute).agentRespondedSince(*transfer, since)
				require.NoError(t, err)
				assert.False(t, got)
			})
		}
	}
}

func TestSLAOriginActualTicksExtendOnlyAssignedJWTOrLegacyReplies(t *testing.T) {
	for _, kind := range []string{"auto_close", "escalation"} {
		for _, origin := range []string{"api_key", "user_jwt", "unattributed", "legacy"} {
			t.Run(kind+"/"+origin, func(t *testing.T) {
				app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
				agent := testutil.CreateTestUser(t, app.DB, org.ID)
				past := time.Now().UTC().Add(-time.Hour)
				sla := models.SLATracking{ExpiresAt: &past}
				if kind == "escalation" {
					sla = models.SLATracking{EscalationAt: &past}
				}
				transfer := createTransfer(sla)
				slaEligibilityAssign(t, app, transfer, agent.ID)
				message := slaEligibilityMessage(t, app, transfer, time.Now().Add(-30*time.Minute), models.MessageStatusSent, models.MessageTypeText)
				if origin != "legacy" {
					raw := fmt.Sprintf(`{"sender_origin_v1":%q}`, origin)
					slaOriginSetJSON(t, app, message.ID, &raw)
				}
				settings := slaNoticeStateSettings(org.ID)
				require.NoError(t, app.WithTenantApp(org.ID, func(scoped *App) error {
					processor := NewSLAProcessor(scoped, time.Minute)
					if kind == "auto_close" {
						processor.autoCloseExpiredTransfers(org.ID, settings, time.Now())
					} else {
						processor.escalateTransfers(org.ID, settings, time.Now())
					}
					return nil
				}))
				var stored models.AgentTransfer
				require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
				if origin == "user_jwt" || origin == "legacy" {
					assert.Equal(t, models.TransferStatusActive, stored.Status)
					assert.Zero(t, stored.SLA.EscalationLevel)
					require.NotNil(t, stored.SLA.FirstResponseAt)
					deadline := stored.SLA.ExpiresAt
					if kind == "escalation" {
						deadline = stored.SLA.EscalationAt
					}
					require.NotNil(t, deadline)
					assert.True(t, deadline.After(time.Now()))
					assert.Empty(t, deliveries())
				} else {
					assert.Nil(t, stored.SLA.FirstResponseAt)
					if kind == "auto_close" {
						assert.Equal(t, models.TransferStatusExpired, stored.Status)
					} else {
						assert.Equal(t, 1, stored.SLA.EscalationLevel)
					}
					require.Len(t, deliveries(), 1)
					assert.Equal(t, transfer.PhoneNumber, deliveries()[0].To)
				}
			})
		}
	}
}

// Install an earlier owned after-commit callback. The actual tick commits its
// escalation, the callback commits a new reply, then the actual queued warning
// must freshly apply the same origin rule. No sleep or unjoined goroutine.
func TestSLAOriginDeferredWarningRechecksReplyCommittedAfterTick(t *testing.T) {
	for _, origin := range []string{"api_key", "user_jwt", "unattributed"} {
		t.Run(origin, func(t *testing.T) {
			app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
			agent := testutil.CreateTestUser(t, app.DB, org.ID)
			past := time.Now().UTC().Add(-time.Hour)
			transfer := createTransfer(models.SLATracking{EscalationAt: &past})
			slaEligibilityAssign(t, app, transfer, agent.ID)
			committedReply := false
			require.NoError(t, app.WithTenantApp(org.ID, func(scoped *App) error {
				scoped.afterTenantCommit(func() {
					var transitioned models.AgentTransfer
					require.NoError(t, app.DB.First(&transitioned, "id = ?", transfer.ID).Error)
					require.Equal(t, 1, transitioned.SLA.EscalationLevel)
					require.NotNil(t, transitioned.SLA.EscalatedAt)
					// An explicit later timestamp avoids clock precision deciding eligibility.
					message := slaEligibilityMessage(t, app, transfer, transitioned.SLA.EscalatedAt.Add(time.Second), models.MessageStatusSent, models.MessageTypeText)
					raw := fmt.Sprintf(`{"sender_origin_v1":%q}`, origin)
					slaOriginSetJSON(t, app, message.ID, &raw)
					committedReply = true
				})
				NewSLAProcessor(scoped, time.Minute).escalateTransfers(org.ID, slaNoticeStateSettings(org.ID), time.Now())
				assert.Empty(t, deliveries(), "no notice may run before transaction commit")
				return nil
			}))
			require.True(t, committedReply, "causal reply callback must execute before warning recheck")
			if origin == "user_jwt" {
				assert.Empty(t, deliveries())
			} else {
				require.Len(t, deliveries(), 1)
			}
		})
	}
}

// Like the existing SLA runtime-role fixture this changes shared test RLS
// policies, so it must remain non-parallel. One runtime connection makes an
// accidental nested checkout observable rather than hiding it in a large pool.
func TestSLAOriginRuntimeRoleUsesOwnedSingleConnection(t *testing.T) {
	// SetupTestDB is shared and only cleans once. Isolate this serial policy
	// fixture before creating rows, and clean after the runtime pool/role close.
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	t.Cleanup(func() { testutil.TruncateTables(adminDB) })
	app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
	reseller := testutil.CreateTestReseller(t, adminDB)
	require.NoError(t, adminDB.Model(org).Update("reseller_id", reseller.ID).Error)
	org.ResellerID = &reseller.ID
	agent := testutil.CreateTestUser(t, adminDB, org.ID)
	past := time.Now().UTC().Add(-time.Hour)
	transfers := map[string]*models.AgentTransfer{}
	for _, origin := range []string{"api_key", "user_jwt"} {
		transfer := createTransfer(models.SLATracking{EscalationAt: &past})
		slaEligibilityAssign(t, app, transfer, agent.ID)
		message := slaEligibilityMessage(t, app, transfer, time.Now().Add(-30*time.Minute), models.MessageStatusSent, models.MessageTypeText)
		raw := fmt.Sprintf(`{"sender_origin_v1":%q}`, origin)
		slaOriginSetJSON(t, app, message.ID, &raw)
		transfers[origin] = transfer
	}
	role, password := "rereply_origin_"+uuid.NewString()[:8], "synthetic"+uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS", role, password)).Error)
	t.Cleanup(func() {
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + role).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + role).Error
	})
	require.NoError(t, database.ApplyTenantRLS(adminDB, role))
	runtimeDB := openRuntimeRoleTestDB(t, role, password)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, role))
	pool, err := runtimeDB.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	app.DB = runtimeDB.WithContext(ctx)
	app.Config.Database.RuntimeRole = role
	before := pool.Stats()
	require.NoError(t, app.WithTenantApp(org.ID, func(scoped *App) error {
		for origin, transfer := range transfers {
			got, err := NewSLAProcessor(scoped, time.Minute).agentRespondedSince(*transfer, past)
			if err != nil {
				return err
			}
			assert.Equal(t, origin == "user_jwt", got)
		}
		NewSLAProcessor(scoped, time.Minute).escalateTransfers(org.ID, slaNoticeStateSettings(org.ID), time.Now())
		assert.Empty(t, deliveries(), "runtime-role provider handoff must remain after commit")
		return nil
	}))
	assert.NoError(t, ctx.Err(), "the owned connection must finish within its query budget")
	assert.Equal(t, before.WaitCount, pool.Stats().WaitCount, "no nested pool checkout while a tenant transaction owns the single connection")
	for origin, transfer := range transfers {
		var stored models.AgentTransfer
		require.NoError(t, adminDB.First(&stored, "id = ?", transfer.ID).Error)
		if origin == "user_jwt" {
			assert.Zero(t, stored.SLA.EscalationLevel)
			require.NotNil(t, stored.SLA.FirstResponseAt)
		} else {
			assert.Equal(t, 1, stored.SLA.EscalationLevel)
			assert.Nil(t, stored.SLA.FirstResponseAt)
		}
	}
	require.Len(t, deliveries(), 1)
	assert.Equal(t, transfers["api_key"].PhoneNumber, deliveries()[0].To)
}

// This joins the actual authentication, routed writer, durable provider result
// and SLA tick in one fixture, in addition to the adversarial stored-JSON matrix.
func TestSLAOriginAuthenticatedSendControlsActualEscalation(t *testing.T) {
	for _, method := range []string{"api_key", "user_jwt"} {
		t.Run(method, func(t *testing.T) {
			app, org, createTransfer, deliveries := slaAfterCommitFixture(t)
			role := testutil.CreateAdminRole(t, app.DB, org.ID)
			agent := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
			past := time.Now().UTC().Add(-time.Hour)
			transfer := createTransfer(models.SLATracking{EscalationAt: &past})
			slaEligibilityAssign(t, app, transfer, agent.ID)
			require.NoError(t, app.DB.Model(&models.Contact{}).Where("id = ?", transfer.ContactID).Update("last_inbound_at", past).Error)
			r := testutil.NewJSONRequest(t, map[string]any{"type": "text", "content": map[string]string{"body": "Synthetic authenticated SLA reply"}})
			testutil.SetPathParam(r, "id", transfer.ContactID.String())
			if method == "api_key" {
				key := "whm_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
				require.NoError(t, err)
				require.NoError(t, app.DB.Create(&models.APIKey{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
					UserID: agent.ID, Name: "Synthetic SLA", KeyPrefix: key[4:20], KeyHash: string(hash), IsActive: true}).Error)
				r.RequestCtx.Request.Header.Set("X-API-Key", key)
			} else {
				claims := middleware.JWTClaims{UserID: agent.ID, OrganizationID: org.ID, Email: agent.Email,
					RegisteredClaims: jwt.RegisteredClaims{Subject: middleware.JWTSubjectAccess, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
				token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testutil.TestJWTSecret))
				require.NoError(t, err)
				r.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
			}
			require.NotNil(t, middleware.AuthWithDB(testutil.TestJWTSecret, app.DB)(r))
			require.Equal(t, method, middleware.AuthenticatedRequestMethod(r))
			require.NoError(t, app.Tenant((*App).SendMessage)(r))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r), string(testutil.GetResponseBody(r)))
			joined := make(chan struct{})
			go func() { app.WaitForBackgroundTasks(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(20 * time.Second):
				t.Fatal("authenticated send did not settle")
			}
			var message models.Message
			require.NoError(t, app.DB.Where("organization_id = ? AND contact_id = ? AND direction = ?", org.ID, transfer.ContactID, models.DirectionOutgoing).First(&message).Error)
			require.Equal(t, models.MessageStatusSent, message.Status)
			require.Equal(t, method, message.Metadata[senderOriginMetadataKey])
			require.Len(t, deliveries(), 1)
			require.NoError(t, app.WithTenantApp(org.ID, func(scoped *App) error {
				NewSLAProcessor(scoped, time.Minute).escalateTransfers(org.ID, slaNoticeStateSettings(org.ID), time.Now())
				return nil
			}))
			var stored models.AgentTransfer
			require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
			if method == "api_key" {
				assert.Equal(t, 1, stored.SLA.EscalationLevel)
				assert.Nil(t, stored.SLA.FirstResponseAt)
				require.Len(t, deliveries(), 2, "API-key reply must not suppress the actual later warning")
			} else {
				assert.Zero(t, stored.SLA.EscalationLevel)
				require.NotNil(t, stored.SLA.FirstResponseAt)
				require.Len(t, deliveries(), 1, "assigned JWT reply extends the deadline instead")
			}
		})
	}
}
