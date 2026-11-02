package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errReaformDisabled = errors.New("Reaform pilot is not enabled for this workspace")

func (a *App) reaformEnabled(orgID uuid.UUID) bool {
	return a != nil && a.Config != nil && a.Config.ReaformPilot.Enabled && a.Config.ReaformPilot.OrganizationID == orgID.String() && config.ValidateReaformPilotConfig(a.Config.ReaformPilot) == nil
}

// Each phase owns a real commit. These handlers must never be wrapped in the
// request-long Tenant adapter: network I/O only starts after the claim commits.
func (a *App) reaformAuthorize(r *fastglue.Request, write bool, leadID *uuid.UUID, fn func(*App, uuid.UUID, *models.CRMLead) error) (uuid.UUID, error) {
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return uuid.Nil, err
	}
	if !a.reaformEnabled(orgID) {
		return orgID, errReaformDisabled
	}
	action := models.ActionRead
	if write {
		action = models.ActionWrite
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		verifiedOrg, userID, err := scoped.requireAuth(r, models.ResourceCRMLeads, action)
		if err != nil {
			return err
		}
		if verifiedOrg != orgID {
			return errors.New("tenant_binding_mismatch")
		}
		var lead *models.CRMLead
		if leadID != nil {
			lead = &models.CRMLead{}
			query := scoped.DB.Where("id = ? AND organization_id = ?", *leadID, orgID)
			if write {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			if err := query.First(lead).Error; err != nil {
				return err
			}
			var contact models.Contact
			query = scoped.scopeAssignedContact(scoped.DB.Model(&models.Contact{}), userID, orgID)
			if err := query.Where("contacts.id = ? AND contacts.organization_id = ?", lead.ContactID, orgID).First(&contact).Error; err != nil {
				return err
			}
		}
		return fn(scoped, userID, lead)
	})
	return orgID, err
}

func reaformSendError(r *fastglue.Request, err error) error {
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if errors.Is(err, errReaformDisabled) {
		return r.SendErrorEnvelope(404, "Reaform pilot is not enabled", nil, "")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.SendErrorEnvelope(404, "Journey not found", nil, "")
	}
	var conflict *productCRMClientError
	if errors.As(err, &conflict) {
		return r.SendErrorEnvelope(conflict.status, conflict.message, nil, "")
	}
	return r.SendErrorEnvelope(503, "Clinic integration is temporarily unavailable; retry or review the saved appointment status", nil, "")
}

func reaformPublicLink(link *reaformPilotLink) any {
	if link == nil {
		return nil
	}
	return map[string]any{"command_id": link.CommandID, "patient_id": link.PatientID, "state": link.State, "can_retry": link.State == "rejected" && link.RetryAllowed, "appointment": link.Appointment, "last_attempt_at": link.LastAttemptAt, "last_synced_at": link.LastSyncedAt, "error_code": link.ErrorCode}
}

func (a *App) GetReaformLead(r *fastglue.Request) error {
	leadID, err := parsePathUUID(r, "id", "lead")
	if err != nil {
		return nil
	}
	var link *reaformPilotLink
	canWrite := false
	_, err = a.reaformAuthorize(r, false, &leadID, func(scoped *App, userID uuid.UUID, lead *models.CRMLead) error {
		canWrite = scoped.HasPermission(userID, models.ResourceCRMLeads, models.ActionWrite, lead.OrganizationID)
		var err error
		link, err = readReaformLink(lead.Metadata)
		if err == nil && link != nil && (link.ContactID != lead.ContactID.String() || link.Binding != reaformBinding(a.Config.ReaformPilot) || link.Request.SourceReference != "rereply:"+lead.ID.String()) {
			// Keep the durable command, but never reveal an old patient's
			// appointment after an explicit contact/clinic binding change.
			link = &reaformPilotLink{CommandID: link.CommandID, State: "unknown", ErrorCode: "patient_or_clinic_binding_changed", LastAttemptAt: link.LastAttemptAt}
			canWrite = false
		}
		return err
	})
	if errors.Is(err, errReaformDisabled) {
		return r.SendEnvelope(map[string]any{"enabled": false, "can_write": false, "link": nil})
	}
	if err != nil {
		return reaformSendError(r, err)
	}
	c, err := a.reaformCapabilities(context.Background())
	clinicName := "Clinic appointment"
	if err == nil {
		clinicName = c.SiteName
	}
	return r.SendEnvelope(map[string]any{"enabled": true, "clinic_name": clinicName, "can_write": canWrite, "sync_available": err == nil, "link": reaformPublicLink(link)})
}

