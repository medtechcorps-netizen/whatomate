package threadsreview

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	StatusKey           = "app_review_status"
	ApprovedStatus      = "approved"
	ApprovalEvidenceKey = "_app_review_approval"

	ModeBlocked            = "blocked"
	ModeApproved           = "approved"
	ModeDevelopmentTesting = "development_testing"
)

const maximumApprovalEvidenceBytes = 2048

// ProductionApproved requires server-owned evidence bound to the exact Meta
// app. A tenant-written status string alone never authorizes provider access.
func ProductionApproved(integrationConfig models.JSONB) bool {
	if rawString(integrationConfig, StatusKey) != ApprovedStatus {
		return false
	}
	metadata := jsonMap(integrationConfig[ApprovalEvidenceKey])
	if metadata == nil || rawString(metadata, "app_id") == "" ||
		rawString(metadata, "app_id") != rawString(integrationConfig, "app_id") {
		return false
	}
	hash := rawString(metadata, "evidence_sha256")
	decodedHash, err := hex.DecodeString(hash)
	if err != nil || len(decodedHash) != sha256.Size || strings.ToLower(hash) != hash {
		return false
	}
	approvedAt, err := time.Parse(time.RFC3339Nano, rawString(metadata, "approved_at"))
	if err != nil || approvedAt.IsZero() {
		return false
	}
	approvedByText := rawString(metadata, "approved_by")
	approvedBy, err := uuid.Parse(approvedByText)
	return err == nil && approvedBy != uuid.Nil && approvedBy.String() == approvedByText
}

// RecordApproval returns a copy carrying evidence that can only be minted by
// the server-owned superadmin control plane.
func RecordApproval(
	integrationConfig models.JSONB,
	appID, evidence string,
	approvedBy uuid.UUID,
	approvedAt time.Time,
) (models.JSONB, bool) {
	evidence = strings.TrimSpace(evidence)
	appID = strings.TrimSpace(appID)
	if evidence == "" || len(evidence) > maximumApprovalEvidenceBytes || appID == "" ||
		rawString(integrationConfig, "app_id") != appID || approvedBy == uuid.Nil || approvedAt.IsZero() {
		return nil, false
	}
	result := clone(integrationConfig)
	digest := sha256.Sum256([]byte(evidence))
	result[StatusKey] = ApprovedStatus
	result[ApprovalEvidenceKey] = models.JSONB{
		"app_id":          appID,
		"evidence_sha256": hex.EncodeToString(digest[:]),
		"approved_at":     approvedAt.UTC().Format(time.RFC3339Nano),
		"approved_by":     approvedBy.String(),
	}
	return result, true
}

// RemoveApproval clears both the visible status and all server-owned evidence.
func RemoveApproval(integrationConfig models.JSONB, status string) models.JSONB {
	result := clone(integrationConfig)
	delete(result, ApprovalEvidenceKey)
	result[StatusKey] = strings.TrimSpace(status)
	return result
}

// AccessMode is the single fail-closed runtime decision shared by handlers and
// workers. The development exception is exact, deployment-owned, and cannot be
// enabled in production even if a Config is constructed without Load.
func AccessMode(
	cfg *config.Config,
	organizationID uuid.UUID,
	integrationConfig models.JSONB,
	appID, profileID string,
) string {
	if ProductionApproved(integrationConfig) {
		return ModeApproved
	}
	if cfg == nil || organizationID == uuid.Nil ||
		strings.EqualFold(strings.TrimSpace(cfg.App.Environment), "production") ||
		!cfg.ThreadsAppReview.DevelopmentTestingEnabled {
		return ModeBlocked
	}
	status := rawString(integrationConfig, StatusKey)
	if status != "not_submitted" && status != "pending" {
		return ModeBlocked
	}
	development := cfg.ThreadsAppReview
	if development.DevelopmentOrganizationID != organizationID.String() ||
		development.DevelopmentAppID == "" || development.DevelopmentAppID != appID ||
		rawString(integrationConfig, "app_id") != development.DevelopmentAppID ||
		development.DevelopmentProfileID == "" {
		return ModeBlocked
	}
	if profileID != "" && profileID != development.DevelopmentProfileID {
		return ModeBlocked
	}
	return ModeDevelopmentTesting
}

func ExpectedDevelopmentProfileID(cfg *config.Config, mode string) string {
	if cfg == nil || mode != ModeDevelopmentTesting {
		return ""
	}
	return cfg.ThreadsAppReview.DevelopmentProfileID
}

func clone(input models.JSONB) models.JSONB {
	result := make(models.JSONB, len(input)+1)
	for key, value := range input {
		result[key] = value
	}
	return result
}

func jsonMap(value any) models.JSONB {
	switch typed := value.(type) {
	case models.JSONB:
		return typed
	case map[string]any:
		return models.JSONB(typed)
	default:
		return nil
	}
}

func rawString(values models.JSONB, key string) string {
	value, _ := values[key].(string)
	return value
}
