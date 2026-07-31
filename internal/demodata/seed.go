package demodata

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var expectedOrganizationID = uuid.MustParse(KlinikReliveOrganizationID)

// SeedOptions controls the production-safe fixture transaction.
type SeedOptions struct {
	OrganizationID   uuid.UUID
	OrganizationName string
	Apply            bool
	Now              time.Time
}

// SeedReport contains non-sensitive counts suitable for terminal output.
type SeedReport struct {
	Applied         bool
	CannedResponses int
	IVRFlows        int
	CallLogs        int
	CallTransfers   int
	MetaSnapshots   int
}

type invariantValue struct {
	Count  int64
	Digest string
}

type prerequisiteRows struct {
	refs FixtureReferences
}

// SeedKlinikReliveSales runs the complete build, ownership checks, upserts, and
// post-write assertions in one serializable transaction. Dry-run is the default:
// the transaction is rolled back unless Apply is explicitly true.
func SeedKlinikReliveSales(ctx context.Context, db *gorm.DB, opts SeedOptions) (SeedReport, error) {
	if db == nil {
		return SeedReport{}, fmt.Errorf("database is required")
	}
	if opts.OrganizationID != expectedOrganizationID {
		return SeedReport{}, fmt.Errorf("organization ID must exactly equal %s", KlinikReliveOrganizationID)
	}
	if opts.OrganizationName != KlinikReliveOrganizationName {
		return SeedReport{}, fmt.Errorf("organization name must exactly equal %q", KlinikReliveOrganizationName)
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	tx := db.WithContext(ctx).Begin(&sql.TxOptions{Isolation: sql.LevelSerializable})
	if tx.Error != nil {
		return SeedReport{}, fmt.Errorf("begin sales fixture transaction: %w", tx.Error)
	}
	finalized := false
	defer func() {
		if !finalized {
			_ = tx.Rollback().Error
		}
	}()

	if err := tx.Exec(
		"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
		KlinikReliveSalesDataset+":"+opts.OrganizationID.String(),
	).Error; err != nil {
		return SeedReport{}, fmt.Errorf("acquire fixture transaction lock: %w", err)
	}
	if err := tx.Exec(
		"SELECT set_config('app.current_organization_id', ?, true)",
		opts.OrganizationID.String(),
	).Error; err != nil {
		return SeedReport{}, fmt.Errorf("set transaction tenant context: %w", err)
	}
	// Hold provider-account tables read-locked for the short fixture
	// transaction. SHARE conflicts with INSERT/UPDATE/DELETE row locks, so a
	// concurrent account creation cannot race the provider-free checks below.
	if err := tx.Exec(
		"LOCK TABLE whatsapp_accounts, channel_accounts, channel_credentials IN SHARE MODE",
	).Error; err != nil {
		return SeedReport{}, fmt.Errorf("lock provider account tables for fixture safety: %w", err)
	}
	if !tx.Migrator().HasTable((&models.MetaAnalyticsSnapshot{}).TableName()) {
		return SeedReport{}, fmt.Errorf("meta_analytics_snapshots is missing; run the approved schema migration first")
	}

	prereqs, err := loadAndValidatePrerequisites(tx, opts)
	if err != nil {
		return SeedReport{}, err
	}
	fixtures, err := BuildKlinikReliveSalesFixtures(opts.Now, prereqs.refs)
	if err != nil {
		return SeedReport{}, fmt.Errorf("build sales fixtures: %w", err)
	}
	if err := assertFixtureOwnership(tx, fixtures, prereqs.refs); err != nil {
		return SeedReport{}, err
	}

	before, err := captureForbiddenState(tx, opts.OrganizationID)
	if err != nil {
		return SeedReport{}, fmt.Errorf("capture pre-seed safety state: %w", err)
	}
	if err := upsertFixtureRows(tx, fixtures); err != nil {
		return SeedReport{}, err
	}
	if err := assertPersistedFixtures(tx, fixtures, prereqs.refs); err != nil {
		return SeedReport{}, err
	}
	after, err := captureForbiddenState(tx, opts.OrganizationID)
	if err != nil {
		return SeedReport{}, fmt.Errorf("capture post-seed safety state: %w", err)
	}
	if err := compareForbiddenState(before, after); err != nil {
		return SeedReport{}, err
	}

	report := SeedReport{
		Applied:         opts.Apply,
		CannedResponses: len(fixtures.CannedResponses),
		IVRFlows:        len(fixtures.IVRFlows),
		CallLogs:        len(fixtures.CallLogs),
		CallTransfers:   len(fixtures.CallTransfers),
		MetaSnapshots:   len(fixtures.MetaSnapshots),
	}
	if !opts.Apply {
		if err := tx.Rollback().Error; err != nil {
			return SeedReport{}, fmt.Errorf("roll back sales fixture dry-run: %w", err)
		}
		finalized = true
		return report, nil
	}
	if err := tx.Commit().Error; err != nil {
		return SeedReport{}, fmt.Errorf("commit sales fixtures: %w", err)
	}
	finalized = true
	return report, nil
}

func loadAndValidatePrerequisites(tx *gorm.DB, opts SeedOptions) (prerequisiteRows, error) {
	var organization models.Organization
	if err := tx.Unscoped().Where("id = ?", opts.OrganizationID).Take(&organization).Error; err != nil {
		return prerequisiteRows{}, fmt.Errorf("load exact target organization: %w", err)
	}
	if organization.DeletedAt.Valid || organization.Name != opts.OrganizationName {
		return prerequisiteRows{}, fmt.Errorf(
			"organization %s is deleted or its name is not exactly %q",
			opts.OrganizationID,
			opts.OrganizationName,
		)
	}

	var owner models.User
	if err := tx.Unscoped().
		Where("organization_id = ? AND email = ?", opts.OrganizationID, KlinikReliveOwnerEmail).
		Take(&owner).Error; err != nil {
		return prerequisiteRows{}, fmt.Errorf("load Klinik Relive fixture owner: %w", err)
	}
	if owner.DeletedAt.Valid || !owner.IsActive {
		return prerequisiteRows{}, fmt.Errorf("fixture owner %s must exist and be active", KlinikReliveOwnerEmail)
	}

	contacts, err := loadCRMContacts(tx, opts.OrganizationID)
	if err != nil {
		return prerequisiteRows{}, err
	}
	agentIDs, err := loadInactiveOmnichannelAgents(tx, opts.OrganizationID)
	if err != nil {
		return prerequisiteRows{}, err
	}
	teamID, err := loadInactiveOmnichannelTeam(tx, opts.OrganizationID, agentIDs)
	if err != nil {
		return prerequisiteRows{}, err
	}
	if err := assertLogicalAccountIsProviderFree(tx, opts.OrganizationID); err != nil {
		return prerequisiteRows{}, err
	}

	return prerequisiteRows{
		refs: FixtureReferences{
			OrganizationID: opts.OrganizationID,
			OwnerID:       owner.ID,
			TeamID:        teamID,
			Contacts:      contacts,
			AgentIDs:      agentIDs,
		},
	}, nil
}

func loadCRMContacts(tx *gorm.DB, organizationID uuid.UUID) ([]ContactReference, error) {
	expected := make([]uuid.UUID, 0, 30)
	sequenceByID := make(map[uuid.UUID]int, 30)
	for sequence := 11; sequence <= 40; sequence++ {
		id := deterministicUUID(fmt.Sprintf("%s-contact-%d", crmDataset, sequence))
		expected = append(expected, id)
		sequenceByID[id] = sequence
	}

	var rows []models.Contact
	if err := tx.Unscoped().Where("id IN ?", expected).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load CRM mock contacts: %w", err)
	}
	if len(rows) != len(expected) {
		return nil, fmt.Errorf("expected 30 deterministic CRM mock contacts, found %d", len(rows))
	}
	byID := make(map[uuid.UUID]models.Contact, len(rows))
	for _, row := range rows {
		sequence, ok := sequenceByID[row.ID]
		if !ok ||
			row.DeletedAt.Valid ||
			row.OrganizationID != organizationID ||
			row.PhoneNumber != fmt.Sprintf("6000000000%02d", sequence) ||
			!strings.HasPrefix(row.ProfileName, "[MOCK] ") ||
			jsonString(row.Metadata, "mock_dataset") != crmDataset ||
			!jsonBool(row.Metadata, "is_mock") {
			return nil, fmt.Errorf("CRM mock contact %s failed the ownership or safety check", row.ID)
		}
		byID[row.ID] = row
	}

	contacts := make([]ContactReference, 0, len(expected))
	for _, id := range expected {
		row, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("CRM mock contact %s is missing", id)
		}
		contacts = append(contacts, ContactReference{ID: row.ID, PhoneNumber: row.PhoneNumber})
	}
	return contacts, nil
}