func (a *App) GetReaformPatients(r *fastglue.Request) error {
	_, err := a.reaformAuthorize(r, true, nil, func(_ *App, _ uuid.UUID, _ *models.CRMLead) error { return nil })
	if err != nil {
		return reaformSendError(r, err)
	}
	query := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("query")))
	if utf8.RuneCountInString(query) < 3 || utf8.RuneCountInString(query) > 80 {
		return r.SendErrorEnvelope(400, "Enter 3 to 80 characters to find an existing clinic patient", nil, "")
	}
	if _, err = a.reaformCapabilities(context.Background()); err != nil {
		return reaformSendError(r, err)
	}
	var response struct {
		Patients []reaformPatient `json:"patients"`
	}
	if err = a.reaformRemote(context.Background(), http.MethodGet, "/patients?query="+reaformQuery(query), nil, &response); err != nil {
		return reaformSendError(r, err)
	}
	if len(response.Patients) > 20 {
		return reaformSendError(r, errors.New("invalid_remote_response"))
	}
	if response.Patients == nil {
		response.Patients = []reaformPatient{}
	}
	for _, p := range response.Patients {
		if !reaformUUID(p.ID) || len(p.DisplayName) > 250 || len(p.MaskedIdentifier) > 80 {
			return reaformSendError(r, errors.New("invalid_remote_response"))
		}
	}
	return r.SendEnvelope(response)
}

func (a *App) GetReaformAvailability(r *fastglue.Request) error {
	_, err := a.reaformAuthorize(r, true, nil, func(_ *App, _ uuid.UUID, _ *models.CRMLead) error { return nil })
	if err != nil {
		return reaformSendError(r, err)
	}
	date := string(r.RequestCtx.QueryArgs().Peek("date"))
	if d, err := time.Parse("2006-01-02", date); err != nil || d.Format("2006-01-02") != date {
		return r.SendErrorEnvelope(400, "Choose an appointment date", nil, "")
	}
	if _, err = a.reaformCapabilities(context.Background()); err != nil {
		return reaformSendError(r, err)
	}
	var response struct {
		Slots []reaformSlot `json:"slots"`
	}
	if err = a.reaformRemote(context.Background(), http.MethodGet, "/availability?date="+reaformQuery(date), nil, &response); err != nil {
		return reaformSendError(r, err)
	}
	if len(response.Slots) > 2500 {
		return reaformSendError(r, errors.New("invalid_remote_response"))
	}
	if response.Slots == nil {
		response.Slots = []reaformSlot{}
	}
	for _, s := range response.Slots {
		if !reaformUUID(s.DoctorID) || s.Date != date || s.DurationMinutes != 30 || len(s.DoctorName) > 200 {
			return reaformSendError(r, errors.New("invalid_remote_response"))
		}
		if _, err := time.Parse("15:04", s.Time); err != nil {
			return reaformSendError(r, err)
		}
	}
	return r.SendEnvelope(response)
}

func writeReaformLink(tx *gorm.DB, lead *models.CRMLead, link *reaformPilotLink) error {
	metadata := preserveReaformMetadata(lead.Metadata, nil)
	metadata[reaformPilotMetadataKey] = link
	// Caller holds the lead row lock. Integration bookkeeping does not change
	// the human CRM edit version or updated_at; generic edits preserve this
	// namespace from the same locked current row.
	result := tx.Model(&models.CRMLead{}).Where("id = ? AND organization_id = ?", lead.ID, lead.OrganizationID).UpdateColumn("metadata", metadata)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("lead_version_conflict")
	}
	lead.Metadata = metadata
	return nil
}

