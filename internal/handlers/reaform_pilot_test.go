package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func reaformTestConfig(orgID uuid.UUID) config.ReaformPilotConfig {
	return config.ReaformPilotConfig{Enabled: true, OrganizationID: orgID.String(), RemoteOrganizationID: uuid.NewString(), RemoteSiteID: uuid.NewString(), APIBaseURL: "https://clinic.example.test", APIToken: strings.Repeat("a", 64)}
}

func reaformTestRequest(leadID uuid.UUID) reaformBookingRequest {
	return reaformBookingRequest{CommandID: uuid.NewString(), PatientID: uuid.NewString(), DoctorID: uuid.NewString(), Date: time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02"), Time: "09:00", DurationMinutes: 30, IdentityConfirmed: true, SourceReference: "rereply:" + leadID.String()}
}

func reaformTestSnapshot(req reaformBookingRequest) reaformBookingResponse {
	name := "Synthetic doctor"
	return reaformBookingResponse{CommandID: req.CommandID, SourceReference: req.SourceReference, PatientID: req.PatientID, Appointment: reaformAppointment{ID: uuid.NewString(), Status: "scheduled", Date: req.Date, Time: req.Time, DoctorID: &req.DoctorID, DoctorName: &name, Version: "1", ScheduledAt: req.Date + "T01:00:00Z", ScheduledEndAt: req.Date + "T01:30:00Z"}}
}

func TestReaformSnapshotBindingAndVersion(t *testing.T) {
	req := reaformTestRequest(uuid.New())
	response := reaformTestSnapshot(req)
	link := &reaformPilotLink{CommandID: req.CommandID, PatientID: req.PatientID, Request: req}
	require.NoError(t, validateReaformSnapshot(link, response))
	wrong := response
	wrong.PatientID = uuid.NewString()
	require.EqualError(t, validateReaformSnapshot(link, wrong), "remote_binding_mismatch")
	wrong = response
	wrong.SourceReference = "rereply:" + uuid.NewString()
	require.Error(t, validateReaformSnapshot(link, wrong))
	link.Appointment = &response.Appointment
	renamed := response
	name := "Renamed synthetic doctor"
	renamed.Appointment.DoctorName = &name
	require.NoError(t, validateReaformSnapshot(link, renamed))
	older := response
	older.Appointment.Version = "0"
	require.Error(t, validateReaformSnapshot(link, older))
	changed := response
	changed.Appointment.Status = "completed"
	require.EqualError(t, validateReaformSnapshot(link, changed), "remote_revision_conflict")
	changed.Appointment.Version = "2"
	require.NoError(t, validateReaformSnapshot(link, changed))
	link.Appointment = &changed.Appointment
	require.EqualError(t, validateReaformSnapshot(link, response), "stale_remote_snapshot")
	changed.Appointment.Version = "9007199254740993"
	require.NoError(t, validateReaformSnapshot(link, changed))
	changed.Appointment.DoctorID = nil
	changed.Appointment.DoctorName = nil
	require.NoError(t, validateReaformSnapshot(link, changed))
	changed.Appointment.DoctorID = &req.DoctorID
	require.Error(t, validateReaformSnapshot(link, changed))
}

func TestReaformMetadataCannotBeForgedOrErased(t *testing.T) {
	owned := models.JSONB{"command_id": "server-only"}
	submitted := models.JSONB{"note": "staff edit", reaformPilotMetadataKey: models.JSONB{"command_id": "forged"}}
	created := preserveReaformMetadata(submitted, nil)
	require.NotContains(t, created, reaformPilotMetadataKey)
	updated := preserveReaformMetadata(submitted, models.JSONB{reaformPilotMetadataKey: owned})
	require.Equal(t, owned, updated[reaformPilotMetadataKey])
	require.Equal(t, "staff edit", updated["note"])
	erased := preserveReaformMetadata(models.JSONB{}, updated)
	require.Equal(t, owned, erased[reaformPilotMetadataKey])
	projected := crmLeadToResponse(&models.CRMLead{Metadata: updated})
	require.NotContains(t, projected.Metadata, reaformPilotMetadataKey)
	require.Equal(t, "staff edit", projected.Metadata["note"])
}

func TestReaformTransportNoRedirectAndExplicitRejection(t *testing.T) {
	var redirected atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer sink.Close()
	command := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+strings.Repeat("a", 64), r.Header.Get("Authorization"))
		switch r.URL.Path {
		case reaformPilotAPIPrefix + "/redirect":
			w.Header().Set("Location", sink.URL)
			w.WriteHeader(302)
		case reaformPilotAPIPrefix + "/reject":
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "slot_unavailable", "message": "provider-private-text", "details": map[string]any{"command_id": command, "command_rejected": true, "retry_allowed": true}}})
		case reaformPilotAPIPrefix + "/conflict":
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "command_conflict"}})
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
		}
	}))
	defer server.Close()
	a := &App{Config: &config.Config{ReaformPilot: reaformTestConfig(uuid.New())}, HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{"https://clinic.example.test": server})}
	var out any
	require.Error(t, a.reaformRemote(context.Background(), "GET", "/redirect", nil, &out))
	require.Zero(t, redirected.Load())
	err := a.reaformRemote(context.Background(), "GET", "/reject", nil, &out)
	var remote *reaformRemoteError
	require.ErrorAs(t, err, &remote)
	require.True(t, remote.CommandRejected)
	require.True(t, remote.RetryAllowed)
	require.Equal(t, command, remote.CommandID)
	require.NotContains(t, err.Error(), "provider-private")
	err = a.reaformRemote(context.Background(), "GET", "/conflict", nil, &out)
	require.ErrorAs(t, err, &remote)
	require.False(t, remote.CommandRejected)
	require.Error(t, a.reaformRemote(context.Background(), "GET", "/bad", nil, &out))
}