func loadInactiveOmnichannelAgents(tx *gorm.DB, organizationID uuid.UUID) ([]uuid.UUID, error) {
	expectedEmails := []string{
		"mock.omni.aina@klinik-relive.example.invalid",
		"mock.omni.daniel@klinik-relive.example.invalid",
		"mock.omni.priya@klinik-relive.example.invalid",
	}
	expectedIDs := make([]uuid.UUID, 0, len(expectedEmails))
	for i := range expectedEmails {
		expectedIDs = append(expectedIDs, deterministicUUID(fmt.Sprintf("%s-agent-%d", omnichannelDataset, i+1)))
	}

	var rows []models.User
	if err := tx.Unscoped().Where("id IN ?", expectedIDs).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load inactive omnichannel agents: %w", err)
	}
	if len(rows) != len(expectedIDs) {
		return nil, fmt.Errorf("expected 3 inactive omnichannel agents, found %d", len(rows))
	}
	byID := make(map[uuid.UUID]models.User, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for i, id := range expectedIDs {
		row, ok := byID[id]
		if !ok ||
			row.DeletedAt.Valid ||
			row.OrganizationID != organizationID ||
			row.Email != expectedEmails[i] ||
			row.IsActive ||
			row.IsAvailable ||
			row.IsSuperAdmin ||
			jsonString(row.Settings, "mock_dataset") != omnichannelDataset ||
			!jsonBool(row.Settings, "is_mock") {
			return nil, fmt.Errorf("inactive omnichannel agent %s failed the ownership or safety check", id)
		}
	}
	return expectedIDs, nil
}

