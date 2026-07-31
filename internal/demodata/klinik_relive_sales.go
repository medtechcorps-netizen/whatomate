package demodata

import (
	"crypto/md5" //nolint:gosec // MD5 matches the deterministic UUID convention used by the existing SQL fixtures.
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	// KlinikReliveSalesDataset is the immutable ownership marker for these rows.
	KlinikReliveSalesDataset = "klinik-relive-sales-v1"

	// KlinikReliveOrganizationID and KlinikReliveOrganizationName deliberately
	// make the production command single-tenant. The CLI additionally requires
	// both values to be supplied explicitly before it will open a transaction.
	KlinikReliveOrganizationID   = "c73f761f-5154-4fe1-9a13-06bae570277a"
	KlinikReliveOrganizationName = "Klinik Relive"

	KlinikReliveOwnerEmail = "admintest@rereply.com"
	LogicalAccountName     = "[MOCK] DEMO"

	omnichannelDataset = "klinik-relive-omnichannel-v1"
	crmDataset         = "klinik-relive-crm-v2"
)

var (
	terminalCallStatuses = []models.CallStatus{
		models.CallStatusCompleted,
		models.CallStatusMissed,
		models.CallStatusRejected,
		models.CallStatusFailed,
	}
	terminalTransferStatuses = []models.CallTransferStatus{
		models.CallTransferStatusCompleted,
		models.CallTransferStatusAbandoned,
		models.CallTransferStatusNoAnswer,
	}
	metaAnalyticsTypes = []string{
		"analytics",
		"pricing_analytics",
		"template_analytics",
		"call_analytics",
	}
)

// ContactReference is the small, immutable subset of a CRM contact used by the
// pure fixture builders.
type ContactReference struct {
	ID          uuid.UUID
	PhoneNumber string
}

// FixtureReferences contains prerequisites loaded and verified by the seeder.
type FixtureReferences struct {
	OrganizationID uuid.UUID
	OwnerID       uuid.UUID
	TeamID        uuid.UUID
	Contacts      []ContactReference
	AgentIDs      []uuid.UUID
}

// FixtureSet is a complete, side-effect-free sales showcase dataset.
type FixtureSet struct {
	CannedResponses []models.CannedResponse
	IVRFlows        []models.IVRFlow
	CallLogs        []models.CallLog
	CallTransfers   []models.CallTransfer
	MetaSnapshots   []models.MetaAnalyticsSnapshot
}

// BuildKlinikReliveSalesFixtures builds deterministic fixture rows. The IDs are
// stable across runs; timestamps are anchored to the current UTC day so the
// dashboard remains useful for rolling 30-day screenshots.
func BuildKlinikReliveSalesFixtures(now time.Time, refs FixtureReferences) (FixtureSet, error) {
	if err := validateReferences(refs); err != nil {
		return FixtureSet{}, err
	}

	anchor := utcDay(now)
	canned := buildCannedResponses(anchor, refs)
	ivr := buildIVRFlows(anchor, refs)
	calls := buildCallLogs(anchor.AddDate(0, 0, -1), refs, ivr)
	transfers := buildCallTransfers(anchor, refs, calls)
	snapshots := buildMetaAnalyticsSnapshots(anchor, refs)

	fixtures := FixtureSet{
		CannedResponses: canned,
		IVRFlows:        ivr,
		CallLogs:        calls,
		CallTransfers:   transfers,
		MetaSnapshots:   snapshots,
	}
	if err := ValidateKlinikReliveSalesFixtures(fixtures, refs); err != nil {
		return FixtureSet{}, err
	}
	return fixtures, nil
}

func validateReferences(refs FixtureReferences) error {
	if refs.OrganizationID == uuid.Nil {
		return fmt.Errorf("organization ID is required")
	}
	if refs.OwnerID == uuid.Nil {
		return fmt.Errorf("owner ID is required")
	}
	if refs.TeamID == uuid.Nil {
		return fmt.Errorf("inactive omnichannel team ID is required")
	}
	if len(refs.Contacts) != 30 {
		return fmt.Errorf("expected 30 CRM mock contacts, got %d", len(refs.Contacts))
	}
	if len(refs.AgentIDs) != 3 {
		return fmt.Errorf("expected 3 inactive omnichannel agents, got %d", len(refs.AgentIDs))
	}

	seen := make(map[uuid.UUID]struct{}, len(refs.Contacts)+len(refs.AgentIDs))
	for _, contact := range refs.Contacts {
		if contact.ID == uuid.Nil || strings.TrimSpace(contact.PhoneNumber) == "" {
			return fmt.Errorf("CRM mock contact reference is incomplete")
		}
		if _, ok := seen[contact.ID]; ok {
			return fmt.Errorf("duplicate CRM mock contact reference %s", contact.ID)
		}
		seen[contact.ID] = struct{}{}
	}
	for _, agentID := range refs.AgentIDs {
		if agentID == uuid.Nil {
			return fmt.Errorf("inactive omnichannel agent ID is required")
		}
		if _, ok := seen[agentID]; ok {
			return fmt.Errorf("duplicate fixture reference %s", agentID)
		}
		seen[agentID] = struct{}{}
	}
	return nil
}