func reaformDBFixture(t *testing.T) (*App, *models.CRMLead, uuid.UUID) {
	t.Helper()
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)
	lead := createProductCRMTestLead(t, app.DB, org.ID, user.ID, contact.ID, pipeline.ID, stage.ID, models.CRMLeadStatusOpen)
	enableCareContinuityEntitlements(t, app.DB, org.ID, user.ID, "crm.enabled")
	app.Config = &config.Config{ReaformPilot: reaformTestConfig(org.ID)}
	return app, lead, user.ID
}

func reaformReserveDB(app *App, leadID, userID uuid.UUID, req reaformBookingRequest) error {
	return app.WithCommittedTenantApp(uuid.MustParse(app.Config.ReaformPilot.OrganizationID), func(scoped *App) error {
		var lead models.CRMLead
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", leadID, app.Config.ReaformPilot.OrganizationID).First(&lead).Error; err != nil {
			return err
		}
		_, err := reserveReaformCommand(scoped.DB, &lead, userID, app.Config.ReaformPilot, req)
		return err
	})
}

func reaformLoadDB(t *testing.T, app *App, leadID uuid.UUID) (*models.CRMLead, *reaformPilotLink) {
	t.Helper()
	var lead models.CRMLead
	require.NoError(t, app.DB.First(&lead, "id = ?", leadID).Error)
	link, err := readReaformLink(lead.Metadata)
	require.NoError(t, err)
	return &lead, link
}