func loadInactiveOmnichannelTeam(tx *gorm.DB, organizationID uuid.UUID, agentIDs []uuid.UUID) (uuid.UUID, error) {
	teamID := deterministicUUID(omnichannelDataset + "-team")
	var team models.Team
	if err := tx.Unscoped().Where("id = ?", teamID).Take(&team).Error; err != nil {
		return uuid.Nil, fmt.Errorf("load inactive omnichannel team: %w", err)
	}
	if team.DeletedAt.Valid ||
		team.OrganizationID != organizationID ||
		team.Name != "[MOCK] Omnichannel Care Team (Inactive)" ||
		team.IsActive {
		return uuid.Nil, fmt.Errorf("inactive omnichannel team %s failed the ownership or safety check", teamID)
	}

	expectedMemberIDs := make([]uuid.UUID, 0, len(agentIDs))
	for i := range agentIDs {
		expectedMemberIDs = append(expectedMemberIDs, deterministicUUID(fmt.Sprintf("%s-team-member-%d", omnichannelDataset, i+1)))
	}
	var members []models.TeamMember
	if err := tx.Unscoped().Where("id IN ?", expectedMemberIDs).Find(&members).Error; err != nil {
		return uuid.Nil, fmt.Errorf("load inactive omnichannel team members: %w", err)
	}
	if len(members) != len(expectedMemberIDs) {
		return uuid.Nil, fmt.Errorf("expected 3 inactive omnichannel team members, found %d", len(members))
	}
	expectedAgents := make(map[uuid.UUID]struct{}, len(agentIDs))
	for _, id := range agentIDs {
		expectedAgents[id] = struct{}{}
	}
	for _, member := range members {
		if member.DeletedAt.Valid || member.TeamID != teamID {
			return uuid.Nil, fmt.Errorf("inactive omnichannel team member %s failed the ownership check", member.ID)
		}
		if _, ok := expectedAgents[member.UserID]; !ok {
			return uuid.Nil, fmt.Errorf("inactive omnichannel team member %s references an unexpected user", member.ID)
		}
	}
	return teamID, nil
}