func buildCannedResponses(anchor time.Time, refs FixtureReferences) []models.CannedResponse {
	type cannedSpec struct {
		name     string
		shortcut string
		category string
		content  string
		usage    int
	}
	specs := []cannedSpec{
		{"[MOCK] Welcome & Consent", "mock-welcome", "greeting", "[MOCK] Hi! Thank you for contacting Klinik Relive. May I ask a few questions so our care team can review your enquiry? No booking or treatment decision is made by this demo response.", 58},
		{"[MOCK] New Lead Qualification", "mock-qualify", "sales", "[MOCK] To help our team understand your goals, please share your main concern, preferred appointment timing, and whether this is your first visit.", 46},
		{"[MOCK] Appointment Options", "mock-slots", "sales", "[MOCK] We can prepare several appointment options for human review. Which weekday or Saturday time window usually works best for you?", 41},
		{"[MOCK] Treatment Interest", "mock-treatment", "sales", "[MOCK] Which area are you most interested in discussing: metabolic wellness, preventive care, an initial assessment, or follow-up support?", 39},
		{"[MOCK] Package Overview", "mock-package", "sales", "[MOCK] I can share a synthetic package overview for this CRM demonstration. A Klinik Relive team member must verify all real inclusions, suitability, and pricing.", 34},
		{"[MOCK] Pricing Follow-up", "mock-pricing", "sales", "[MOCK] Thank you. I have noted your pricing question for a team member. This mock response does not quote, charge, reserve, or confirm anything.", 31},
		{"[MOCK] Booking Confirmation Draft", "mock-booking", "closing", "[MOCK] Your preferred date and time have been recorded as an enquiry only. A team member will confirm availability before any appointment exists.", 29},
		{"[MOCK] Pre-visit Checklist", "mock-previsit", "support", "[MOCK] Before a confirmed visit, the clinic may provide preparation guidance. Please wait for instructions verified by a Klinik Relive team member.", 24},
		{"[MOCK] Missed Call Follow-up", "mock-missed-call", "support", "[MOCK] We noticed a missed-call record in this synthetic demonstration. Please tell us a convenient callback window; no callback has been scheduled.", 22},
		{"[MOCK] Aftercare Check-in", "mock-aftercare", "support", "[MOCK] This is a demonstration aftercare check-in. For any real or urgent medical concern, contact the clinic through its verified channels.", 18},
		{"[MOCK] Re-engagement", "mock-reengage", "sales", "[MOCK] It has been a while since this synthetic enquiry was updated. Would you like a team member to review the next step with you?", 15},
		{"[MOCK] Human Handover", "mock-handover", "general", "[MOCK] I have prepared this conversation for human review. No message, call, booking, payment, or provider action has been triggered.", 12},
	}

	rows := make([]models.CannedResponse, 0, len(specs))
	for i, spec := range specs {
		created := anchor.AddDate(0, 0, -90+i)
		rows = append(rows, models.CannedResponse{
			BaseModel: models.BaseModel{
				ID:        deterministicUUID(fmt.Sprintf("%s-canned-%02d", KlinikReliveSalesDataset, i+1)),
				CreatedAt: created,
				UpdatedAt: anchor,
			},
			OrganizationID: refs.OrganizationID,
			Name:           spec.name,
			Shortcut:       spec.shortcut,
			Content:        spec.content,
			Category:       spec.category,
			IsActive:       false,
			UsageCount:     spec.usage,
			Buttons:        models.JSONBArray{},
			CreatedByID:    refs.OwnerID,
		})
	}
	return rows
}

func buildIVRFlows(anchor time.Time, refs FixtureReferences) []models.IVRFlow {
	type ivrSpec struct {
		name        string
		description string
		nodeCount   int
		menuLabels  []string
		storeKeys   []string
	}
	specs := []ivrSpec{
		{"[MOCK] New Patient Lead Intake", "[MOCK] Disabled historical lead-intake example. It cannot receive or route a live call.", 5, []string{"New patient", "Returning patient"}, []string{"preferred_day", "preferred_time"}},
		{"[MOCK] Appointment Request", "[MOCK] Disabled appointment-request example. It records no booking and performs no provider action.", 5, []string{"Weekday", "Saturday"}, []string{"preferred_day", "preferred_hour"}},
		{"[MOCK] Treatment & Package Enquiry", "[MOCK] Disabled treatment-interest example for CRM screenshots only.", 5, []string{"Treatment enquiry", "Package enquiry"}, []string{"interest_code", "budget_band"}},
		{"[MOCK] Existing Patient Follow-up", "[MOCK] Disabled follow-up example. A human must review every real patient request.", 4, []string{"Clinical follow-up", "Package follow-up"}, []string{"followup_reference"}},
		{"[MOCK] After-hours Callback Capture", "[MOCK] Disabled callback-preference example. It never schedules or initiates a callback.", 4, []string{"Morning", "Afternoon"}, []string{"callback_window"}},
	}

	rows := make([]models.IVRFlow, 0, len(specs))
	for i, spec := range specs {
		created := anchor.AddDate(0, 0, -75+i)
		rows = append(rows, models.IVRFlow{
			BaseModel: models.BaseModel{
				ID:        deterministicUUID(fmt.Sprintf("%s-ivr-%02d", KlinikReliveSalesDataset, i+1)),
				CreatedAt: created,
				UpdatedAt: anchor,
			},
			OrganizationID:  refs.OrganizationID,
			WhatsAppAccount: LogicalAccountName,
			Name:            spec.name,
			Description:     spec.description,
			IsActive:        false,
			IsCallStart:     false,
			IsOutgoingEnd:   false,
			Menu:            buildSafeIVRGraph(i+1, spec.nodeCount, spec.menuLabels, spec.storeKeys),
			WelcomeAudioURL: "",
			CreatedByID:     uuidPtr(refs.OwnerID),
			UpdatedByID:     uuidPtr(refs.OwnerID),
		})
	}
	return rows
}