func TestReaformDBConcurrentCommandsAndHumanMetadata(t *testing.T) {
	app, lead, userID := reaformDBFixture(t)
	first := reaformTestRequest(lead.ID)
	second := reaformTestRequest(lead.ID)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, req := range []reaformBookingRequest{first, second} {
		wg.Add(1)
		go func(req reaformBookingRequest) {
			defer wg.Done()
			<-start
			results <- reaformReserveDB(app, lead.ID, userID, req)
		}(req)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.Contains(t, err.Error(), "saved booking request")
		}
	}
	require.Equal(t, 1, success)
	loaded, link := reaformLoadDB(t, app, lead.ID)
	require.Equal(t, lead.Version, loaded.Version)
	require.NoError(t, reaformReserveDB(app, lead.ID, userID, link.Request))
	// A human edit opened before the background claim still has a valid CRM
	// version, and its arbitrary metadata cannot erase or forge the bridge.
	forged := models.JSONB{"note": "edited", reaformPilotMetadataKey: models.JSONB{"state": "linked", "patient_id": uuid.NewString()}}
	req := testutil.NewJSONRequest(t, UpdateCRMLeadRequest{Version: loaded.Version, Metadata: &forged})
	testutil.SetPathParam(req, "id", lead.ID.String())
	req.RequestCtx.SetUserValue("organization_id", lead.OrganizationID)
	req.RequestCtx.SetUserValue("user_id", userID)
	require.NoError(t, app.UpdateCRMLead(req))
	require.Equal(t, 200, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
	loaded, after := reaformLoadDB(t, app, lead.ID)
	require.Equal(t, link.CommandID, after.CommandID)
	require.Equal(t, "edited", loaded.Metadata["note"])
	// The namespace also survives a concurrent full metadata write under the
	// same row lock used by generic CRM edits.
	require.NoError(t, app.WithCommittedTenantApp(lead.OrganizationID, func(scoped *App) error {
		var row models.CRMLead
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", lead.ID).Error; err != nil {
			return err
		}
		_, err := claimReaformLink(scoped.DB, &row, app.Config.ReaformPilot, true, time.Now().UTC())
		return err
	}))
	latest, claimed := reaformLoadDB(t, app, lead.ID)
	require.NotEmpty(t, claimed.LeaseToken)
	require.Equal(t, loaded.Version, latest.Version)
	require.Equal(t, "edited", latest.Metadata["note"])
}

func TestReaformDBUnknownResponseReconcilesWithoutSecondPOST(t *testing.T) {
	app, lead, userID := reaformDBFixture(t)
	req := reaformTestRequest(lead.ID)
	require.NoError(t, reaformReserveDB(app, lead.ID, userID, req))
	response := reaformTestSnapshot(req)
	var posts atomic.Int32
	var committed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == reaformPilotAPIPrefix+"/capabilities" {
			c := app.Config.ReaformPilot
			_ = json.NewEncoder(w).Encode(map[string]any{"data": reaformCapability{1, c.RemoteOrganizationID, c.RemoteSiteID, "Synthetic clinic", 30, "in_person"}})
			return
		}
		if r.Method == "POST" {
			posts.Add(1)
			committed.Store(true)
			w.WriteHeader(502)
			return
		}
		if !committed.Load() {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": response})
	}))
	defer server.Close()
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{"https://clinic.example.test": server})
	require.NoError(t, app.syncReaformLead(context.Background(), lead.OrganizationID, lead.ID, true))
	_, link := reaformLoadDB(t, app, lead.ID)
	require.Equal(t, "unknown", link.State)
	require.False(t, link.RetryAllowed)
	require.NoError(t, app.syncReaformLead(context.Background(), lead.OrganizationID, lead.ID, true))
	_, link = reaformLoadDB(t, app, lead.ID)
	require.Equal(t, "linked", link.State)
	require.Equal(t, int32(1), posts.Load())
	// The same linked appointment's correction replaces the projection without
	// recording an unrelated visit or moving the CRM stage.
	response.Appointment.Status = "checked_in"
	response.Appointment.Version = "2"
	arrival := time.Now().UTC().Format(time.RFC3339Nano)
	response.Appointment.CheckedInAt = &arrival
	require.NoError(t, app.syncReaformLead(context.Background(), lead.OrganizationID, lead.ID, true))
	updated, link := reaformLoadDB(t, app, lead.ID)
	require.Equal(t, "checked_in", link.Appointment.Status)
	require.Equal(t, lead.StageID, updated.StageID)
	response.Appointment.Version = "1"
	response.Appointment.Status = "scheduled"
	response.Appointment.CheckedInAt = nil
	require.NoError(t, app.syncReaformLead(context.Background(), lead.OrganizationID, lead.ID, true))
	_, link = reaformLoadDB(t, app, lead.ID)
	require.Equal(t, "checked_in", link.Appointment.Status)
	require.Equal(t, "stale_remote_snapshot", link.ErrorCode)
}