func assertLogicalAccountIsProviderFree(tx *gorm.DB, organizationID uuid.UUID) error {
	logicalID := deterministicUUID(KlinikReliveSalesDataset + "-logical-account")

	var legacyCount int64
	if err := tx.Unscoped().Model(&models.WhatsAppAccount{}).
		Where(
			"organization_id = ? AND (id = ? OR name = ?)",
			organizationID,
			logicalID,
			LogicalAccountName,
		).
		Count(&legacyCount).Error; err != nil {
		return fmt.Errorf("verify logical account against WhatsApp accounts: %w", err)
	}
	if legacyCount != 0 {
		return fmt.Errorf("logical account %q must not have a WhatsAppAccount or credentials row", LogicalAccountName)
	}

	var channelAccountCount int64
	if err := tx.Unscoped().Model(&models.ChannelAccount{}).
		Where("organization_id = ? AND (id = ? OR name = ?)", organizationID, logicalID, LogicalAccountName).
		Count(&channelAccountCount).Error; err != nil {
		return fmt.Errorf("verify logical account against channel accounts: %w", err)
	}
	if channelAccountCount != 0 {
		return fmt.Errorf("logical account %q must not have a channel provider account", LogicalAccountName)
	}

	var credentialCount int64
	if err := tx.Unscoped().Model(&models.ChannelCredential{}).
		Where("organization_id = ? AND channel_account_id = ?", organizationID, logicalID).
		Count(&credentialCount).Error; err != nil {
		return fmt.Errorf("verify logical account against channel credentials: %w", err)
	}
	if credentialCount != 0 {
		return fmt.Errorf("logical account %q must not have credentials", LogicalAccountName)
	}
	return nil
}