func buildSafeIVRGraph(flowNumber, nodeCount int, menuLabels, storeKeys []string) models.JSONB {
	prefix := fmt.Sprintf("mock-%02d", flowNumber)
	greetingID := prefix + "-greeting"
	menuID := prefix + "-menu"
	hangupID := prefix + "-hangup"

	nodes := []any{
		map[string]any{
			"id": greetingID, "type": "greeting", "label": "[MOCK] Welcome",
			"position": map[string]any{"x": 80, "y": 80},
			"config":   map[string]any{"audio_file": "", "interruptible": false},
		},
		map[string]any{
			"id": menuID, "type": "menu", "label": "[MOCK] Lead choice",
			"position": map[string]any{"x": 320, "y": 80},
			"config": map[string]any{
				"audio_file": "", "timeout_seconds": 10, "max_retries": 2,
				"options": map[string]any{
					"1": map[string]any{"label": menuLabels[0]},
					"2": map[string]any{"label": menuLabels[1]},
				},
			},
		},
	}

	edges := []any{
		map[string]any{"from": greetingID, "to": menuID, "condition": "default"},
	}

	gatherCount := nodeCount - 3
	for i := 0; i < gatherCount; i++ {
		gatherID := fmt.Sprintf("%s-gather-%d", prefix, i+1)
		storeAs := storeKeys[min(i, len(storeKeys)-1)]
		nodes = append(nodes, map[string]any{
			"id": gatherID, "type": "gather", "label": fmt.Sprintf("[MOCK] Capture preference %d", i+1),
			"position": map[string]any{"x": 560, "y": 20 + i*140},
			"config": map[string]any{
				"audio_file": "", "max_digits": 4, "terminator": "#",
				"timeout_seconds": 10, "max_retries": 2, "store_as": storeAs,
			},
		})
		edges = append(edges,
			map[string]any{"from": menuID, "to": gatherID, "condition": fmt.Sprintf("digit:%d", i+1)},
			map[string]any{"from": gatherID, "to": hangupID, "condition": "default"},
			map[string]any{"from": gatherID, "to": hangupID, "condition": "max_retries"},
		)
	}
	if gatherCount == 1 {
		edges = append(edges, map[string]any{"from": menuID, "to": prefix + "-gather-1", "condition": "digit:2"})
	}
	edges = append(edges,
		map[string]any{"from": menuID, "to": hangupID, "condition": "timeout"},
		map[string]any{"from": menuID, "to": hangupID, "condition": "max_retries"},
	)
	nodes = append(nodes, map[string]any{
		"id": hangupID, "type": "hangup", "label": "[MOCK] End",
		"position": map[string]any{"x": 820, "y": 80},
		"config":   map[string]any{"audio_file": ""},
	})

	return models.JSONB{
		"version":    2,
		"nodes":      nodes,
		"edges":      edges,
		"entry_node": greetingID,
	}
}

func buildCallLogs(anchor time.Time, refs FixtureReferences, ivr []models.IVRFlow) []models.CallLog {
	rows := make([]models.CallLog, 0, 36)
	for i := 0; i < 36; i++ {
		direction := models.CallDirectionIncoming
		status := incomingStatus(i)
		daysAgo := i
		if i >= 30 {
			direction = models.CallDirectionOutgoing
			status = outgoingStatus(i)
			daysAgo = 35 + ((i - 30) * 8)
		}

		started := anchor.AddDate(0, 0, -daysAgo).Add(time.Duration(9+(i%8))*time.Hour + time.Duration((i*7)%60)*time.Minute)
		ended := started.Add(time.Duration(25+(i%25)) * time.Second)
		var answered *time.Time
		duration := 0
		disconnectedBy := models.DisconnectedBySystem
		errorMessage := ""
		if status == models.CallStatusCompleted {
			answerTime := started.Add(time.Duration(4+(i%9)) * time.Second)
			duration = 180 + ((i * 37) % 420)
			endTime := answerTime.Add(time.Duration(duration) * time.Second)
			answered = &answerTime
			ended = endTime
			switch i % 3 {
			case 0:
				disconnectedBy = models.DisconnectedByClient
			case 1:
				disconnectedBy = models.DisconnectedByAgent
			default:
				disconnectedBy = models.DisconnectedBySystem
			}
		} else if status == models.CallStatusRejected {
			disconnectedBy = models.DisconnectedByClient
		} else if status == models.CallStatusFailed {
			errorMessage = "[MOCK] Synthetic provider failure; no provider request was made."
		}

		contact := refs.Contacts[i%len(refs.Contacts)]
		var agentID *uuid.UUID
		if i < 18 {
			agentID = uuidPtr(refs.AgentIDs[i%len(refs.AgentIDs)])
		}
		var ivrFlowID *uuid.UUID
		var ivrPath models.JSONB
		if direction == models.CallDirectionIncoming {
			flow := ivr[i%len(ivr)]
			ivrFlowID = uuidPtr(flow.ID)
			ivrPath = historicalIVRPath(flow, i, status)
		}

		callID := deterministicUUID(fmt.Sprintf("%s-call-%04d", KlinikReliveSalesDataset, i+1))
		rows = append(rows, models.CallLog{
			BaseModel: models.BaseModel{
				ID:        callID,
				CreatedAt: started,
				UpdatedAt: ended,
			},
			OrganizationID:    refs.OrganizationID,
			WhatsAppAccount:   LogicalAccountName,
			ContactID:         contact.ID,
			WhatsAppCallID:    fmt.Sprintf("MOCK-KLINIK-RELIVE-CALL-%04d", i+1),
			CallerPhone:       contact.PhoneNumber,
			Direction:         direction,
			Status:            status,
			Duration:          duration,
			IVRFlowID:         ivrFlowID,
			IVRPath:           ivrPath,
			AgentID:           agentID,
			StartedAt:         timePtr(started),
			AnsweredAt:        answered,
			EndedAt:           timePtr(ended),
			DisconnectedBy:    disconnectedBy,
			ErrorMessage:      errorMessage,
			RecordingS3Key:    "",
			RecordingDuration: 0,
			RecordingError:    "",
		})
	}
	return rows
}

