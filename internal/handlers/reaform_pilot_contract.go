package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

const reaformPilotMetadataKey = "reaform_clinic_pilot_v1"
const reaformPilotAPIPrefix = "/api/integrations/rereply/v1"

var errInvalidReaformLink = errors.New("invalid saved Reaform link")

type reaformBookingRequest struct {
	CommandID         string `json:"command_id"`
	PatientID         string `json:"patient_id"`
	DoctorID          string `json:"doctor_id"`
	Date              string `json:"date"`
	Time              string `json:"time"`
	DurationMinutes   int    `json:"duration_minutes"`
	IdentityConfirmed bool   `json:"identity_confirmed"`
	SourceReference   string `json:"source_reference"`
}

type reaformAppointment struct {
	ID                    string  `json:"id"`
	Status                string  `json:"status"`
	Date                  string  `json:"date"`
	Time                  string  `json:"time"`
	DoctorID              *string `json:"doctor_id"`
	DoctorName            *string `json:"doctor_name"`
	Version               string  `json:"version"`
	ScheduledAt           string  `json:"scheduled_at"`
	ScheduledEndAt        string  `json:"scheduled_end_at"`
	CheckedInAt           *string `json:"checked_in_at"`
	ConsultationStartedAt *string `json:"consultation_started_at"`
	CompletedAt           *string `json:"completed_at"`
	CancelledAt           *string `json:"cancelled_at"`
}

type reaformBookingResponse struct {
	CommandID       string             `json:"command_id"`
	SourceReference string             `json:"source_reference"`
	PatientID       string             `json:"patient_id"`
	Appointment     reaformAppointment `json:"appointment"`
}

type reaformCapability struct {
	ContractVersion int    `json:"contract_version"`
	OrganizationID  string `json:"organization_id"`
	SiteID          string `json:"site_id"`
	SiteName        string `json:"site_name"`
	DurationMinutes int    `json:"duration_minutes"`
	VisitType       string `json:"visit_type"`
}

type reaformPatient struct {
	ID               string `json:"id"`
	DisplayName      string `json:"display_name"`
	MaskedIdentifier string `json:"masked_identifier"`
}

type reaformSlot struct {
	DoctorID        string `json:"doctor_id"`
	DoctorName      string `json:"doctor_name"`
	Date            string `json:"date"`
	Time            string `json:"time"`
	DurationMinutes int    `json:"duration_minutes"`
}

// This state belongs to the server, not to generic editable CRM metadata. The
// immutable original contact prevents a contact reassignment changing identity.
type reaformPilotLink struct {
	CommandID        string                   `json:"command_id"`
	PatientID        string                   `json:"patient_id"`
	ContactID        string                   `json:"contact_id"`
	Binding          string                   `json:"binding"`
	Request          reaformBookingRequest    `json:"request"`
	RequestHash      string                   `json:"request_hash"`
	State            string                   `json:"state"`
	RetryAllowed     bool                     `json:"can_retry"`
	PreviousAttempts []reaformRejectedAttempt `json:"previous_attempts,omitempty"`
	Appointment      *reaformAppointment      `json:"appointment"`
	LastAttemptAt    *time.Time               `json:"last_attempt_at"`
	LastSyncedAt     *time.Time               `json:"last_synced_at"`
	ErrorCode        string                   `json:"error_code,omitempty"`
	LeaseToken       string                   `json:"lease_token,omitempty"`
	LeaseUntil       time.Time                `json:"lease_until"`
	NextSyncAt       time.Time                `json:"next_sync_at"`
}

type reaformRejectedAttempt struct {
	Request     reaformBookingRequest `json:"request"`
	RequestHash string                `json:"request_hash"`
	ErrorCode   string                `json:"error_code"`
	RejectedAt  *time.Time            `json:"rejected_at"`
}

func reaformUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validateReaformBooking(req reaformBookingRequest) error {
	if !reaformUUID(req.CommandID) || !reaformUUID(req.PatientID) || !reaformUUID(req.DoctorID) || !req.IdentityConfirmed || req.DurationMinutes != 30 {
		return errors.New("Select an existing patient and explicitly confirm their identity before choosing a 30-minute appointment")
	}
	date, err := time.Parse("2006-01-02", req.Date)
	if err != nil || date.Format("2006-01-02") != req.Date {
		return errors.New("Invalid appointment date")
	}
	clock, err := time.Parse("15:04", req.Time)
	if err != nil || clock.Format("15:04") != req.Time {
		return errors.New("Invalid appointment time")
	}
	return nil
}

func reaformRequestHash(req reaformBookingRequest) string {
	b, _ := json.Marshal(req)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func reaformBinding(c config.ReaformPilotConfig) string {
	h := sha256.Sum256([]byte(c.OrganizationID + "|" + c.RemoteOrganizationID + "|" + c.RemoteSiteID + "|" + strings.TrimSuffix(c.APIBaseURL, "/")))
	return hex.EncodeToString(h[:])
}

func readReaformLink(metadata models.JSONB) (*reaformPilotLink, error) {
	raw, exists := metadata[reaformPilotMetadataKey]
	if !exists {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, errInvalidReaformLink
	}
	var link reaformPilotLink
	if err := json.Unmarshal(b, &link); err != nil || !reaformUUID(link.CommandID) || !reaformUUID(link.ContactID) || link.PatientID != link.Request.PatientID || link.CommandID != link.Request.CommandID || link.RequestHash != reaformRequestHash(link.Request) || validateReaformBooking(link.Request) != nil {
		return nil, errInvalidReaformLink
	}
	switch link.State {
	case "pending", "unknown", "linked", "rejected":
	default:
		return nil, errInvalidReaformLink
	}
	return &link, nil
}

// Preserve server state even when a generic editor submits an old metadata
// snapshot. On creation a client can never introduce the reserved namespace.
func preserveReaformMetadata(submitted, stored models.JSONB) models.JSONB {
	result := make(models.JSONB, len(submitted)+1)
	for k, v := range submitted {
		if k != reaformPilotMetadataKey {
			result[k] = v
		}
	}
	if v, exists := stored[reaformPilotMetadataKey]; exists {
		result[reaformPilotMetadataKey] = v
	}
	return result
}

func reaformVersion(value string) (*big.Int, bool) {
	if value == "" || len(value) > 30 || (len(value) > 1 && value[0] == '0') {
		return nil, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(value, 10)
	return n, ok && n.Sign() > 0
}

func validateReaformSnapshot(link *reaformPilotLink, response reaformBookingResponse) error {
	a := response.Appointment
	validDoctor := (a.DoctorID == nil && a.DoctorName == nil) || (a.DoctorID != nil && a.DoctorName != nil && reaformUUID(*a.DoctorID) && len(*a.DoctorName) <= 200)
	if response.CommandID != link.CommandID || response.PatientID != link.PatientID || response.SourceReference != link.Request.SourceReference || !reaformUUID(a.ID) || !validDoctor {
		return errors.New("remote_binding_mismatch")
	}
	version, ok := reaformVersion(a.Version)
	if !ok {
		return errors.New("invalid_remote_version")
	}
	if link.Appointment != nil {
		previous, valid := reaformVersion(link.Appointment.Version)
		if !valid || a.ID != link.Appointment.ID {
			return errors.New("remote_binding_mismatch")
		}
		if version.Cmp(previous) < 0 {
			return errors.New("stale_remote_snapshot")
		}
		if version.Cmp(previous) == 0 {
			// The revision covers appointment identity, state and timing.
			// Doctor names are descriptive staff labels and may change without
			// an appointment mutation. Never weaken equality for the doctor ID.
			oldAppointment := *link.Appointment
			currentAppointment := a
			oldAppointment.DoctorName = nil
			currentAppointment.DoctorName = nil
			old, _ := json.Marshal(oldAppointment)
			current, _ := json.Marshal(currentAppointment)
			if !bytes.Equal(old, current) {
				return errors.New("remote_revision_conflict")
			}
		}
	}
	if _, err := time.Parse("2006-01-02", a.Date); err != nil {
		return errors.New("invalid_remote_snapshot")
	}
	if _, err := time.Parse("15:04", a.Time); err != nil {
		return errors.New("invalid_remote_snapshot")
	}
	for _, instant := range []string{a.ScheduledAt, a.ScheduledEndAt} {
		if _, err := time.Parse(time.RFC3339Nano, instant); err != nil {
			return errors.New("invalid_remote_snapshot")
		}
	}
	for _, instant := range []*string{a.CheckedInAt, a.ConsultationStartedAt, a.CompletedAt, a.CancelledAt} {
		if instant != nil {
			if _, err := time.Parse(time.RFC3339Nano, *instant); err != nil {
				return errors.New("invalid_remote_snapshot")
			}
		}
	}
	switch a.Status {
	case "scheduled", "confirmed", "checked_in", "in_consultation", "dispensing", "billing", "completed", "cancelled", "no_show":
	default:
		return errors.New("invalid_remote_status")
	}
	return nil
}

type reaformRemoteError struct {
	Status          int
	Code            string
	CommandID       string
	CommandRejected bool
	RetryAllowed    bool
}

func (e *reaformRemoteError) Error() string { return e.Code }

// The response is decoded into allowlisted DTOs, never forwarded as raw JSON.
// The shared production transport rejects private DNS results. Tests inject a
// transport; there is no runtime switch that relaxes destination restrictions.
func (a *App) reaformRemote(ctx context.Context, method, path string, request, result any) error {
	c := a.Config.ReaformPilot
	if err := config.ValidateReaformPilotConfig(c); err != nil || !c.Enabled {
		return errors.New("integration_unavailable")
	}
	var body io.Reader
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return errors.New("invalid_request")
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.APIBaseURL, "/")+reaformPilotAPIPrefix+path, body)
	if err != nil {
		return errors.New("integration_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")
	if a.HTTPClient == nil {
		return errors.New("integration_unavailable")
	}
	client := *a.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("remote_unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 {
		return errors.New("invalid_remote_response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not expose provider text/URLs/credentials or reflect arbitrary errors.
		code := "remote_unavailable"
		switch resp.StatusCode {
		case 404:
			code = "remote_not_found"
		case 400, 409, 422:
			code = "booking_rejected"
		case 401, 403:
			code = "remote_authorization_failed"
		}
		remoteError := &reaformRemoteError{Status: resp.StatusCode, Code: code}
		if resp.StatusCode == 409 || resp.StatusCode == 422 {
			var failure struct {
				Error struct {
					Details struct {
						CommandID       string `json:"command_id"`
						CommandRejected bool   `json:"command_rejected"`
						RetryAllowed    bool   `json:"retry_allowed"`
					} `json:"details"`
				} `json:"error"`
			}
			if json.Unmarshal(b, &failure) == nil && reaformUUID(failure.Error.Details.CommandID) && failure.Error.Details.CommandRejected {
				remoteError.CommandID = failure.Error.Details.CommandID
				remoteError.CommandRejected = true
				remoteError.RetryAllowed = failure.Error.Details.RetryAllowed
			}
		}
		return remoteError
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &envelope) != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, result) != nil {
		return errors.New("invalid_remote_response")
	}
	return nil
}

func (a *App) reaformCapabilities(ctx context.Context) (reaformCapability, error) {
	var c reaformCapability
	if err := a.reaformRemote(ctx, http.MethodGet, "/capabilities", nil, &c); err != nil {
		return c, err
	}
	configured := a.Config.ReaformPilot
	if c.ContractVersion != 1 || c.OrganizationID != configured.RemoteOrganizationID || c.SiteID != configured.RemoteSiteID || c.DurationMinutes != 30 || c.VisitType != "in_person" || strings.TrimSpace(c.SiteName) == "" || len(c.SiteName) > 200 {
		return c, errors.New("clinic_binding_mismatch")
	}
	return c, nil
}

func reaformQuery(value string) string { return url.QueryEscape(value) }