func assertFixtureOwnership(tx *gorm.DB, fixtures FixtureSet, refs FixtureReferences) error {
	cannedIDs := cannedResponseIDs(fixtures.CannedResponses)
	var existingCanned []models.CannedResponse
	if err := tx.Unscoped().Where("id IN ?", cannedIDs).Find(&existingCanned).Error; err != nil {
		return fmt.Errorf("inspect existing canned fixture rows: %w", err)
	}
	for _, row := range existingCanned {
		if row.OrganizationID != refs.OrganizationID ||
			!strings.HasPrefix(row.Name, "[MOCK] ") ||
			!strings.HasPrefix(row.Shortcut, "mock-") ||
			!strings.HasPrefix(row.Content, "[MOCK] ") ||
			!isAllowedCannedCategory(row.Category) ||
			row.IsActive ||
			len(row.Buttons) != 0 {
			return fmt.Errorf("deterministic canned response ID %s is owned by non-fixture or unsafe data", row.ID)
		}
	}
	if err := assertNoAlternateCannedRows(tx, fixtures, refs.OrganizationID); err != nil {
		return err
	}

	ivrIDs := ivrFlowIDs(fixtures.IVRFlows)
	var existingIVR []models.IVRFlow
	if err := tx.Unscoped().Where("id IN ?", ivrIDs).Find(&existingIVR).Error; err != nil {
		return fmt.Errorf("inspect existing IVR fixture rows: %w", err)
	}
	for _, row := range existingIVR {
		if row.OrganizationID != refs.OrganizationID ||
			row.WhatsAppAccount != LogicalAccountName ||
			!strings.HasPrefix(row.Name, "[MOCK] ") ||
			row.IsActive || row.IsCallStart || row.IsOutgoingEnd ||
			row.WelcomeAudioURL != "" {
			return fmt.Errorf("deterministic IVR ID %s is owned by non-fixture or live data", row.ID)
		}
		if err := validateSafeIVRGraph(row.Menu); err != nil {
			return fmt.Errorf("existing deterministic IVR ID %s is unsafe: %w", row.ID, err)
		}
	}
	if err := assertNoAlternateIVRRows(tx, fixtures, refs.OrganizationID); err != nil {
		return err
	}

	callIDs := callLogIDs(fixtures.CallLogs)
	var existingCalls []models.CallLog
	if err := tx.Unscoped().Where("id IN ?", callIDs).Find(&existingCalls).Error; err != nil {
		return fmt.Errorf("inspect existing call fixture rows: %w", err)
	}
	for _, row := range existingCalls {
		if row.OrganizationID != refs.OrganizationID ||
			row.WhatsAppAccount != LogicalAccountName ||
			!strings.HasPrefix(row.WhatsAppCallID, "MOCK-KLINIK-RELIVE-CALL-") ||
			!slicesContainsCallStatus(terminalCallStatuses, row.Status) ||
			row.EndedAt == nil ||
			row.RecordingS3Key != "" ||
			row.RecordingDuration != 0 ||
			row.RecordingError != "" {
			return fmt.Errorf("deterministic call log ID %s is owned by non-fixture or live data", row.ID)
		}
	}
	if err := assertNoAlternateCallRows(tx, fixtures, refs.OrganizationID); err != nil {
		return err
	}

	transferIDs := callTransferIDs(fixtures.CallTransfers)
	expectedCallIDs := make(map[uuid.UUID]struct{}, len(callIDs))
	for _, id := range callIDs {
		expectedCallIDs[id] = struct{}{}
	}
	var existingTransfers []models.CallTransfer
	if err := tx.Unscoped().Where("id IN ?", transferIDs).Find(&existingTransfers).Error; err != nil {
		return fmt.Errorf("inspect existing call-transfer fixture rows: %w", err)
	}
	for _, row := range existingTransfers {
		_, expectedCall := expectedCallIDs[row.CallLogID]
		if row.OrganizationID != refs.OrganizationID ||
			row.WhatsAppAccount != LogicalAccountName ||
			!expectedCall ||
			!slicesContainsTransferStatus(terminalTransferStatuses, row.Status) ||
			row.CompletedAt == nil {
			return fmt.Errorf("deterministic call transfer ID %s is owned by non-fixture or live data", row.ID)
		}
	}

	snapshotIDs := metaSnapshotIDs(fixtures.MetaSnapshots)
	var existingSnapshots []models.MetaAnalyticsSnapshot
	if err := tx.Unscoped().Where("id IN ?", snapshotIDs).Find(&existingSnapshots).Error; err != nil {
		return fmt.Errorf("inspect existing Meta snapshot fixture rows: %w", err)
	}
	for _, row := range existingSnapshots {
		if row.OrganizationID != refs.OrganizationID ||
			row.AccountName != LogicalAccountName ||
			row.PhoneID != "MOCK-KLINIK-RELIVE-PHONE" ||
			row.Dataset != KlinikReliveSalesDataset ||
			!row.IsMock {
			return fmt.Errorf("deterministic Meta snapshot ID %s is owned by non-fixture data", row.ID)
		}
	}
	if err := assertNoAlternateMetaRows(tx, fixtures, refs.OrganizationID); err != nil {
		return err
	}
	return nil
}