func incomingStatus(index int) models.CallStatus {
	switch {
	case index < 16:
		return models.CallStatusCompleted
	case index < 22:
		return models.CallStatusMissed
	case index < 25:
		return models.CallStatusRejected
	default:
		return models.CallStatusFailed
	}
}

func outgoingStatus(index int) models.CallStatus {
	switch {
	case index < 34:
		return models.CallStatusCompleted
	case index == 34:
		return models.CallStatusRejected
	default:
		return models.CallStatusFailed
	}
}

func historicalIVRPath(flow models.IVRFlow, index int, status models.CallStatus) models.JSONB {
	outcome := "completed"
	if status != models.CallStatusCompleted {
		outcome = string(status)
	}
	return models.JSONB{
		"mock_dataset": KlinikReliveSalesDataset,
		"outcome":      outcome,
		"steps": []any{
			map[string]any{"node": fmt.Sprintf("mock-%02d-greeting", (index%5)+1), "type": "greeting", "label": "[MOCK] Welcome"},
			map[string]any{"node": fmt.Sprintf("mock-%02d-menu", (index%5)+1), "type": "menu", "label": "[MOCK] Lead choice", "digit": fmt.Sprintf("%d", (index%2)+1)},
		},
		"flow_name": flow.Name,
	}
}

func buildCallTransfers(anchor time.Time, refs FixtureReferences, calls []models.CallLog) []models.CallTransfer {
	callIndexes := []int{0, 1, 2, 3, 4, 5, 16, 17, 18, 25, 26, 27}
	rows := make([]models.CallTransfer, 0, len(callIndexes))
	for i, callIndex := range callIndexes {
		call := calls[callIndex]
		transferred := call.StartedAt.Add(time.Duration(18+(i%8)) * time.Second)
		completed := transferred.Add(time.Duration(45+(i*9)) * time.Second)
		status := models.CallTransferStatusCompleted
		holdDuration := 8 + (i * 3)
		talkDuration := 120 + (i * 31)
		var connectedAt *time.Time
		var agentID *uuid.UUID
		if i < 6 {
			connected := transferred.Add(time.Duration(holdDuration) * time.Second)
			connectedAt = &connected
			completed = connected.Add(time.Duration(talkDuration) * time.Second)
			agentID = uuidPtr(refs.AgentIDs[i%len(refs.AgentIDs)])
		} else {
			holdDuration = 20 + (i * 2)
			talkDuration = 0
			if i < 9 {
				status = models.CallTransferStatusAbandoned
			} else {
				status = models.CallTransferStatusNoAnswer
			}
		}

		rows = append(rows, models.CallTransfer{
			BaseModel: models.BaseModel{
				ID:        deterministicUUID(fmt.Sprintf("%s-transfer-%03d", KlinikReliveSalesDataset, i+1)),
				CreatedAt: transferred,
				UpdatedAt: completed,
			},
			OrganizationID:    refs.OrganizationID,
			CallLogID:         call.ID,
			WhatsAppCallID:    call.WhatsAppCallID,
			CallerPhone:       call.CallerPhone,
			ContactID:         call.ContactID,
			WhatsAppAccount:   LogicalAccountName,
			Status:            status,
			TeamID:            uuidPtr(refs.TeamID),
			AgentID:           agentID,
			InitiatingAgentID: nil,
			TransferredAt:     transferred,
			ConnectedAt:       connectedAt,
			CompletedAt:       timePtr(completed),
			HoldDuration:      holdDuration,
			TalkDuration:      talkDuration,
			IVRPath:           call.IVRPath,
			TriedAgentIDs:     models.JSONBArray{},
		})
	}
	_ = anchor // The call timestamps provide the transfer time anchors.
	return rows
}

func buildMetaAnalyticsSnapshots(anchor time.Time, refs FixtureReferences) []models.MetaAnalyticsSnapshot {
	periodEnd := anchor
	periodStart := periodEnd.AddDate(0, 0, -30)
	accountID := deterministicUUID(KlinikReliveSalesDataset + "-logical-account")
	templateNames := models.JSONB{
		"mock-template-appointment": "[MOCK] Appointment Reminder",
		"mock-template-followup":    "[MOCK] Care Follow-up",
		"mock-template-package":     "[MOCK] Package Information",
		"mock-template-welcome":     "[MOCK] Welcome & Consent",
		"mock-template-aftercare":   "[MOCK] Aftercare Check-in",
	}

	rows := make([]models.MetaAnalyticsSnapshot, 0, len(metaAnalyticsTypes))
	for _, analyticsType := range metaAnalyticsTypes {
		names := models.JSONB{}
		if analyticsType == "template_analytics" {
			names = templateNames
		}
		rows = append(rows, models.MetaAnalyticsSnapshot{
			BaseModel: models.BaseModel{
				ID:        deterministicUUID(KlinikReliveSalesDataset + "-meta-" + analyticsType),
				CreatedAt: anchor,
				UpdatedAt: anchor,
			},
			OrganizationID: refs.OrganizationID,
			AccountID:      accountID,
			AccountName:    LogicalAccountName,
			PhoneID:        "MOCK-KLINIK-RELIVE-PHONE",
			Dataset:        KlinikReliveSalesDataset,
			AnalyticsType:  analyticsType,
			Granularity:    "DAY",
			PeriodStart:    periodStart,
			PeriodEnd:      periodEnd,
			Payload:        analyticsPayload(accountID, analyticsType, periodStart),
			TemplateNames:  names,
			IsMock:         true,
		})
	}
	return rows
}