func reserveReaformCommand(tx *gorm.DB, lead *models.CRMLead, userID uuid.UUID, c config.ReaformPilotConfig, req reaformBookingRequest) (*reaformPilotLink, error) {
	if lead.Status == models.CRMLeadStatusArchived {
		return nil, newProductCRMClientError(409, "Reopen the journey before booking")
	}
	current, err := readReaformLink(lead.Metadata)
	if err != nil {
		return nil, err
	}
	req.SourceReference = "rereply:" + lead.ID.String()
	var previous []reaformRejectedAttempt
	if current != nil {
		if current.ContactID != lead.ContactID.String() || current.Binding != reaformBinding(c) || current.Request.SourceReference != "rereply:"+lead.ID.String() {
			return nil, newProductCRMClientError(409, "Linked patient or clinic binding changed; review the existing appointment")
		}
		if current.RequestHash == reaformRequestHash(req) {
			return current, nil
		}
		if current.State != "rejected" || !current.RetryAllowed || current.CommandID == req.CommandID {
			return nil, newProductCRMClientError(409, "This journey already has a saved booking request; refresh its status instead of creating another")
		}
		if len(current.PreviousAttempts) >= 50 {
			return nil, newProductCRMClientError(409, "Booking attempt limit reached; review this journey with the clinic")
		}
		for _, old := range current.PreviousAttempts {
			if old.Request.CommandID == req.CommandID {
				return nil, newProductCRMClientError(409, "A previous rejected request ID cannot be reused")
			}
		}
		previous = append(current.PreviousAttempts, reaformRejectedAttempt{Request: current.Request, RequestHash: current.RequestHash, ErrorCode: current.ErrorCode, RejectedAt: current.LastAttemptAt})
	}
	now := time.Now().UTC()
	link := &reaformPilotLink{CommandID: req.CommandID, PatientID: req.PatientID, ContactID: lead.ContactID.String(), Binding: reaformBinding(c), Request: req, RequestHash: reaformRequestHash(req), State: "pending", NextSyncAt: now, PreviousAttempts: previous}
	if err = writeReaformLink(tx, lead, link); err != nil {
		return nil, err
	}
	err = audit.LogAudit(tx, lead.OrganizationID, userID, audit.GetUserName(tx, userID), "reaform_clinic_booking", lead.ID, "create", nil, map[string]any{"command_id": req.CommandID, "identity_confirmed": true})
	return link, err
}

func (a *App) CreateReaformAppointment(r *fastglue.Request) error {
	leadID, err := parsePathUUID(r, "id", "lead")
	if err != nil {
		return nil
	}
	body := r.RequestCtx.PostBody()
	if len(body) > 4096 {
		return r.SendErrorEnvelope(413, "Booking request is too large", nil, "")
	}
	var req reaformBookingRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.SourceReference != "" {
		return r.SendErrorEnvelope(400, "Invalid booking request", nil, "")
	}
	if err = validateReaformBooking(req); err != nil {
		return r.SendErrorEnvelope(400, err.Error(), nil, "")
	}
	orgID, err := a.reaformAuthorize(r, true, &leadID, func(scoped *App, userID uuid.UUID, lead *models.CRMLead) error {
		_, err := reserveReaformCommand(scoped.DB, lead, userID, a.Config.ReaformPilot, req)
		return err
	})
	if err != nil {
		return reaformSendError(r, err)
	}
	// The durable command is committed above, even if this request loses its
	// response. The processor and Refresh always recover this same command ID.
	if err = a.syncReaformLead(context.Background(), orgID, leadID, true); err != nil {
		return reaformSendError(r, err)
	}
	return a.GetReaformLead(r)
}

func (a *App) RefreshReaformAppointment(r *fastglue.Request) error {
	leadID, err := parsePathUUID(r, "id", "lead")
	if err != nil {
		return nil
	}
	orgID, err := a.reaformAuthorize(r, true, &leadID, func(_ *App, _ uuid.UUID, lead *models.CRMLead) error {
		_, err := readReaformLink(lead.Metadata)
		return err
	})
	if err != nil {
		return reaformSendError(r, err)
	}
	if err = a.syncReaformLead(context.Background(), orgID, leadID, true); err != nil {
		return reaformSendError(r, err)
	}
	return a.GetReaformLead(r)
}

// claimReaformLink serializes concurrent API/worker replicas on the lead row.
// The lease is longer than the bounded remote calls. Expired claims resume by
// reading the command first; no new request UUID is ever generated here.
func claimReaformLink(tx *gorm.DB, lead *models.CRMLead, c config.ReaformPilotConfig, force bool, now time.Time) (*reaformPilotLink, error) {
	link, err := readReaformLink(lead.Metadata)
	if err != nil || link == nil {
		return nil, err
	}
	if link.LeaseUntil.After(now) || (!force && link.NextSyncAt.After(now)) {
		return nil, nil
	}
	if link.ContactID != lead.ContactID.String() || link.Binding != reaformBinding(c) || link.Request.SourceReference != "rereply:"+lead.ID.String() {
		link.ErrorCode = "patient_or_clinic_binding_changed"
		link.NextSyncAt = now.Add(time.Minute)
		return nil, writeReaformLink(tx, lead, link)
	}
	if link.State == "rejected" {
		return nil, nil
	}
	link.LeaseToken = uuid.NewString()
	link.LeaseUntil = now.Add(55 * time.Second)
	link.LastAttemptAt = &now
	link.NextSyncAt = now.Add(time.Minute)
	if err = writeReaformLink(tx, lead, link); err != nil {
		return nil, err
	}
	return link, nil
}