func assertNoAlternateCannedRows(tx *gorm.DB, fixtures FixtureSet, organizationID uuid.UUID) error {
	names := make([]string, 0, len(fixtures.CannedResponses))
	shortcuts := make([]string, 0, len(fixtures.CannedResponses))
	ids := cannedResponseIDs(fixtures.CannedResponses)
	for _, row := range fixtures.CannedResponses {
		names = append(names, row.Name)
		shortcuts = append(shortcuts, row.Shortcut)
	}
	var count int64
	if err := tx.Unscoped().Model(&models.CannedResponse{}).
		Where("organization_id = ? AND (name IN ? OR shortcut IN ?) AND id NOT IN ?", organizationID, names, shortcuts, ids).
		Count(&count).Error; err != nil {
		return fmt.Errorf("check alternate canned fixture rows: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("found %d alternate canned response rows with fixture names or shortcuts", count)
	}
	return nil
}

func assertNoAlternateIVRRows(tx *gorm.DB, fixtures FixtureSet, organizationID uuid.UUID) error {
	names := make([]string, 0, len(fixtures.IVRFlows))
	ids := ivrFlowIDs(fixtures.IVRFlows)
	for _, row := range fixtures.IVRFlows {
		names = append(names, row.Name)
	}
	var count int64
	if err := tx.Unscoped().Model(&models.IVRFlow{}).
		Where("organization_id = ? AND name IN ? AND id NOT IN ?", organizationID, names, ids).
		Count(&count).Error; err != nil {
		return fmt.Errorf("check alternate IVR fixture rows: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("found %d alternate IVR rows with fixture names", count)
	}
	return nil
}

func assertNoAlternateCallRows(tx *gorm.DB, fixtures FixtureSet, organizationID uuid.UUID) error {
	externalIDs := make([]string, 0, len(fixtures.CallLogs))
	ids := callLogIDs(fixtures.CallLogs)
	for _, row := range fixtures.CallLogs {
		externalIDs = append(externalIDs, row.WhatsAppCallID)
	}
	var count int64
	if err := tx.Unscoped().Model(&models.CallLog{}).
		Where("organization_id = ? AND whatsapp_call_id IN ? AND id NOT IN ?", organizationID, externalIDs, ids).
		Count(&count).Error; err != nil {
		return fmt.Errorf("check alternate call fixture rows: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("found %d alternate call logs with fixture external IDs", count)
	}
	return nil
}

func assertNoAlternateMetaRows(tx *gorm.DB, fixtures FixtureSet, organizationID uuid.UUID) error {
	ids := metaSnapshotIDs(fixtures.MetaSnapshots)
	accountID := fixtures.MetaSnapshots[0].AccountID
	periodStart := fixtures.MetaSnapshots[0].PeriodStart
	periodEnd := fixtures.MetaSnapshots[0].PeriodEnd
	var count int64
	if err := tx.Unscoped().Model(&models.MetaAnalyticsSnapshot{}).
		Where(
			"organization_id = ? AND account_id = ? AND analytics_type IN ? AND granularity = ? AND period_start = ? AND period_end = ? AND id NOT IN ?",
			organizationID,
			accountID,
			metaAnalyticsTypes,
			"DAY",
			periodStart,
			periodEnd,
			ids,
		).
		Count(&count).Error; err != nil {
		return fmt.Errorf("check alternate Meta snapshot fixture rows: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("found %d alternate Meta snapshots for the fixture period", count)
	}
	return nil
}

func upsertFixtureRows(tx *gorm.DB, fixtures FixtureSet) error {
	if err := upsertCannedResponses(tx, fixtures.CannedResponses); err != nil {
		return err
	}
	if err := upsertIVRFlows(tx, fixtures.IVRFlows); err != nil {
		return err
	}
	if err := upsertCallLogs(tx, fixtures.CallLogs); err != nil {
		return err
	}
	if err := upsertCallTransfers(tx, fixtures.CallTransfers); err != nil {
		return err
	}
	if err := upsertMetaSnapshots(tx, fixtures.MetaSnapshots); err != nil {
		return err
	}
	return nil
}

func upsertCannedResponses(tx *gorm.DB, rows []models.CannedResponse) error {
	conflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"organization_id", "name", "shortcut", "content", "category", "is_active",
			"usage_count", "buttons", "created_by_id", "updated_at", "deleted_at",
		}),
	}
	if err := tx.Clauses(conflict).Omit(clause.Associations).Create(&rows).Error; err != nil {
		return fmt.Errorf("upsert canned responses: %w", err)
	}
	return nil
}

func upsertIVRFlows(tx *gorm.DB, rows []models.IVRFlow) error {
	conflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"organization_id", "whatsapp_account", "name", "description", "is_active",
			"is_call_start", "is_outgoing_end", "menu", "welcome_audio_url",
			"created_by_id", "updated_by_id", "updated_at", "deleted_at",
		}),
	}
	if err := tx.Clauses(conflict).Omit(clause.Associations).Create(&rows).Error; err != nil {
		return fmt.Errorf("upsert disabled IVR flows: %w", err)
	}
	return nil
}

func upsertCallLogs(tx *gorm.DB, rows []models.CallLog) error {
	conflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"organization_id", "whatsapp_account", "contact_id", "whatsapp_call_id",
			"caller_phone", "direction", "status", "duration", "ivr_flow_id", "ivr_path",
			"agent_id", "started_at", "answered_at", "ended_at", "disconnected_by",
			"error_message", "recording_s3_key", "recording_duration", "recording_error",
			"created_at", "updated_at", "deleted_at",
		}),
	}
	if err := tx.Clauses(conflict).Omit(clause.Associations).CreateInBatches(&rows, 36).Error; err != nil {
		return fmt.Errorf("upsert terminal call logs: %w", err)
	}
	return nil
}