func analyticsPayload(accountID uuid.UUID, analyticsType string, periodStart time.Time) models.JSONB {
	points := make([]any, 0, 30)
	for i := 0; i < 30; i++ {
		start := periodStart.AddDate(0, 0, i)
		end := start.AddDate(0, 0, 1)
		switch analyticsType {
		case "analytics":
			sent := 108
			delivered := 102
			if i < 18 {
				delivered++
			}
			points = append(points, map[string]any{
				"start": start.Unix(), "end": end.Unix(),
				"sent": sent, "delivered": delivered,
			})
		case "pricing_analytics":
			if i >= 8 {
				continue
			}
			volumes := []int{890, 250, 980, 650, 310, 90, 45, 25}
			costs := []float64{0, 0, 46.06, 11.05, 6.82, 7.56, 1.35, 0}
			countries := []string{"MY", "MY", "MY", "MY", "MY", "SG", "SG", "MY"}
			pricingTypes := []string{
				"FREE_CUSTOMER_SERVICE",
				"FREE_ENTRY_POINT",
				"REGULAR",
				"REGULAR",
				"REGULAR",
				"REGULAR",
				"REGULAR",
				"REGULAR",
			}
			categories := []string{
				"SERVICE",
				"MARKETING",
				"MARKETING",
				"UTILITY",
				"AUTHENTICATION",
				"MARKETING",
				"UTILITY",
				"SERVICE",
			}
			start = periodStart.AddDate(0, 0, 22+i)
			end = start.AddDate(0, 0, 1)
			points = append(points, map[string]any{
				"start": start.Unix(), "end": end.Unix(),
				"volume": volumes[i], "cost": costs[i],
				"country": countries[i], "pricing_type": pricingTypes[i],
				"pricing_category": categories[i%len(categories)], "tier": "STANDARD",
			})
		case "template_analytics":
			templateIDs := []string{"mock-template-appointment", "mock-template-followup", "mock-template-package", "mock-template-welcome", "mock-template-aftercare"}
			sent := 104
			delivered := 99
			if i < 11 {
				delivered++
			}
			read := 85
			if i < 28 {
				read++
			}
			clicked := 16
			if i == 0 {
				clicked++
			}
			points = append(points, map[string]any{
				"start": start.Unix(), "end": end.Unix(), "template_id": templateIDs[i%len(templateIDs)],
				"sent": sent, "delivered": delivered, "read": read,
				"replied": 24,
				"clicked": []any{
					map[string]any{"type": "quick_reply_button", "button_content": "View details", "count": clicked},
				},
				"cost": []any{
					map[string]any{"type": "amount_spent", "value": 2.083},
				},
			})
		case "call_analytics":
			count := 1
			direction := "BUSINESS_INITIATED"
			if i%2 == 0 {
				count = 2
				direction = "USER_INITIATED"
				if i == 0 {
					count = 3
				}
			} else if i == 1 {
				count = 3
			}
			points = append(points, map[string]any{
				"start": start.Unix(), "end": end.Unix(),
				"count": count, "cost": float64(count) * 0.1425,
				"average_duration": 252,
				"direction":        direction,
			})
		}
	}

	return models.JSONB{
		"id": accountID.String(),
		analyticsType: map[string]any{
			"granularity": "DAY",
			"data_points": points,
		},
	}
}