func TestReaformDBDefinitiveRejectionAllowsNewExplicitCommand(t *testing.T) {
	app, lead, userID := reaformDBFixture(t)
	req := reaformTestRequest(lead.ID)
	require.NoError(t, reaformReserveDB(app, lead.ID, userID, req))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			c := app.Config.ReaformPilot
			_ = json.NewEncoder(w).Encode(map[string]any{"data": reaformCapability{1, c.RemoteOrganizationID, c.RemoteSiteID, "Synthetic clinic", 30, "in_person"}})
			return
		}
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"details": map[string]any{"command_id": req.CommandID, "command_rejected": true, "retry_allowed": true}}})
	}))
	defer server.Close()
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{"https://clinic.example.test": server})
	require.NoError(t, app.syncReaformLead(context.Background(), lead.OrganizationID, lead.ID, true))
	_, link := reaformLoadDB(t, app, lead.ID)
	require.Equal(t, "rejected", link.State)
	require.True(t, link.RetryAllowed)
	changed := req
	changed.CommandID = uuid.NewString()
	changed.Time = "10:00"
	require.NoError(t, reaformReserveDB(app, lead.ID, userID, changed))
	_, link = reaformLoadDB(t, app, lead.ID)
	require.Equal(t, "pending", link.State)
	require.Len(t, link.PreviousAttempts, 1)
	require.Equal(t, req.CommandID, link.PreviousAttempts[0].Request.CommandID)
	require.Error(t, reaformReserveDB(app, lead.ID, userID, req))
}

func TestReaformDBBatchDrainsMoreThanTwentyAndQuarantinesCorruptState(t *testing.T) {
	app, first, userID := reaformDBFixture(t)
	responses := map[string]reaformBookingResponse{}
	for i := 0; i < 25; i++ {
		lead := first
		if i > 0 {
			lead = createProductCRMTestLead(t, app.DB, first.OrganizationID, userID, first.ContactID, first.PipelineID, first.StageID, models.CRMLeadStatusOpen)
		}
		req := reaformTestRequest(lead.ID)
		require.NoError(t, reaformReserveDB(app, lead.ID, userID, req))
		responses[req.CommandID] = reaformTestSnapshot(req)
	}
	bad := createProductCRMTestLead(t, app.DB, first.OrganizationID, userID, first.ContactID, first.PipelineID, first.StageID, models.CRMLeadStatusOpen)
	require.NoError(t, app.DB.Model(bad).UpdateColumn("metadata", models.JSONB{reaformPilotMetadataKey: models.JSONB{"state": "pending", "command_id": "corrupt"}}).Error)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			c := app.Config.ReaformPilot
			_ = json.NewEncoder(w).Encode(map[string]any{"data": reaformCapability{1, c.RemoteOrganizationID, c.RemoteSiteID, "Synthetic clinic", 30, "in_person"}})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, reaformPilotAPIPrefix+"/commands/")
		response, ok := responses[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		reads.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": response})
	}))
	defer server.Close()
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{"https://clinic.example.test": server})
	// A corrupt row is quarantined and may end this tick after its batch, but
	// all peers in that batch progress. The next tick drains the remaining rows.
	err := app.syncReaformBatch(context.Background())
	if err != nil {
		require.True(t, errors.Is(err, errInvalidReaformLink))
		require.NoError(t, app.syncReaformBatch(context.Background()))
	}
	require.Equal(t, int32(25), reads.Load())
	var stored models.CRMLead
	require.NoError(t, app.DB.First(&stored, "id = ?", bad.ID).Error)
	quarantine := stored.Metadata[reaformPilotMetadataKey].(map[string]any)
	require.Equal(t, "quarantined", quarantine["state"])
	var count int64
	require.NoError(t, app.DB.Model(&models.CRMLead{}).Where("organization_id = ? AND metadata -> ? ->> 'state' = 'linked'", first.OrganizationID, reaformPilotMetadataKey).Count(&count).Error)
	require.Equal(t, int64(25), count)
}