func upsertCallTransfers(tx *gorm.DB, rows []models.CallTransfer) error {
	conflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"organization_id", "call_log_id", "whats_app_call_id", "caller_phone",
			"contact_id", "whats_app_account", "status", "team_id", "agent_id",
			"initiating_agent_id", "transferred_at", "connected_at", "completed_at",
			"hold_duration", "talk_duration", "ivr_path", "tried_agent_ids",
			"created_at", "updated_at", "deleted_at",
		}),
	}
	if err := tx.Clauses(conflict).Omit(clause.Associations).Create(&rows).Error; err != nil {
		return fmt.Errorf("upsert terminal call transfers: %w", err)
	}
	return nil
}

func upsertMetaSnapshots(tx *gorm.DB, rows []models.MetaAnalyticsSnapshot) error {
	conflict := clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"organization_id", "account_id", "account_name", "phone_id", "dataset", "analytics_type",
			"granularity", "period_start", "period_end", "payload", "template_names",
			"is_mock", "updated_at", "deleted_at",
		}),
	}
	if err := tx.Clauses(conflict).Omit(clause.Associations).Create(&rows).Error; err != nil {
		return fmt.Errorf("upsert Meta analytics snapshots: %w", err)
	}
	return nil
}

func assertPersistedFixtures(tx *gorm.DB, expected FixtureSet, refs FixtureReferences) error {
	var actual FixtureSet
	if err := tx.Unscoped().Where("id IN ?", cannedResponseIDs(expected.CannedResponses)).
		Find(&actual.CannedResponses).Error; err != nil {
		return fmt.Errorf("reload canned responses: %w", err)
	}
	if err := tx.Unscoped().Where("id IN ?", ivrFlowIDs(expected.IVRFlows)).
		Find(&actual.IVRFlows).Error; err != nil {
		return fmt.Errorf("reload IVR flows: %w", err)
	}
	if err := tx.Unscoped().Where("id IN ?", callLogIDs(expected.CallLogs)).
		Find(&actual.CallLogs).Error; err != nil {
		return fmt.Errorf("reload call logs: %w", err)
	}
	if err := tx.Unscoped().Where("id IN ?", callTransferIDs(expected.CallTransfers)).
		Find(&actual.CallTransfers).Error; err != nil {
		return fmt.Errorf("reload call transfers: %w", err)
	}
	if err := tx.Unscoped().Where("id IN ?", metaSnapshotIDs(expected.MetaSnapshots)).
		Find(&actual.MetaSnapshots).Error; err != nil {
		return fmt.Errorf("reload Meta analytics snapshots: %w", err)
	}

	if hasDeletedBaseModels(actual.CannedResponses, actual.IVRFlows, actual.CallLogs, actual.CallTransfers, actual.MetaSnapshots) {
		return fmt.Errorf("one or more persisted fixture rows remained soft-deleted")
	}
	if err := ValidateKlinikReliveSalesFixtures(actual, refs); err != nil {
		return fmt.Errorf("persisted sales fixture safety assertion failed: %w", err)
	}
	return nil
}

func hasDeletedBaseModels(
	canned []models.CannedResponse,
	ivr []models.IVRFlow,
	calls []models.CallLog,
	transfers []models.CallTransfer,
	meta []models.MetaAnalyticsSnapshot,
) bool {
	for _, row := range canned {
		if row.DeletedAt.Valid {
			return true
		}
	}
	for _, row := range ivr {
		if row.DeletedAt.Valid {
			return true
		}
	}
	for _, row := range calls {
		if row.DeletedAt.Valid {
			return true
		}
	}
	for _, row := range transfers {
		if row.DeletedAt.Valid {
			return true
		}
	}
	for _, row := range meta {
		if row.DeletedAt.Valid {
			return true
		}
	}
	return false
}