// ValidateKlinikReliveSalesFixtures rejects any operational calling state,
// provider-bearing field, or non-mock ownership before the database is touched.
func ValidateKlinikReliveSalesFixtures(fixtures FixtureSet, refs FixtureReferences) error {
	if err := validateReferences(refs); err != nil {
		return err
	}
	if len(fixtures.CannedResponses) != 12 ||
		len(fixtures.IVRFlows) != 5 ||
		len(fixtures.CallLogs) != 36 ||
		len(fixtures.CallTransfers) != 12 ||
		len(fixtures.MetaSnapshots) != 4 {
		return fmt.Errorf(
			"unexpected fixture counts: canned=%d ivr=%d calls=%d transfers=%d meta=%d",
			len(fixtures.CannedResponses),
			len(fixtures.IVRFlows),
			len(fixtures.CallLogs),
			len(fixtures.CallTransfers),
			len(fixtures.MetaSnapshots),
		)
	}

	ids := make(map[uuid.UUID]string, 69)
	addID := func(id uuid.UUID, kind string) error {
		if id == uuid.Nil {
			return fmt.Errorf("%s fixture has a nil ID", kind)
		}
		if prior, ok := ids[id]; ok {
			return fmt.Errorf("fixture ID %s is shared by %s and %s", id, prior, kind)
		}
		ids[id] = kind
		return nil
	}

	for i := range fixtures.CannedResponses {
		row := &fixtures.CannedResponses[i]
		if err := addID(row.ID, "canned response"); err != nil {
			return err
		}
		if row.OrganizationID != refs.OrganizationID ||
			!strings.HasPrefix(row.Name, "[MOCK] ") ||
			!strings.HasPrefix(row.Shortcut, "mock-") ||
			!strings.HasPrefix(row.Content, "[MOCK] ") ||
			!isAllowedCannedCategory(row.Category) ||
			row.IsActive ||
			row.CreatedByID != refs.OwnerID ||
			len(row.Buttons) != 0 {
			return fmt.Errorf("canned response %s violates mock-only safety", row.ID)
		}
	}

	for i := range fixtures.IVRFlows {
		row := &fixtures.IVRFlows[i]
		if err := addID(row.ID, "IVR flow"); err != nil {
			return err
		}
		if row.OrganizationID != refs.OrganizationID ||
			row.WhatsAppAccount != LogicalAccountName ||
			!strings.HasPrefix(row.Name, "[MOCK] ") ||
			row.IsActive || row.IsCallStart || row.IsOutgoingEnd ||
			row.WelcomeAudioURL != "" {
			return fmt.Errorf("IVR flow %s is not safely disabled", row.ID)
		}
		if err := validateSafeIVRGraph(row.Menu); err != nil {
			return fmt.Errorf("IVR flow %s: %w", row.ID, err)
		}
	}

	statusCount := map[models.CallStatus]int{}
	directionCount := map[models.CallDirection]int{}
	assignedCount := map[uuid.UUID]int{}
	ivrCount := map[uuid.UUID]int{}
	for i := range fixtures.CallLogs {
		row := &fixtures.CallLogs[i]
		if err := addID(row.ID, "call log"); err != nil {
			return err
		}
		if row.OrganizationID != refs.OrganizationID ||
			row.WhatsAppAccount != LogicalAccountName ||
			!strings.HasPrefix(row.WhatsAppCallID, "MOCK-KLINIK-RELIVE-CALL-") ||
			row.EndedAt == nil ||
			row.RecordingS3Key != "" ||
			row.RecordingDuration != 0 ||
			row.RecordingError != "" ||
			!slices.Contains(terminalCallStatuses, row.Status) {
			return fmt.Errorf("call log %s violates terminal mock-call safety", row.ID)
		}
		if row.Status == models.CallStatusCompleted {
			if row.AnsweredAt == nil || row.Duration <= 0 {
				return fmt.Errorf("completed call log %s has inconsistent timing", row.ID)
			}
		} else if row.AnsweredAt != nil || row.Duration != 0 {
			return fmt.Errorf("non-completed call log %s has answered timing", row.ID)
		}
		if row.Status == models.CallStatusFailed {
			if !strings.HasPrefix(row.ErrorMessage, "[MOCK] ") {
				return fmt.Errorf("failed call log %s lacks an explicit mock error", row.ID)
			}
		} else if row.ErrorMessage != "" {
			return fmt.Errorf("non-failed call log %s unexpectedly has an error", row.ID)
		}
		if row.Direction == models.CallDirectionIncoming {
			if row.IVRFlowID == nil {
				return fmt.Errorf("incoming call log %s has no historical IVR", row.ID)
			}
			ivrCount[*row.IVRFlowID]++
		} else if row.IVRFlowID != nil {
			return fmt.Errorf("outgoing call log %s unexpectedly references an IVR", row.ID)
		}
		if row.AgentID != nil {
			if !slices.Contains(refs.AgentIDs, *row.AgentID) {
				return fmt.Errorf("call log %s references a non-fixture agent", row.ID)
			}
			assignedCount[*row.AgentID]++
		}
		statusCount[row.Status]++
		directionCount[row.Direction]++
	}
	if statusCount[models.CallStatusCompleted] != 20 ||
		statusCount[models.CallStatusMissed] != 6 ||
		statusCount[models.CallStatusRejected] != 4 ||
		statusCount[models.CallStatusFailed] != 6 ||
		directionCount[models.CallDirectionIncoming] != 30 ||
		directionCount[models.CallDirectionOutgoing] != 6 {
		return fmt.Errorf("call status or direction distribution changed")
	}
	for _, agentID := range refs.AgentIDs {
		if assignedCount[agentID] != 6 {
			return fmt.Errorf("agent %s must have exactly 6 historical calls", agentID)
		}
	}
	for _, flow := range fixtures.IVRFlows {
		if ivrCount[flow.ID] != 6 {
			return fmt.Errorf("IVR flow %s must have exactly 6 historical calls", flow.ID)
		}
	}

	transferCount := map[models.CallTransferStatus]int{}
	callIDs := make(map[uuid.UUID]struct{}, len(fixtures.CallLogs))
	for _, call := range fixtures.CallLogs {
		callIDs[call.ID] = struct{}{}
	}
	for i := range fixtures.CallTransfers {
		row := &fixtures.CallTransfers[i]
		if err := addID(row.ID, "call transfer"); err != nil {
			return err
		}
		if row.OrganizationID != refs.OrganizationID ||
			row.WhatsAppAccount != LogicalAccountName ||
			row.TeamID == nil || *row.TeamID != refs.TeamID ||
			row.InitiatingAgentID != nil ||
			row.CompletedAt == nil ||
			!slices.Contains(terminalTransferStatuses, row.Status) ||
			len(row.TriedAgentIDs) != 0 {
			return fmt.Errorf("call transfer %s violates terminal mock-transfer safety", row.ID)
		}
		if _, ok := callIDs[row.CallLogID]; !ok {
			return fmt.Errorf("call transfer %s references a non-fixture call", row.ID)
		}
		if row.Status == models.CallTransferStatusCompleted {
			if row.ConnectedAt == nil || row.AgentID == nil || row.TalkDuration <= 0 {
				return fmt.Errorf("completed call transfer %s has inconsistent timing", row.ID)
			}
		} else if row.ConnectedAt != nil || row.AgentID != nil || row.TalkDuration != 0 {
			return fmt.Errorf("non-connected call transfer %s has connected state", row.ID)
		}
		transferCount[row.Status]++
	}
	if transferCount[models.CallTransferStatusCompleted] != 6 ||
		transferCount[models.CallTransferStatusAbandoned] != 3 ||
		transferCount[models.CallTransferStatusNoAnswer] != 3 {
		return fmt.Errorf("call transfer distribution changed")
	}

	metaTypes := make(map[string]int, len(fixtures.MetaSnapshots))
	logicalAccountID := deterministicUUID(KlinikReliveSalesDataset + "-logical-account")
	for i := range fixtures.MetaSnapshots {
		row := &fixtures.MetaSnapshots[i]
		if err := addID(row.ID, "Meta analytics snapshot"); err != nil {
			return err
		}
		if row.OrganizationID != refs.OrganizationID ||
			row.AccountID != logicalAccountID ||
			row.AccountName != LogicalAccountName ||
			row.PhoneID != "MOCK-KLINIK-RELIVE-PHONE" ||
			row.Granularity != "DAY" ||
			!row.IsMock ||
			row.Dataset != KlinikReliveSalesDataset ||
			row.PeriodEnd.Sub(row.PeriodStart) != 30*24*time.Hour ||
			!slices.Contains(metaAnalyticsTypes, row.AnalyticsType) {
			return fmt.Errorf("Meta analytics snapshot %s violates mock-only safety", row.ID)
		}
		if _, ok := row.Payload[row.AnalyticsType]; !ok {
			return fmt.Errorf("Meta analytics snapshot %s has no matching payload", row.ID)
		}
		metaTypes[row.AnalyticsType]++
	}
	for _, analyticsType := range metaAnalyticsTypes {
		if metaTypes[analyticsType] != 1 {
			return fmt.Errorf("expected one %s snapshot", analyticsType)
		}
	}
	if err := validateMetaShowcaseTotals(fixtures.MetaSnapshots); err != nil {
		return err
	}
	return nil
}