func TestReaformDBLeaseSurvivesConcurrentHumanEdit(t *testing.T) {
	app, lead, userID := reaformDBFixture(t)
	req := reaformTestRequest(lead.ID)
	require.NoError(t, reaformReserveDB(app, lead.ID, userID, req))
	locked := make(chan struct{})
	release := make(chan struct{})
	claimDone := make(chan error, 1)
	go func() {
		claimDone <- app.WithCommittedTenantApp(lead.OrganizationID, func(scoped *App) error {
			var row models.CRMLead
			if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", lead.ID).Error; err != nil {
				return err
			}
			if _, err := claimReaformLink(scoped.DB, &row, app.Config.ReaformPilot, true, time.Now().UTC()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	editorPID := make(chan int, 1)
	editorDone := make(chan error, 1)
	go func() {
		editorDone <- app.DB.Connection(func(db *gorm.DB) error {
			var pid int
			if err := db.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				return err
			}
			editorPID <- pid
			return db.Transaction(func(tx *gorm.DB) error {
				var row models.CRMLead
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", lead.ID).Error; err != nil {
					return err
				}
				metadata := preserveReaformMetadata(models.JSONB{"human": "survives", reaformPilotMetadataKey: models.JSONB{"command_id": "spoof"}}, row.Metadata)
				return tx.Model(&models.CRMLead{}).Where("id = ? AND version = ?", lead.ID, lead.Version).Updates(map[string]any{"metadata": metadata, "version": gorm.Expr("version + 1")}).Error
			})
		})
	}()
	pid := <-editorPID
	testutil.RequirePostgresBackendWaitingForLock(t, app.DB, pid)
	close(release)
	require.NoError(t, <-claimDone)
	require.NoError(t, <-editorDone)
	stored, link := reaformLoadDB(t, app, lead.ID)
	require.Equal(t, req.CommandID, link.CommandID)
	require.NotEmpty(t, link.LeaseToken)
	require.Equal(t, "survives", stored.Metadata["human"])
	require.Equal(t, lead.Version+1, stored.Version)
	// A live lease cannot be claimed by a second replica; an expired lease
	// recovers the immutable original command rather than creating another.
	require.NoError(t, app.WithCommittedTenantApp(lead.OrganizationID, func(scoped *App) error {
		var row models.CRMLead
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", lead.ID).Error; err != nil {
			return err
		}
		claimed, err := claimReaformLink(scoped.DB, &row, app.Config.ReaformPilot, true, time.Now().UTC())
		require.NoError(t, err)
		require.Nil(t, claimed)
		claimed, err = claimReaformLink(scoped.DB, &row, app.Config.ReaformPilot, true, time.Now().UTC().Add(time.Minute))
		require.NoError(t, err)
		require.Equal(t, req.CommandID, claimed.CommandID)
		require.NotEqual(t, link.LeaseToken, claimed.LeaseToken)
		return err
	}))
}

func TestReaformDBSavedStateOutageAndChangedBinding(t *testing.T) {
	app, lead, userID := reaformDBFixture(t)
	request := reaformTestRequest(lead.ID)
	require.NoError(t, reaformReserveDB(app, lead.ID, userID, request))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{"https://clinic.example.test": server})
	get := func() map[string]any {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, lead.OrganizationID, userID)
		testutil.SetHeader(req, "X-Organization-ID", lead.OrganizationID.String())
		testutil.SetPathParam(req, "id", lead.ID.String())
		require.NoError(t, app.GetReaformLead(req))
		require.Equal(t, 200, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
		var response struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(req.RequestCtx.Response.Body(), &response))
		return response.Data
	}
	data := get()
	require.Equal(t, false, data["sync_available"])
	link := data["link"].(map[string]any)
	require.Equal(t, request.PatientID, link["patient_id"])
	app.Config.ReaformPilot.RemoteSiteID = uuid.NewString()
	data = get()
	link = data["link"].(map[string]any)
	require.Equal(t, "", link["patient_id"])
	require.Nil(t, link["appointment"])
	require.Equal(t, false, data["can_write"])
	require.Equal(t, "patient_or_clinic_binding_changed", link["error_code"])
	// The unconfigured organization cannot use a guessed lead UUID to make
	// any external request. No remote tenant selection comes from its header.
	other := testutil.CreateTestOrganization(t, app.DB)
	otherUser := testutil.CreateTestUser(t, app.DB, other.ID, testutil.WithSuperAdmin())
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, other.ID, otherUser.ID)
	testutil.SetHeader(req, "X-Organization-ID", other.ID.String())
	testutil.SetPathParam(req, "id", lead.ID.String())
	before := calls.Load()
	require.NoError(t, app.GetReaformLead(req))
	require.Equal(t, 200, req.RequestCtx.Response.StatusCode())
	require.Contains(t, string(req.RequestCtx.Response.Body()), `"enabled":false`)
	require.Equal(t, before, calls.Load())
}