func (a *App) syncReaformLead(ctx context.Context, orgID, leadID uuid.UUID, force bool) error {
	return a.syncReaformLeadChecked(ctx, orgID, leadID, force, false, nil)
}

func (a *App) syncReaformLeadChecked(ctx context.Context, orgID, leadID uuid.UUID, force, capabilityChecked bool, capabilityError error) error {
	if !a.reaformEnabled(orgID) {
		return errReaformDisabled
	}
	var claimed *reaformPilotLink
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		allowed, err := scoped.HasProductEntitlement(uuid.Nil, orgID, "crm.enabled")
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("crm_entitlement_unavailable")
		}
		var lead models.CRMLead
		if err = scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", leadID, orgID).First(&lead).Error; err != nil {
			return err
		}
		claimed, err = claimReaformLink(scoped.DB, &lead, a.Config.ReaformPilot, force, time.Now().UTC())
		return err
	})
	if err != nil || claimed == nil {
		return err
	}
	var response reaformBookingResponse
	remoteErr := capabilityError
	if !capabilityChecked {
		_, remoteErr = a.reaformCapabilities(ctx)
	}
	if remoteErr == nil {
		path := "/commands/" + claimed.CommandID
		if claimed.Appointment != nil {
			path = "/appointments/" + claimed.Appointment.ID
		}
		remoteErr = a.reaformRemote(ctx, http.MethodGet, path, nil, &response)
		var remoteStatus *reaformRemoteError
		if claimed.Appointment == nil && errors.As(remoteErr, &remoteStatus) && remoteStatus.Status == 404 {
			remoteErr = a.reaformRemote(ctx, http.MethodPost, "/appointments", claimed.Request, &response)
		}
	}
	if remoteErr == nil {
		remoteErr = validateReaformSnapshot(claimed, response)
	}
	return a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var lead models.CRMLead
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", leadID, orgID).First(&lead).Error; err != nil {
			return err
		}
		link, err := readReaformLink(lead.Metadata)
		if err != nil {
			return err
		}
		if link == nil || link.CommandID != claimed.CommandID || link.LeaseToken != claimed.LeaseToken {
			return errors.New("booking_lease_lost")
		}
		now := time.Now().UTC()
		link.LeaseToken = ""
		link.LeaseUntil = time.Time{}
		link.NextSyncAt = now.Add(time.Minute)
		if link.ContactID != lead.ContactID.String() || link.Binding != reaformBinding(a.Config.ReaformPilot) || link.Request.SourceReference != "rereply:"+lead.ID.String() {
			remoteErr = errors.New("patient_or_clinic_binding_changed")
		}
		if remoteErr != nil {
			link.ErrorCode = remoteErr.Error()
			if link.Appointment == nil {
				link.State = "unknown"
			}
			var status *reaformRemoteError
			if link.Appointment == nil && errors.As(remoteErr, &status) && status.CommandRejected && status.CommandID == link.CommandID {
				link.State = "rejected"
				link.RetryAllowed = status.RetryAllowed
			}
		} else {
			// Recheck the latest projection under lock, not just the claimed copy.
			if err := validateReaformSnapshot(link, response); err != nil {
				link.ErrorCode = err.Error()
			} else {
				link.State = "linked"
				link.Appointment = &response.Appointment
				link.LastSyncedAt = &now
				link.ErrorCode = ""
				if reaformTerminalOld(link.Appointment, now) {
					link.NextSyncAt = now.Add(time.Hour)
				}
			}
		}
		return writeReaformLink(scoped.DB, &lead, link)
	})
}