func validateMetaShowcaseTotals(rows []models.MetaAnalyticsSnapshot) error {
	byType := make(map[string]models.MetaAnalyticsSnapshot, len(rows))
	for _, row := range rows {
		byType[row.AnalyticsType] = row
	}

	messagingPoints, err := snapshotDataPoints(byType["analytics"], "analytics")
	if err != nil {
		return err
	}
	var messagingSent, messagingDelivered int64
	for _, point := range messagingPoints {
		messagingSent += jsonInt64(point, "sent")
		messagingDelivered += jsonInt64(point, "delivered")
	}
	if messagingSent != 3240 || messagingDelivered != 3078 {
		return fmt.Errorf("messaging analytics totals changed: sent=%d delivered=%d", messagingSent, messagingDelivered)
	}

	pricingPoints, err := snapshotDataPoints(byType["pricing_analytics"], "pricing_analytics")
	if err != nil {
		return err
	}
	var pricingVolume int64
	var pricingFreeVolume int64
	var pricingPaidVolume int64
	var pricingCost float64
	pricingCountries := make(map[string]struct{})
	for _, point := range pricingPoints {
		volume := jsonInt64(point, "volume")
		pricingVolume += volume
		pricingCost += jsonFloat64(point, "cost")
		pricingType, _ := point["pricing_type"].(string)
		if pricingType == "FREE_CUSTOMER_SERVICE" || pricingType == "FREE_ENTRY_POINT" {
			pricingFreeVolume += volume
		} else {
			pricingPaidVolume += volume
		}
		if country, _ := point["country"].(string); country != "" {
			pricingCountries[country] = struct{}{}
		}
	}
	if pricingVolume != 3240 ||
		pricingFreeVolume != 1140 ||
		pricingPaidVolume != 2100 ||
		len(pricingCountries) != 2 ||
		math.Abs(pricingCost-72.84) > 0.000001 {
		return fmt.Errorf(
			"pricing analytics totals changed: volume=%d free=%d paid=%d countries=%d cost=%.6f",
			pricingVolume,
			pricingFreeVolume,
			pricingPaidVolume,
			len(pricingCountries),
			pricingCost,
		)
	}

	templateSnapshot := byType["template_analytics"]
	templatePoints, err := snapshotDataPoints(templateSnapshot, "template_analytics")
	if err != nil {
		return err
	}
	var templateSent, templateDelivered, templateRead, templateReplied, templateClicked int64
	var templateCost float64
	templateIDs := make(map[string]struct{})
	for _, point := range templatePoints {
		templateSent += jsonInt64(point, "sent")
		templateDelivered += jsonInt64(point, "delivered")
		templateRead += jsonInt64(point, "read")
		templateReplied += jsonInt64(point, "replied")
		if templateID, _ := point["template_id"].(string); templateID != "" {
			templateIDs[templateID] = struct{}{}
		}
		for _, click := range jsonObjectArray(point["clicked"]) {
			templateClicked += jsonInt64(click, "count")
		}
		for _, cost := range jsonObjectArray(point["cost"]) {
			templateCost += jsonFloat64(cost, "value")
		}
	}
	if templateSent != 3120 ||
		templateDelivered != 2981 ||
		templateRead != 2578 ||
		templateReplied != 720 ||
		templateClicked != 481 ||
		math.Abs(templateCost-62.49) > 0.000001 ||
		len(templateIDs) != 5 ||
		len(templateSnapshot.TemplateNames) != 5 {
		return fmt.Errorf(
			"template analytics totals changed: sent=%d delivered=%d read=%d replied=%d clicked=%d cost=%.6f templates=%d",
			templateSent,
			templateDelivered,
			templateRead,
			templateReplied,
			templateClicked,
			templateCost,
			len(templateIDs),
		)
	}

	callPoints, err := snapshotDataPoints(byType["call_analytics"], "call_analytics")
	if err != nil {
		return err
	}
	var callCount, userCalls, businessCalls, weightedDuration int64
	var callCost float64
	for _, point := range callPoints {
		count := jsonInt64(point, "count")
		callCount += count
		weightedDuration += count * jsonInt64(point, "average_duration")
		callCost += jsonFloat64(point, "cost")
		switch point["direction"] {
		case "USER_INITIATED":
			userCalls += count
		case "BUSINESS_INITIATED":
			businessCalls += count
		default:
			return fmt.Errorf("call analytics contains an unsupported direction")
		}
	}
	if callCount != 48 ||
		userCalls != 31 ||
		businessCalls != 17 ||
		weightedDuration != 48*252 ||
		math.Abs(callCost-6.84) > 0.000001 {
		return fmt.Errorf(
			"call analytics totals changed: calls=%d user=%d business=%d weighted_duration=%d cost=%.6f",
			callCount,
			userCalls,
			businessCalls,
			weightedDuration,
			callCost,
		)
	}
	return nil
}

