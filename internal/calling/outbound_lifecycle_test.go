package calling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveIncomingCallAccountRejectsTenantMismatchWithoutGraph(t *testing.T) {
	t.Parallel()

	called := false
	manager := &Manager{}
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: uuid.New(),
		Name:           "tenant-a-account",
		PhoneID:        "123456789",
		Status:         "active",
	}
	session := &CallSession{
		ID:             "wacid.cross-tenant",
		OrganizationID: uuid.New(),
		AccountName:    account.Name,
	}

	err := manager.withActiveIncomingCallAccount(
		context.Background(),
		session,
		account,
		func(*whatsapp.Account) error {
			called = true
			return nil
		},
	)

	require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
	assert.False(t, called)
}

func TestCallingGraphWritesRejectDisconnectedAccount(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	staleRuntime := account.ToWAAccount()
	require.NoError(t, db.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organization.ID).
		Update("status", "disconnected").Error)

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"calls":[{"id":"wacid.must-not-call"}]}`))
	}))
	t.Cleanup(server.Close)
	manager := &Manager{
		db:       db,
		log:      testutil.NopLogger(),
		whatsapp: whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL),
	}
	session := &CallSession{
		ID:             "wacid.incoming",
		OrganizationID: organization.ID,
		AccountName:    account.Name,
	}

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "outgoing initiate",
			call: func() error {
				_, err := manager.initiateActiveCall(
					context.Background(),
					organization.ID,
					account.Name,
					staleRuntime,
					whatsapp.Recipient{Phone: "15550000000"},
					"synthetic-offer",
				)
				return err
			},
		},
		{
			name: "incoming pre-accept",
			call: func() error {
				return manager.preAcceptActiveCall(
					context.Background(),
					session,
					account,
					"synthetic-answer",
				)
			},
		},
		{
			name: "incoming accept",
			call: func() error {
				return manager.acceptActiveCall(
					context.Background(),
					session,
					account,
					"synthetic-answer",
				)
			},
		},
		{
			name: "incoming reject",
			call: func() error {
				return manager.rejectActiveCall(context.Background(), session, account)
			},
		},
		{
			name: "terminate",
			call: func() error {
				return manager.terminateActiveCall(context.Background(), session, staleRuntime)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
			assert.Zero(t, providerRequests.Load(), "a disconnected account must make zero Graph requests")
		})
	}
}

func TestCallingGraphWritesReloadCurrentCredential(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	staleRuntime := account.ToWAAccount()
	staleRuntime.AccessToken = "stale-token"
	account.AccessToken = "stale-token"

	type observedRequest struct {
		action        string
		authorization string
		path          string
	}
	requests := make(chan observedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		requests <- observedRequest{
			action:        payload.Action,
			authorization: r.Header.Get("Authorization"),
			path:          r.URL.Path,
		}
		w.Header().Set("Content-Type", "application/json")
		if payload.Action == "connect" {
			_, _ = w.Write([]byte(`{"calls":[{"id":"wacid.fresh"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(server.Close)
	manager := &Manager{
		db:       db,
		log:      testutil.NopLogger(),
		whatsapp: whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL),
	}
	session := &CallSession{
		ID:             "wacid.incoming",
		OrganizationID: organization.ID,
		AccountName:    account.Name,
	}

	callID, err := manager.initiateActiveCall(
		context.Background(),
		organization.ID,
		account.Name,
		staleRuntime,
		whatsapp.Recipient{Phone: "15550000000"},
		"synthetic-offer",
	)
	require.NoError(t, err)
	assert.Equal(t, "wacid.fresh", callID)
	require.NoError(t, manager.preAcceptActiveCall(
		context.Background(), session, account, "synthetic-answer",
	))
	require.NoError(t, manager.acceptActiveCall(
		context.Background(), session, account, "synthetic-answer",
	))
	require.NoError(t, manager.terminateActiveCall(
		context.Background(), session, staleRuntime,
	))

	for _, expectedAction := range []string{"connect", "pre_accept", "accept", "terminate"} {
		select {
		case request := <-requests:
			assert.Equal(t, expectedAction, request.action)
			assert.Equal(t, "Bearer test-token", request.authorization)
			assert.Equal(t, "/v18.0/"+account.PhoneID+"/calls", request.path)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s Graph request", expectedAction)
		}
	}
}

func TestInitiateCallWaitsForLifecycleWriterAndFailsClosed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	staleRuntime := account.ToWAAccount()

	var providerRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"calls":[{"id":"wacid.must-not-call"}]}`))
	}))
	t.Cleanup(server.Close)
	manager := &Manager{
		db:       db,
		log:      testutil.NopLogger(),
		whatsapp: whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL),
	}

	lifecycleTx := db.Begin()
	require.NoError(t, lifecycleTx.Error)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = lifecycleTx.Rollback().Error
		}
	})
	require.NoError(t, lifecycleTx.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organization.ID).
		Update("status", "disconnected").Error)

	result := make(chan error, 1)
	go func() {
		_, err := manager.initiateActiveCall(
			context.Background(),
			organization.ID,
			account.Name,
			staleRuntime,
			whatsapp.Recipient{Phone: "15550000000"},
			"synthetic-offer",
		)
		result <- err
	}()

	select {
	case err := <-result:
		t.Fatalf("call lifecycle gate bypassed the pending disconnect: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	assert.Zero(t, providerRequests.Load())
	require.NoError(t, lifecycleTx.Commit().Error)
	committed = true

	select {
	case err := <-result:
		require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
	case <-time.After(5 * time.Second):
		t.Fatal("call lifecycle gate did not resume after the disconnect committed")
	}
	assert.Zero(t, providerRequests.Load())
}