func captureForbiddenState(tx *gorm.DB, organizationID uuid.UUID) (map[string]invariantValue, error) {
	state := make(map[string]invariantValue)
	orgTables := []string{
		"organizations",
		"whatsapp_accounts",
		"channel_accounts",
		"channel_credentials",
		"users",
		"user_organizations",
		"teams",
		"user_availability_logs",
		"call_permissions",
		"inbound_events",
		"message_events",
		"outbox_jobs",
		"scheduled_jobs",
		"outbox_events",
		"automation_event_receipts",
		"customer_activity_events",
	}
	for _, table := range orgTables {
		value, err := captureOrganizationTable(tx, table, organizationID)
		if err != nil {
			return nil, err
		}
		state[table] = value
	}

	teamMembers, err := captureTeamMembers(tx, organizationID)
	if err != nil {
		return nil, err
	}
	state["team_members"] = teamMembers
	return state, nil
}

func captureOrganizationTable(tx *gorm.DB, table string, organizationID uuid.UUID) (invariantValue, error) {
	allowed := map[string]bool{
		"organizations": true, "whatsapp_accounts": true, "channel_accounts": true,
		"channel_credentials": true, "users": true, "user_organizations": true,
		"teams": true, "user_availability_logs": true, "call_permissions": true,
		"inbound_events": true, "message_events": true, "outbox_jobs": true,
		"scheduled_jobs": true, "outbox_events": true, "automation_event_receipts": true,
		"customer_activity_events": true,
	}
	if !allowed[table] {
		return invariantValue{}, fmt.Errorf("unsupported invariant table %q", table)
	}
	var result struct {
		Count  int64  `gorm:"column:row_count"`
		Digest string `gorm:"column:digest"`
	}
	query := fmt.Sprintf(`
		SELECT
			COUNT(*) AS row_count,
			COALESCE(
				md5(string_agg(md5(row_to_json(t)::text), '' ORDER BY t.id::text)),
				md5('')
			) AS digest
		FROM %s AS t
		WHERE t.%s = ?`,
		table,
		map[bool]string{true: "id", false: "organization_id"}[table == "organizations"],
	)
	if err := tx.Raw(query, organizationID).Scan(&result).Error; err != nil {
		return invariantValue{}, fmt.Errorf("snapshot invariant table %s: %w", table, err)
	}
	return invariantValue{Count: result.Count, Digest: result.Digest}, nil
}

func captureTeamMembers(tx *gorm.DB, organizationID uuid.UUID) (invariantValue, error) {
	var result struct {
		Count  int64  `gorm:"column:row_count"`
		Digest string `gorm:"column:digest"`
	}
	const query = `
		SELECT
			COUNT(*) AS row_count,
			COALESCE(
				md5(string_agg(md5(row_to_json(member)::text), '' ORDER BY member.id::text)),
				md5('')
			) AS digest
		FROM team_members AS member
		JOIN teams AS team ON team.id = member.team_id
		WHERE team.organization_id = ?`
	if err := tx.Raw(query, organizationID).Scan(&result).Error; err != nil {
		return invariantValue{}, fmt.Errorf("snapshot invariant table team_members: %w", err)
	}
	return invariantValue{Count: result.Count, Digest: result.Digest}, nil
}

func compareForbiddenState(before, after map[string]invariantValue) error {
	if len(before) != len(after) {
		return fmt.Errorf("forbidden-state invariant set changed")
	}
	for name, beforeValue := range before {
		afterValue, ok := after[name]
		if !ok {
			return fmt.Errorf("forbidden-state invariant %s disappeared", name)
		}
		if beforeValue != afterValue {
			return fmt.Errorf("forbidden-state invariant %s changed; transaction will not commit", name)
		}
	}
	return nil
}

func cannedResponseIDs(rows []models.CannedResponse) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func ivrFlowIDs(rows []models.IVRFlow) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func callLogIDs(rows []models.CallLog) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func callTransferIDs(rows []models.CallTransfer) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func metaSnapshotIDs(rows []models.MetaAnalyticsSnapshot) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func slicesContainsCallStatus(values []models.CallStatus, value models.CallStatus) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func slicesContainsTransferStatus(values []models.CallTransferStatus, value models.CallTransferStatus) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func jsonString(value models.JSONB, key string) string {
	text, _ := value[key].(string)
	return text
}

func jsonBool(value models.JSONB, key string) bool {
	boolean, _ := value[key].(bool)
	return boolean
}