func snapshotDataPoints(row models.MetaAnalyticsSnapshot, analyticsType string) ([]map[string]any, error) {
	if row.ID == uuid.Nil {
		return nil, fmt.Errorf("%s analytics snapshot is missing", analyticsType)
	}
	section, ok := row.Payload[analyticsType].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s analytics payload has an invalid section", analyticsType)
	}
	points := jsonObjectArray(section["data_points"])
	expectedPoints := 30
	if analyticsType == "pricing_analytics" {
		expectedPoints = 8
	}
	if len(points) != expectedPoints {
		return nil, fmt.Errorf(
			"%s analytics must contain exactly %d showcase points",
			analyticsType,
			expectedPoints,
		)
	}
	return points, nil
}

func jsonObjectArray(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if ok {
			result = append(result, object)
		}
	}
	return result
}

func jsonInt64(object map[string]any, key string) int64 {
	return int64(math.Round(jsonFloat64(object, key)))
}

func jsonFloat64(object map[string]any, key string) float64 {
	switch value := object[key].(type) {
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case float64:
		return value
	case float32:
		return float64(value)
	default:
		return 0
	}
}

func validateSafeIVRGraph(graph models.JSONB) error {
	version := jsonInt64(map[string]any(graph), "version")
	if version != 2 {
		return fmt.Errorf("graph version must be 2")
	}
	entry, ok := graph["entry_node"].(string)
	if !ok || entry == "" {
		return fmt.Errorf("entry node is required")
	}
	nodes, ok := graph["nodes"].([]any)
	if !ok || len(nodes) == 0 {
		return fmt.Errorf("nodes must be a non-empty array")
	}
	allowedTypes := []string{"greeting", "menu", "gather", "hangup"}
	nodeIDs := make(map[string]struct{}, len(nodes))
	terminal := make(map[string]struct{})
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("node has an invalid shape")
		}
		id, _ := node["id"].(string)
		nodeType, _ := node["type"].(string)
		if id == "" || !slices.Contains(allowedTypes, nodeType) {
			return fmt.Errorf("node has an unsafe type or missing ID")
		}
		if _, exists := nodeIDs[id]; exists {
			return fmt.Errorf("duplicate node ID %s", id)
		}
		nodeIDs[id] = struct{}{}
		if nodeType == "hangup" {
			terminal[id] = struct{}{}
		}
		config, _ := node["config"].(map[string]any)
		if err := validateSafeIVRConfig(config); err != nil {
			return err
		}
	}
	if _, ok := nodeIDs[entry]; !ok {
		return fmt.Errorf("entry node does not exist")
	}

	edges, ok := graph["edges"].([]any)
	if !ok {
		return fmt.Errorf("edges must be an array")
	}
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("edge has an invalid shape")
		}
		from, _ := edge["from"].(string)
		to, _ := edge["to"].(string)
		if _, ok := nodeIDs[from]; !ok {
			return fmt.Errorf("edge source %s does not exist", from)
		}
		if _, ok := nodeIDs[to]; !ok {
			return fmt.Errorf("edge destination %s does not exist", to)
		}
		if _, ok := terminal[from]; ok {
			return fmt.Errorf("terminal node %s has an outgoing edge", from)
		}
	}
	return nil
}

func validateSafeIVRConfig(config map[string]any) error {
	for key, value := range config {
		normalized := strings.ToLower(key)
		switch normalized {
		case "greeting_text", "url", "callback_url", "team_id", "agent_id", "flow_id", "schedule":
			if !isEmptyFixtureValue(value) {
				return fmt.Errorf("IVR config field %s may cause an external or live action", key)
			}
		case "audio_file":
			if value != "" {
				return fmt.Errorf("IVR audio_file must be empty")
			}
		}
		switch nested := value.(type) {
		case map[string]any:
			if err := validateSafeIVRConfig(nested); err != nil {
				return err
			}
		case []any:
			for _, item := range nested {
				if itemMap, ok := item.(map[string]any); ok {
					if err := validateSafeIVRConfig(itemMap); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func isEmptyFixtureValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func isAllowedCannedCategory(value string) bool {
	return slices.Contains([]string{"greeting", "support", "sales", "closing", "general"}, value)
}

func deterministicUUID(key string) uuid.UUID {
	sum := md5.Sum([]byte(key))
	return uuid.UUID(sum)
}

func utcDay(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func uuidPtr(value uuid.UUID) *uuid.UUID {
	copy := value
	return &copy
}

func timePtr(value time.Time) *time.Time {
	copy := value
	return &copy
}