// RunReaformPilotSync polls one configured organization, up to 40 links in batches of 20,
// with four concurrent requests and a 45-second total tick budget. Due-time
// ordering prevents newer journeys starving old
// ones. Terminal appointments remain polled for the configured retention so a
// clinical correction/reversal is reflected. Explicit Refresh bypasses age.
func (a *App) RunReaformPilotSync(ctx context.Context) {
	if a == nil || a.Config == nil || !a.Config.ReaformPilot.Enabled {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			started := time.Now()
			if err := a.syncReaformBatch(ctx); err != nil && ctx.Err() == nil {
				a.Log.Warn("Reaform pilot sync needs attention")
			}
			delay := time.Minute - time.Since(started)
			if delay < time.Second {
				delay = time.Second
			}
			timer.Reset(delay)
		}
	}
}

func (a *App) syncReaformBatch(ctx context.Context) error {
	if a == nil || a.Config == nil {
		return nil
	}
	orgID, err := uuid.Parse(a.Config.ReaformPilot.OrganizationID)
	if err != nil || !a.reaformEnabled(orgID) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// One capability probe plus at most 40 command GET/POST pairs keeps this
	// worker below the remote 120/minute limit and reserves capacity for staff.
	// Each remote endpoint still authenticates its fixed clinic independently.
	_, capabilityError := a.reaformCapabilities(ctx)
	var failures []error
	processed := 0
	for ctx.Err() == nil && processed < 40 {
		var ids []uuid.UUID
		now := time.Now().UTC()
		days := a.Config.ReaformPilot.TerminalRetentionDays
		if days == 0 {
			days = 90
		}
		cutoff := now.AddDate(0, 0, -days).Format(time.RFC3339Nano)
		err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			return scoped.DB.WithContext(ctx).Model(&models.CRMLead{}).
				Where("organization_id = ? AND metadata -> ? IS NOT NULL", orgID, reaformPilotMetadataKey).
				Where("COALESCE(metadata -> ? ->> 'state','') NOT IN ('rejected','quarantined')", reaformPilotMetadataKey).
				Where("COALESCE(metadata -> ? ->> 'next_sync_at', '') <= ?", reaformPilotMetadataKey, now.Format(time.RFC3339Nano)).
				Where("COALESCE(metadata -> ? -> 'appointment' ->> 'status', '') NOT IN ('completed','cancelled','no_show') OR metadata -> ? -> 'appointment' ->> 'scheduled_at' >= ?", reaformPilotMetadataKey, reaformPilotMetadataKey, cutoff).
				Order("metadata -> 'reaform_clinic_pilot_v1' ->> 'next_sync_at' ASC, id ASC").Limit(min(20, 40-processed)).Pluck("id", &ids).Error
		})
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return errors.Join(failures...)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		jobs := make(chan uuid.UUID)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for id := range jobs {
					if ctx.Err() != nil {
						continue
					}
					if syncErr := a.syncReaformLeadChecked(ctx, orgID, id, false, true, capabilityError); syncErr != nil {
						if errors.Is(syncErr, errInvalidReaformLink) {
							syncErr = errors.Join(syncErr, a.quarantineReaformLink(orgID, id))
						}
						mu.Lock()
						failures = append(failures, syncErr)
						mu.Unlock()
					}
				}
			}()
		}
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
			}
		}
		close(jobs)
		wg.Wait()
		processed += len(ids)
		// A database/authority failure must not create a hot retry loop. Every
		// selected lead was attempted, then the next minute retries the backlog.
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
	}
	return errors.Join(ctx.Err(), capabilityError)
}

func reaformTerminalOld(a *reaformAppointment, now time.Time) bool {
	if a == nil {
		return false
	}
	if a.Status != "completed" && a.Status != "cancelled" && a.Status != "no_show" {
		return false
	}
	scheduled, err := time.Parse(time.RFC3339Nano, a.ScheduledAt)
	return err == nil && scheduled.Before(now.Add(-24*time.Hour))
}

func (a *App) quarantineReaformLink(orgID, leadID uuid.UUID) error {
	return a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var lead models.CRMLead
		if err := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", leadID, orgID).First(&lead).Error; err != nil {
			return err
		}
		if _, err := readReaformLink(lead.Metadata); !errors.Is(err, errInvalidReaformLink) {
			return nil
		}
		metadata := preserveReaformMetadata(lead.Metadata, nil)
		metadata[reaformPilotMetadataKey] = models.JSONB{"state": "quarantined", "error_code": "invalid_saved_link", "previous_state": lead.Metadata[reaformPilotMetadataKey]}
		return scoped.DB.Model(&models.CRMLead{}).Where("id = ? AND organization_id = ?", leadID, orgID).UpdateColumn("metadata", metadata).Error
	})
}
