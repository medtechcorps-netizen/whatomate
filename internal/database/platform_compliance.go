package database

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	// PlatformCompliancePurposeMarkerKey is the durable, feature-independent
	// organization classification used by both the operator control plane and
	// the database write barrier.
	PlatformCompliancePurposeMarkerKey = "platform_compliance_tenant"
	// PlatformComplianceInstagramMarkerKey and
	// PlatformComplianceThreadsMarkerKey are independent operator-controlled
	// feature assertions. Keeping their canonical names in the database package
	// lets the write barrier enforce them without an import cycle.
	PlatformComplianceInstagramMarkerKey = "meta_instagram_data_deletion_compliance_tenant"
	PlatformComplianceThreadsMarkerKey   = "threads_managed_compliance_tenant"

	platformComplianceWriteGuardFunction      = "rereply_platform_compliance_write_guard"
	platformComplianceScanGuardFunction       = "rereply_assert_platform_compliance_scan_authority"
	platformComplianceClassifyFunction        = "rereply_platform_compliance_classification_guard"
	platformComplianceCreationAuditFunction   = "rereply_platform_compliance_creation_audit"
	platformComplianceIdentityGuardFunction   = "rereply_platform_compliance_identity_registry_guard"
	platformComplianceCreatorFunction         = "rereply_create_platform_compliance_organization"
	platformComplianceContractFunction        = "rereply_platform_compliance_guard_contract"
	platformComplianceWriteTrigger            = "rereply_platform_compliance_write_guard"
	platformComplianceClassifyTrigger         = "rereply_platform_compliance_classification_guard"
	platformComplianceCreationAuditTrigger    = "rereply_platform_compliance_creation_audit"
	platformComplianceTruncateTrigger         = "rereply_platform_compliance_truncate_guard"
	platformComplianceIdentityWriteTrigger    = "rereply_platform_compliance_identity_registry_write_guard"
	platformComplianceIdentityTruncateTrigger = "rereply_platform_compliance_identity_registry_truncate_guard"
	platformComplianceIdentityRegistryTable   = "organization_identity_registry"
	platformComplianceCreationEvidenceIndex   = "rereply_platform_compliance_creation_evidence_unique"
	platformComplianceAuditRunIndex           = "rereply_platform_compliance_audit_run_unique"
	platformComplianceGuardVersion            = 7
	platformComplianceOperatorRunIDSetting    = "app.platform_compliance_operator_run_id"
	platformComplianceOperationIntentSetting  = "app.platform_compliance_operation_intent"
	platformComplianceCreationIntentSetting   = "app.platform_compliance_creation_intent"
	platformComplianceIdentityIntentSetting   = "app.platform_compliance_identity_claim_intent"

	platformComplianceRowToken = "{{row}}"
)

var (
	ErrPlatformComplianceRegistryInvalid  = errors.New("platform compliance table registry is invalid")
	ErrPlatformComplianceScanAuthority    = errors.New("platform compliance historical scan authority is incomplete")
	ErrPlatformComplianceFootprint        = errors.New("organization has a prohibited platform compliance footprint")
	ErrPlatformComplianceTenant           = errors.New("authenticated tenant access is prohibited for platform compliance organizations")
	ErrPlatformComplianceMarkerInvalid    = errors.New("organization has invalid reserved platform compliance markers")
	ErrPlatformComplianceCreationMismatch = errors.New("platform compliance organization does not match the requested atomic creation")
	ErrPlatformComplianceRollbackBlocked  = errors.New("tenant RLS rollback is blocked while a platform compliance organization exists")
)

// PlatformComplianceTableRule is derived from the same authoritative
// organization-scoped registries used by tenant_registry_test. An empty
// AllowedRowPredicate means every row is prohibited for a purpose organization.
// Predicates use platformComplianceRowToken for a jsonb representation of the
// candidate row so the same reviewed expression can be used by historical
// scans and live triggers.
type PlatformComplianceTableRule struct {
	Table               string
	ForceTenantRLS      bool
	AllowedRowPredicate string
	ReviewReason        string
}

type platformComplianceWritableRule struct {
	predicate string
	reason    string
}

// platformComplianceWritableExemptions is deliberately small and reviewed.
// These are row-shape exemptions, never whole-table exemptions. A row that
// does not continue to match its predicate is rejected, and a pre-existing row
// that does not match prevents atomic purpose creation.
var platformComplianceWritableExemptions = map[string]platformComplianceWritableRule{
	"audit_logs": {
		predicate: platformComplianceAuditPredicateV7,
		reason:    "only the sanitized, migration-authority bootstrap audit evidence row is writable; ordinary audit history remains prohibited",
	},
	"meta_instagram_data_deletion_events": {
		predicate: platformComplianceInstagramDeletionPredicateWithParent(),
		reason:    "only HMAC-identity no-target or ambiguous managed-Instagram deletion journals are compliance control-plane records",
	},
	"privacy_requests": {
		predicate: platformCompliancePrivacyRequestPredicate,
		reason:    "only the HMAC-bound managed-Instagram no-target deletion request shape is a compliance control-plane record",
	},
	"privacy_request_events": {
		predicate: platformCompliancePrivacyEventPredicate(),
		reason:    "only system-authored events linked to an admitted managed-Instagram compliance request are writable",
	},
}

// platformComplianceExplicitProhibitions documents sensitive tables whose
// disposition is easy to misread during review. They remain fully prohibited.
var platformComplianceExplicitProhibitions = map[string]string{
	"legal_holds":               "legal holds are clinic/customer obligations; an active hold is also a feature-removal blocker, never a writable exemption",
	"privacy_jobs":              "asynchronous export/erasure jobs can contain customer work and remain prohibited until a separately reviewed purpose-only shape exists",
	"retention_policies":        "retention policy rows are tenant policy, not platform bootstrap state, and remain prohibited",
	"threads_platform_bindings": "a compliance tenant must never own a managed Threads profile binding",
}

// platformComplianceAuditPredicateV7 admits only append-only evidence emitted
// by the atomic creator or a reviewed feature-marker transition. The operation
// token and every marker delta are bidirectionally bound so free-form JSON
// cannot attest an operation that did not occur.
const platformComplianceAuditPredicateV7 = `
(
	` + platformComplianceRowToken + `->>'resource_type' = 'platform_compliance'
	AND ` + platformComplianceRowToken + `->>'resource_id' = ` + platformComplianceRowToken + `->>'organization_id'
	AND ` + platformComplianceRowToken + `->>'user_id' = '00000000-0000-0000-0000-000000000000'
	AND ` + platformComplianceRowToken + `->>'action' = 'updated'
	AND ` + platformComplianceRowToken + `->>'user_name' ~ '^platform-compliance-bootstrap/[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$'
	AND pg_catalog.jsonb_typeof(` + platformComplianceRowToken + `->'changes') = 'array'
	AND pg_catalog.jsonb_array_length(` + platformComplianceRowToken + `->'changes') BETWEEN 5 AND 7
	AND NOT EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE pg_catalog.jsonb_typeof(item) <> 'object'
		   OR (
			SELECT pg_catalog.array_agg(key ORDER BY key)
			FROM pg_catalog.jsonb_object_keys(item) AS keys(key)
		   ) <> ARRAY['field', 'new_value', 'old_value']::text[]
		   OR item->>'field' NOT IN (
			'platform_compliance_tenant',
			'meta_instagram_data_deletion_compliance_tenant',
			'threads_managed_compliance_tenant',
			'operator_run_id',
			'database_current_user',
			'database_session_user',
			'control_plane_operation'
		   )
	)
	AND (
		SELECT pg_catalog.count(DISTINCT item->>'field')
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
	) = pg_catalog.jsonb_array_length(` + platformComplianceRowToken + `->'changes')
	AND (
		SELECT pg_catalog.count(*)
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE item->>'field' IN (
			'platform_compliance_tenant',
			'meta_instagram_data_deletion_compliance_tenant',
			'threads_managed_compliance_tenant'
		)
	) = pg_catalog.jsonb_array_length(` + platformComplianceRowToken + `->'changes') - 4
	AND NOT EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE item->>'field' = 'platform_compliance_tenant'
		  AND NOT (item->'old_value' = 'null'::jsonb AND item->'new_value' = 'true'::jsonb)
	)
	AND NOT EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE item->>'field' IN (
			'meta_instagram_data_deletion_compliance_tenant',
			'threads_managed_compliance_tenant'
		)
		  AND NOT (
			(item->'old_value' = 'null'::jsonb AND item->'new_value' = 'true'::jsonb)
			OR (item->'old_value' = 'true'::jsonb AND item->'new_value' = 'null'::jsonb)
		  )
	)
	AND EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE item->>'field' = 'operator_run_id'
		  AND item->'old_value' = 'null'::jsonb
		  AND pg_catalog.jsonb_typeof(item->'new_value') = 'string'
		  AND item->>'new_value' ~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$'
		  AND ` + platformComplianceRowToken + `->>'user_name' = 'platform-compliance-bootstrap/' || (item->>'new_value')
	)
	AND EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE item->>'field' = 'database_current_user'
		  AND item->'old_value' = 'null'::jsonb
		  AND pg_catalog.jsonb_typeof(item->'new_value') = 'string'
		  AND item->>'new_value' ~ '^[A-Za-z_][A-Za-z0-9_]{0,62}$'
	)
	AND EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS item
		WHERE item->>'field' = 'database_session_user'
		  AND item->'old_value' = 'null'::jsonb
		  AND pg_catalog.jsonb_typeof(item->'new_value') = 'string'
		  AND item->>'new_value' ~ '^[A-Za-z_][A-Za-z0-9_]{0,62}$'
	)
	AND EXISTS (
		SELECT 1
		FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS operation_item
		WHERE operation_item->>'field' = 'control_plane_operation'
		  AND operation_item->'old_value' = 'null'::jsonb
		  AND pg_catalog.jsonb_typeof(operation_item->'new_value') = 'string'
		  AND (
			(
				EXISTS (
					SELECT 1 FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS marker
					WHERE marker->>'field' = 'platform_compliance_tenant'
				)
				AND operation_item->>'new_value' ~ '^create:purpose(,enable:instagram)?(,enable:threads)?$'
			)
			OR (
				NOT EXISTS (
					SELECT 1 FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS marker
					WHERE marker->>'field' = 'platform_compliance_tenant'
				)
				AND operation_item->>'new_value' ~ '^(enable|remove):(instagram|threads)(,(enable|remove):(instagram|threads))?$'
			)
		  )
		  AND (operation_item->>'new_value' ~ '(^|,)enable:instagram(,|$)') = EXISTS (
			SELECT 1 FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS marker
			WHERE marker->>'field' = 'meta_instagram_data_deletion_compliance_tenant'
			  AND marker->'new_value' = 'true'::jsonb
		  )
		  AND (operation_item->>'new_value' ~ '(^|,)remove:instagram(,|$)') = EXISTS (
			SELECT 1 FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS marker
			WHERE marker->>'field' = 'meta_instagram_data_deletion_compliance_tenant'
			  AND marker->'new_value' = 'null'::jsonb
		  )
		  AND (operation_item->>'new_value' ~ '(^|,)enable:threads(,|$)') = EXISTS (
			SELECT 1 FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS marker
			WHERE marker->>'field' = 'threads_managed_compliance_tenant'
			  AND marker->'new_value' = 'true'::jsonb
		  )
		  AND (operation_item->>'new_value' ~ '(^|,)remove:threads(,|$)') = EXISTS (
			SELECT 1 FROM pg_catalog.jsonb_array_elements(` + platformComplianceRowToken + `->'changes') AS marker
			WHERE marker->>'field' = 'threads_managed_compliance_tenant'
			  AND marker->'new_value' = 'null'::jsonb
		  )
	)
)
`

const platformComplianceInstagramDeletionPredicate = `
(
	` + platformComplianceRowToken + `->>'digest' ~ '^[0-9a-f]{64}$'
	AND ` + platformComplianceRowToken + `->>'platform_app_id' ~ '^[0-9a-f]{64}$'
	AND ` + platformComplianceRowToken + `->>'authorizing_user_id' ~ '^[0-9a-f]{64}$'
	AND (` + platformComplianceRowToken + `->>'identity_hashed')::boolean IS TRUE
	AND ` + platformComplianceRowToken + `->>'target_resolution' IN ('no_current_target', 'ambiguous_targets')
	AND (
		(
			` + platformComplianceRowToken + `->>'state' = 'verified'
			AND NULLIF(` + platformComplianceRowToken + `->>'privacy_request_id', '') IS NULL
			AND COALESCE(` + platformComplianceRowToken + `->>'request_number', '') = ''
			AND NULLIF(` + platformComplianceRowToken + `->>'completed_at', '') IS NULL
		)
		OR (
			` + platformComplianceRowToken + `->>'state' = 'completed'
			AND (` + platformComplianceRowToken + `->>'privacy_request_id') ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
			AND ` + platformComplianceRowToken + `->>'request_number' ~ '^IGDEL[0-9A-F]{32}$'
			AND NULLIF(` + platformComplianceRowToken + `->>'completed_at', '') IS NOT NULL
		)
	)
)`

const platformCompliancePrivacyRequestPredicate = `
(
	NULLIF(` + platformComplianceRowToken + `->>'deleted_at', '') IS NULL
	AND
	` + platformComplianceRowToken + `->>'request_number' ~ '^IGDEL[0-9A-F]{32}$'
	AND ` + platformComplianceRowToken + `->>'type' = 'deletion'
	AND ` + platformComplianceRowToken + `->>'status' = 'in_progress'
	AND ` + platformComplianceRowToken + `->>'subject_type' = 'instagram_profile'
	AND ` + platformComplianceRowToken + `->>'subject_key' ~ '^[0-9a-f]{64}$'
	AND NULLIF(` + platformComplianceRowToken + `->>'contact_id', '') IS NULL
	AND ` + platformComplianceRowToken + `->>'received_channel' = 'instagram'
	AND COALESCE(` + platformComplianceRowToken + `->'requester_profile', '{}'::jsonb) = '{}'::jsonb
	AND ` + platformComplianceRowToken + `->'request_details' IN (
		'{"source":"meta_instagram_managed_login","target_resolution":"no_current_target","requires_manual_resolution":false}'::jsonb,
		'{"source":"meta_instagram_managed_login","target_resolution":"ambiguous_targets","requires_manual_resolution":true}'::jsonb
	)
	AND ` + platformComplianceRowToken + `->>'verification_method' = 'meta_instagram_signed_request'
	AND ` + platformComplianceRowToken + `->>'verification_token_hash' ~ '^[0-9a-f]{64}$'
	AND NULLIF(` + platformComplianceRowToken + `->>'assigned_to_id', '') IS NULL
	AND NULLIF(` + platformComplianceRowToken + `->>'approved_by_id', '') IS NULL
	AND NULLIF(` + platformComplianceRowToken + `->>'completed_at', '') IS NULL
	AND NULLIF(` + platformComplianceRowToken + `->>'decision_reason', '') IS NULL
	AND NULLIF(` + platformComplianceRowToken + `->>'result_object_key', '') IS NULL
	AND NULLIF(` + platformComplianceRowToken + `->>'result_expires_at', '') IS NULL
	AND NULLIF(` + platformComplianceRowToken + `->>'verified_at', '') IS NOT NULL
)`

func platformCompliancePrivacyEventPredicate() string {
	parentPredicate := strings.ReplaceAll(
		platformCompliancePrivacyRequestPredicate,
		platformComplianceRowToken,
		"pg_catalog.to_jsonb(parent_request)",
	)
	return `
(
	NULLIF(` + platformComplianceRowToken + `->>'deleted_at', '') IS NULL
	AND
	(
		(
			` + platformComplianceRowToken + `->>'event_type' = 'request_created'
			AND COALESCE(` + platformComplianceRowToken + `->>'from_status', '') = ''
			AND ` + platformComplianceRowToken + `->>'to_status' = 'in_progress'
			AND ` + platformComplianceRowToken + `->>'message' = 'Managed Instagram data deletion request verified and initiated'
			AND ` + platformComplianceRowToken + `->'details' = '{"verification":"meta_instagram_signed_request"}'::jsonb
		)
		OR (
			` + platformComplianceRowToken + `->>'event_type' = 'target_resolution_required'
			AND COALESCE(` + platformComplianceRowToken + `->>'from_status', '') = ''
			AND ` + platformComplianceRowToken + `->>'to_status' = 'in_progress'
			AND ` + platformComplianceRowToken + `->>'message' = 'Managed Instagram deletion target resolution requires compliance review'
			AND ` + platformComplianceRowToken + `->'details' = '{"target_resolution":"ambiguous_targets"}'::jsonb
		)
	)
	AND NULLIF(` + platformComplianceRowToken + `->>'actor_user_id', '') IS NULL
	AND EXISTS (
		SELECT 1
		FROM public.privacy_requests AS parent_request
		WHERE parent_request.id = (` + platformComplianceRowToken + `->>'privacy_request_id')::uuid
		  AND parent_request.organization_id = (` + platformComplianceRowToken + `->>'organization_id')::uuid
		  AND ` + parentPredicate + `
	)
)`
}

func platformComplianceInstagramDeletionPredicateWithParent() string {
	parentPredicate := strings.ReplaceAll(
		platformCompliancePrivacyRequestPredicate,
		platformComplianceRowToken,
		"pg_catalog.to_jsonb(parent_request)",
	)
	return `
(
	(` + platformComplianceInstagramDeletionPredicate + `)
	AND (
		` + platformComplianceRowToken + `->>'state' <> 'completed'
		OR EXISTS (
			SELECT 1
			FROM public.privacy_requests AS parent_request
			WHERE parent_request.id = (` + platformComplianceRowToken + `->>'privacy_request_id')::uuid
			  AND parent_request.organization_id = (` + platformComplianceRowToken + `->>'organization_id')::uuid
			  AND parent_request.request_number = ` + platformComplianceRowToken + `->>'request_number'
			  AND parent_request.subject_key = ` + platformComplianceRowToken + `->>'authorizing_user_id'
			  AND parent_request.request_details->>'target_resolution' = ` + platformComplianceRowToken + `->>'target_resolution'
			  AND (` + parentPredicate + `)
		)
	)
)`
}

// PlatformComplianceTableRules returns the complete, sorted compliance write
// contract derived from the tenant registries. It fails rather than silently
// ignoring duplicate, missing, or mistargeted review entries.
func PlatformComplianceTableRules() ([]PlatformComplianceTableRule, error) {
	rules := make(map[string]PlatformComplianceTableRule, len(DirectTenantTables)+len(DirectTenantTableExemptions))
	for _, table := range DirectTenantTables {
		if err := addPlatformComplianceTableRule(rules, table, true); err != nil {
			return nil, err
		}
	}
	for table, reason := range DirectTenantTableExemptions {
		if strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%w: tenant exemption %q has no reason", ErrPlatformComplianceRegistryInvalid, table)
		}
		if err := addPlatformComplianceTableRule(rules, table, false); err != nil {
			return nil, err
		}
	}
	for table, review := range platformComplianceWritableExemptions {
		rule, ok := rules[table]
		if !ok {
			return nil, fmt.Errorf("%w: writable exemption %q is not in the tenant registry", ErrPlatformComplianceRegistryInvalid, table)
		}
		if strings.TrimSpace(review.reason) == "" || strings.TrimSpace(review.predicate) == "" {
			return nil, fmt.Errorf("%w: writable exemption %q is incomplete", ErrPlatformComplianceRegistryInvalid, table)
		}
		rule.AllowedRowPredicate = review.predicate
		rule.ReviewReason = review.reason
		rules[table] = rule
	}
	for table, reason := range platformComplianceExplicitProhibitions {
		rule, ok := rules[table]
		if !ok {
			return nil, fmt.Errorf("%w: explicit prohibition %q is not in the tenant registry", ErrPlatformComplianceRegistryInvalid, table)
		}
		if strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%w: explicit prohibition %q has no reason", ErrPlatformComplianceRegistryInvalid, table)
		}
		if rule.AllowedRowPredicate != "" {
			return nil, fmt.Errorf("%w: table %q is both prohibited and writable", ErrPlatformComplianceRegistryInvalid, table)
		}
		rule.ReviewReason = reason
		rules[table] = rule
	}

	result := make([]PlatformComplianceTableRule, 0, len(rules))
	for _, rule := range rules {
		result = append(result, rule)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Table < result[j].Table })
	return result, nil
}

func addPlatformComplianceTableRule(
	rules map[string]PlatformComplianceTableRule,
	table string,
	forceTenantRLS bool,
) error {
	if err := validateIdentifier(table); err != nil {
		return fmt.Errorf("%w: invalid table %q", ErrPlatformComplianceRegistryInvalid, table)
	}
	if _, exists := rules[table]; exists {
		return fmt.Errorf("%w: duplicate table %q", ErrPlatformComplianceRegistryInvalid, table)
	}
	rules[table] = PlatformComplianceTableRule{
		Table:          table,
		ForceTenantRLS: forceTenantRLS,
		ReviewReason:   "ordinary tenant data is prohibited for a platform compliance organization",
	}
	return nil
}

// IsPlatformComplianceSettings requires an exact JSON boolean true. Strings,
// numbers, null, false, and a missing marker are all ordinary tenant state.
func IsPlatformComplianceSettings(settings models.JSONB) bool {
	value, exists := settings[PlatformCompliancePurposeMarkerKey]
	enabled, boolean := value.(bool)
	return exists && boolean && enabled
}

// ExcludePlatformComplianceOrganizations is the fail-closed organization-list
// scope for ordinary startup seeders and background workers. It excludes every
// row carrying any reserved marker, including malformed/preseeded states that
// startup verification will separately reject.
func ExcludePlatformComplianceOrganizations(db *gorm.DB) *gorm.DB {
	if db == nil {
		return db
	}
	return db.Where(`
		NOT pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
		AND NOT pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
		AND NOT pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
	`, PlatformCompliancePurposeMarkerKey,
		PlatformComplianceInstagramMarkerKey,
		PlatformComplianceThreadsMarkerKey,
	)
}

// IsPlatformComplianceOrganization is the schema-qualified central check used
// before an authenticated or background tenant transaction is established.
// Reserved control markers are fail-closed: only an organization with none of
// the markers is ordinary, and only exact JSON boolean true is accepted on a
// purpose organization. A malformed or preseeded marker is never downgraded to
// ordinary tenant state.
func IsPlatformComplianceOrganization(db *gorm.DB, organizationID uuid.UUID) (bool, error) {
	if db == nil || organizationID == uuid.Nil {
		return false, ErrMissingTenant
	}
	var state struct {
		Exists     bool `gorm:"column:exists"`
		Compliance bool `gorm:"column:compliance"`
		Invalid    bool `gorm:"column:invalid"`
	}
	result := db.Raw(`
		SELECT
			true AS exists,
			COALESCE(
				pg_catalog.jsonb_typeof(settings -> CAST(? AS text)) = 'boolean'
				AND settings -> CAST(? AS text) = 'true'::jsonb,
				false
			) AS compliance,
			CASE
				WHEN NOT pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text)) THEN
					pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
					OR pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
				WHEN COALESCE(settings, '{}'::jsonb) -> CAST(? AS text) IS DISTINCT FROM 'true'::jsonb THEN true
				ELSE
					(pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
						AND COALESCE(settings, '{}'::jsonb) -> CAST(? AS text) IS DISTINCT FROM 'true'::jsonb)
					OR (pg_catalog.jsonb_exists(COALESCE(settings, '{}'::jsonb), CAST(? AS text))
						AND COALESCE(settings, '{}'::jsonb) -> CAST(? AS text) IS DISTINCT FROM 'true'::jsonb)
			END AS invalid
		FROM public.organizations
		WHERE id = ? AND deleted_at IS NULL
	`,
		PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey,
		PlatformCompliancePurposeMarkerKey,
		PlatformComplianceInstagramMarkerKey,
		PlatformComplianceThreadsMarkerKey,
		PlatformCompliancePurposeMarkerKey,
		PlatformComplianceInstagramMarkerKey, PlatformComplianceInstagramMarkerKey,
		PlatformComplianceThreadsMarkerKey, PlatformComplianceThreadsMarkerKey,
		organizationID,
	).Scan(&state)
	if result.Error != nil {
		return false, fmt.Errorf("inspect platform compliance organization: %w", result.Error)
	}
	if result.RowsAffected != 1 || !state.Exists {
		return false, gorm.ErrRecordNotFound
	}
	if state.Invalid {
		return false, ErrPlatformComplianceMarkerInvalid
	}
	return state.Compliance, nil
}

// SetPlatformComplianceOperatorIntent records the bounded run identity and
// exact reviewed operation set in the current transaction. Both settings are
// forgeable, so organization triggers additionally call the owner-only scan
// helper; runtime callers cannot turn the settings into authority.
func SetPlatformComplianceOperatorIntent(
	tx *gorm.DB,
	operatorRunID string,
	operation string,
) error {
	if tx == nil {
		return errors.New("database transaction is required")
	}
	if err := tx.Exec(
		"SELECT pg_catalog.set_config(?, ?, true)",
		platformComplianceOperatorRunIDSetting,
		operatorRunID,
	).Error; err != nil {
		return fmt.Errorf("set platform compliance operator run identity: %w", err)
	}
	if err := tx.Exec(
		"SELECT pg_catalog.set_config(?, ?, true)",
		platformComplianceOperationIntentSetting,
		operation,
	).Error; err != nil {
		return fmt.Errorf("set platform compliance operation intent: %w", err)
	}
	return nil
}

func platformCompliancePredicate(rule PlatformComplianceTableRule, rowExpression string) string {
	if strings.TrimSpace(rule.AllowedRowPredicate) == "" {
		return "false"
	}
	return strings.ReplaceAll(rule.AllowedRowPredicate, platformComplianceRowToken, rowExpression)
}

// VerifyPlatformComplianceScanAuthority proves that the current migration role
// can see historical rows before any atomic-creation/bootstrap scan. It never
// disables row security.
func VerifyPlatformComplianceScanAuthority(db *gorm.DB) error {
	if db == nil {
		return ErrPlatformComplianceScanAuthority
	}
	rules, err := PlatformComplianceTableRules()
	if err != nil {
		return err
	}
	for _, rule := range rules {
		var state struct {
			Exists             bool `gorm:"column:exists"`
			ExpectedRelkind    bool `gorm:"column:expected_relkind"`
			SelectPrivilege    bool `gorm:"column:select_privilege"`
			RowSecurity        bool `gorm:"column:row_security"`
			ForceRowSecurity   bool `gorm:"column:force_row_security"`
			PermissiveTrue     bool `gorm:"column:permissive_true"`
			UnexpectedTrue     bool `gorm:"column:unexpected_true"`
			RestrictiveNotTrue bool `gorm:"column:restrictive_not_true"`
		}
		result := db.Raw(`
			WITH relation AS (
				SELECT relation.*
				FROM pg_catalog.pg_class AS relation
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname = ?
			), current_role_oid AS (
				SELECT role.oid
				FROM pg_catalog.pg_roles AS role
				WHERE role.rolname = current_user
			), relation_policy AS (
				SELECT policy.*
				FROM pg_catalog.pg_policy AS policy
				JOIN relation ON relation.oid = policy.polrelid
			)
			SELECT
				EXISTS (SELECT 1 FROM relation) AS exists,
				COALESCE((SELECT
					relkind = 'r' AND NOT EXISTS (
						SELECT 1 FROM pg_catalog.pg_inherits AS inheritance
						WHERE inheritance.inhparent = relation.oid OR inheritance.inhrelid = relation.oid
					)
				FROM relation), false) AS expected_relkind,
				COALESCE((SELECT pg_catalog.has_table_privilege(
					current_user, relation.oid, 'SELECT'
				) FROM relation), false) AS select_privilege,
				COALESCE((SELECT relrowsecurity FROM relation), false) AS row_security,
				COALESCE((SELECT relforcerowsecurity FROM relation), false) AS force_row_security,
				EXISTS (
					SELECT 1 FROM relation_policy AS policy
					WHERE policy.polname = 'rereply_migration_access'
					  AND policy.polroles = ARRAY[(SELECT oid FROM current_role_oid)]::oid[]
					  AND policy.polpermissive AND policy.polcmd = '*'
					  AND pg_catalog.pg_get_expr(policy.polqual, policy.polrelid) = 'true'
					  AND pg_catalog.pg_get_expr(policy.polwithcheck, policy.polrelid) = 'true'
				) AS permissive_true,
				EXISTS (
					SELECT 1 FROM relation_policy AS policy
					WHERE policy.polpermissive AND policy.polcmd IN ('*', 'r')
					  AND pg_catalog.pg_get_expr(policy.polqual, policy.polrelid) = 'true'
					  AND NOT (
						policy.polname = 'rereply_migration_access'
						AND policy.polroles = ARRAY[(SELECT oid FROM current_role_oid)]::oid[]
						AND policy.polcmd = '*'
						AND pg_catalog.pg_get_expr(policy.polwithcheck, policy.polrelid) = 'true'
					  )
				) AS unexpected_true,
				EXISTS (
					SELECT 1 FROM relation_policy AS policy
					WHERE NOT policy.polpermissive AND policy.polcmd IN ('*', 'r')
					  AND EXISTS (
						SELECT 1
						FROM pg_catalog.unnest(policy.polroles) AS policy_role(oid)
						WHERE policy_role.oid = 0::oid
						   OR pg_catalog.pg_has_role(current_user, policy_role.oid, 'MEMBER')
					  )
					  AND COALESCE(pg_catalog.pg_get_expr(policy.polqual, policy.polrelid), 'true') <> 'true'
				) AS restrictive_not_true
		`, rule.Table).Scan(&state)
		if result.Error != nil {
			return fmt.Errorf("%w on %s: %v", ErrPlatformComplianceScanAuthority, rule.Table, result.Error)
		}
		if result.RowsAffected != 1 || !state.Exists || !state.SelectPrivilege {
			return fmt.Errorf("%w on %s: relation or SELECT privilege missing", ErrPlatformComplianceScanAuthority, rule.Table)
		}
		if !state.ExpectedRelkind {
			return fmt.Errorf("%w on %s: partition/inheritance hierarchies are unsupported", ErrPlatformComplianceScanAuthority, rule.Table)
		}
		if rule.ForceTenantRLS && (!state.RowSecurity || !state.ForceRowSecurity || !state.PermissiveTrue || state.UnexpectedTrue || state.RestrictiveNotTrue) {
			return fmt.Errorf("%w on %s: FORCE RLS visibility is not provably unrestricted", ErrPlatformComplianceScanAuthority, rule.Table)
		}
		if !rule.ForceTenantRLS && (state.RowSecurity || state.ForceRowSecurity) {
			return fmt.Errorf("%w on %s: pre-tenant exemptions must remain outside RLS", ErrPlatformComplianceScanAuthority, rule.Table)
		}
	}
	return nil
}

// FindPlatformComplianceDisallowedFootprint returns the first authoritative
// table containing a row that is not one of the narrow reviewed compliance
// shapes. Soft-deleted and historical rows are intentionally included.
func FindPlatformComplianceDisallowedFootprint(db *gorm.DB, organizationID uuid.UUID) (string, error) {
	if db == nil || organizationID == uuid.Nil {
		return "", ErrPlatformComplianceScanAuthority
	}
	if err := VerifyPlatformComplianceScanAuthority(db); err != nil {
		return "", err
	}
	rules, err := PlatformComplianceTableRules()
	if err != nil {
		return "", err
	}
	for _, rule := range rules {
		predicate := platformCompliancePredicate(rule, "pg_catalog.to_jsonb(candidate)")
		query := fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM public.%s AS candidate WHERE candidate.organization_id = ? AND NOT (%s))",
			quoteIdentifier(rule.Table), predicate,
		)
		var found bool
		if err := db.Raw(query, organizationID).Scan(&found).Error; err != nil {
			return "", fmt.Errorf("scan platform compliance footprint %s: %w", rule.Table, err)
		}
		if found {
			return rule.Table, nil
		}
	}
	return "", nil
}

// FindPlatformComplianceAnyFootprint is the atomic-creation scan. A UUID that
// has never identified an organization must have no historical row at all,
// including a syntactically valid compliance-shaped orphan.
func FindPlatformComplianceAnyFootprint(db *gorm.DB, organizationID uuid.UUID) (string, error) {
	if db == nil || organizationID == uuid.Nil {
		return "", ErrPlatformComplianceScanAuthority
	}
	if err := VerifyPlatformComplianceScanAuthority(db); err != nil {
		return "", err
	}
	rules, err := PlatformComplianceTableRules()
	if err != nil {
		return "", err
	}
	for _, rule := range rules {
		query := fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM public.%s WHERE organization_id = ?)",
			quoteIdentifier(rule.Table),
		)
		var found bool
		if err := db.Raw(query, organizationID).Scan(&found).Error; err != nil {
			return "", fmt.Errorf("scan atomic platform compliance footprint %s: %w", rule.Table, err)
		}
		if found {
			return rule.Table, nil
		}
	}
	return "", nil
}

type publicRelationState struct {
	Exists       bool
	Relkind      string
	HasHierarchy bool `gorm:"column:has_hierarchy"`
}

func inspectPublicTable(db *gorm.DB, table string) (publicRelationState, error) {
	if db == nil {
		return publicRelationState{}, errors.New("database connection is required")
	}
	var state publicRelationState
	result := db.Raw(`
		SELECT
			true AS exists,
			relation.relkind::text AS relkind,
			EXISTS (
				SELECT 1 FROM pg_catalog.pg_inherits AS inheritance
				WHERE inheritance.inhparent = relation.oid OR inheritance.inhrelid = relation.oid
			) AS has_hierarchy
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public' AND relation.relname = ?
	`, table).Scan(&state)
	if result.Error != nil {
		return publicRelationState{}, fmt.Errorf("inspect public.%s: %w", table, result.Error)
	}
	if result.RowsAffected == 0 {
		return publicRelationState{}, nil
	}
	if result.RowsAffected != 1 || state.Relkind != "r" || state.HasHierarchy {
		return publicRelationState{}, fmt.Errorf(
			"public.%s has unsupported relation kind/hierarchy (relkind=%q hierarchy=%t)",
			table, state.Relkind, state.HasHierarchy,
		)
	}
	return state, nil
}

func normalizePlatformComplianceFunctionACL(
	tx *gorm.DB,
	function string,
) error {
	var grantees []string
	if err := tx.Raw(`
		SELECT DISTINCT role.rolname::text
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(
			procedure.proacl,
			pg_catalog.acldefault('f', procedure.proowner)
		)) AS privilege
		JOIN pg_catalog.pg_roles AS role ON role.oid = privilege.grantee
		WHERE namespace.nspname = 'public'
		  AND procedure.proname = ? AND procedure.pronargs = 0
		  AND privilege.privilege_type = 'EXECUTE'
	`, function).Scan(&grantees).Error; err != nil {
		return fmt.Errorf("inspect platform compliance function ACL %s: %w", function, err)
	}
	if err := tx.Exec(fmt.Sprintf(
		"REVOKE ALL ON FUNCTION public.%s() FROM PUBLIC",
		quoteIdentifier(function),
	)).Error; err != nil {
		return fmt.Errorf("restrict platform compliance function %s from PUBLIC: %w", function, err)
	}
	for _, grantee := range grantees {
		if err := validateIdentifier(grantee); err != nil {
			return fmt.Errorf("invalid platform compliance function grantee %q: %w", grantee, err)
		}
		if err := tx.Exec(fmt.Sprintf(
			"REVOKE ALL ON FUNCTION public.%s() FROM %s",
			quoteIdentifier(function), quoteIdentifier(grantee),
		)).Error; err != nil {
			return fmt.Errorf("clear platform compliance function %s grant from %s: %w", function, grantee, err)
		}
	}
	return nil
}

func normalizePlatformComplianceTableACL(
	tx *gorm.DB,
	table, expectedOwner, privilege string,
) error {
	for _, identifier := range []string{table, expectedOwner} {
		if err := validateIdentifier(identifier); err != nil {
			return fmt.Errorf("invalid platform compliance table ACL identifier %q: %w", identifier, err)
		}
	}
	if privilege != "ALL" && privilege != "TRIGGER" {
		return fmt.Errorf("invalid platform compliance table privilege %q", privilege)
	}
	var grantees []string
	privilegeFilter := ""
	if privilege != "ALL" {
		privilegeFilter = " AND privilege.privilege_type = '" + privilege + "'"
	}
	if err := tx.Raw(`
		SELECT DISTINCT role.rolname::text
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(
			relation.relacl, pg_catalog.acldefault('r', relation.relowner)
		)) AS privilege
		JOIN pg_catalog.pg_roles AS role ON role.oid = privilege.grantee
		WHERE namespace.nspname = 'public'
		  AND relation.relname = CAST(? AS text)
		  AND role.rolname <> CAST(? AS text)`+privilegeFilter,
		table, expectedOwner,
	).Scan(&grantees).Error; err != nil {
		return fmt.Errorf("inspect public.%s %s ACL: %w", table, privilege, err)
	}
	tableID := quoteIdentifier(table)
	if err := tx.Exec(fmt.Sprintf(
		"REVOKE %s ON TABLE public.%s FROM PUBLIC", privilege, tableID,
	)).Error; err != nil {
		return fmt.Errorf("restrict public.%s %s privilege from PUBLIC: %w", table, privilege, err)
	}
	for _, grantee := range grantees {
		if err := validateIdentifier(grantee); err != nil {
			return fmt.Errorf("invalid public.%s ACL grantee %q: %w", table, grantee, err)
		}
		if err := tx.Exec(fmt.Sprintf(
			"REVOKE %s ON TABLE public.%s FROM %s",
			privilege, tableID, quoteIdentifier(grantee),
		)).Error; err != nil {
			return fmt.Errorf("clear public.%s %s grant from %s: %w", table, privilege, grantee, err)
		}
	}
	return nil
}

func installPlatformComplianceIdentityRegistry(
	tx *gorm.DB,
	migrationRole, runtimeRole string,
) error {
	if tx == nil {
		return errors.New("database transaction is required")
	}
	if err := validateIdentifier(migrationRole); err != nil {
		return fmt.Errorf("invalid migration role: %w", err)
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	if err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS public.organization_identity_registry (
			organization_id uuid PRIMARY KEY,
			claimed_at timestamp with time zone NOT NULL,
			database_current_user text NOT NULL,
			database_session_user text NOT NULL,
			CONSTRAINT organization_identity_registry_current_user_format
				CHECK (database_current_user ~ '^[A-Za-z_][A-Za-z0-9_]{0,62}$'),
			CONSTRAINT organization_identity_registry_session_user_format
				CHECK (database_session_user ~ '^[A-Za-z_][A-Za-z0-9_]{0,62}$')
		)
	`).Error; err != nil {
		return fmt.Errorf("install organization identity registry: %w", err)
	}
	if err := tx.Exec(fmt.Sprintf(
		"ALTER TABLE public.%s OWNER TO %s",
		quoteIdentifier(platformComplianceIdentityRegistryTable),
		quoteIdentifier(migrationRole),
	)).Error; err != nil {
		return fmt.Errorf("own organization identity registry: %w", err)
	}
	registry := quoteIdentifier(platformComplianceIdentityRegistryTable)
	for _, trigger := range []string{
		platformComplianceIdentityWriteTrigger,
		platformComplianceIdentityTruncateTrigger,
	} {
		if err := tx.Exec(fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON public.%s", quoteIdentifier(trigger), registry,
		)).Error; err != nil {
			return fmt.Errorf("prepare organization identity registry backfill: %w", err)
		}
	}
	if err := normalizePlatformComplianceTableACL(
		tx, platformComplianceIdentityRegistryTable, migrationRole, "ALL",
	); err != nil {
		return err
	}
	// Preserve UUIDs claimed by the prior unreleased tombstone design before
	// removing it. This migration is idempotent and remains private to the
	// migration owner while both relations are locked by the surrounding DDL.
	var legacyTombstones bool
	if err := tx.Raw(`
		SELECT pg_catalog.to_regclass('public.organization_identity_tombstones') IS NOT NULL
	`).Scan(&legacyTombstones).Error; err != nil {
		return fmt.Errorf("inspect legacy organization identity tombstones: %w", err)
	}
	if legacyTombstones {
		if err := tx.Exec(`
			INSERT INTO public.organization_identity_registry (
				organization_id, claimed_at, database_current_user, database_session_user
			)
			SELECT organization_id, deleted_at, database_current_user, database_session_user
			FROM public.organization_identity_tombstones
			ON CONFLICT (organization_id) DO NOTHING
		`).Error; err != nil {
			return fmt.Errorf("migrate legacy organization identity tombstones: %w", err)
		}
	}
	if err := tx.Exec(`
		INSERT INTO public.organization_identity_registry (
			organization_id, claimed_at, database_current_user, database_session_user
		)
		SELECT organization.id, COALESCE(organization.created_at, pg_catalog.clock_timestamp()),
			current_user::text, session_user::text
		FROM public.organizations AS organization
		ON CONFLICT (organization_id) DO NOTHING
	`).Error; err != nil {
		return fmt.Errorf("backfill organization identity registry: %w", err)
	}
	if legacyTombstones {
		if err := tx.Exec("DROP TABLE public.organization_identity_tombstones").Error; err != nil {
			return fmt.Errorf("remove legacy organization identity tombstones: %w", err)
		}
	}
	if err := tx.Exec(`
		DROP FUNCTION IF EXISTS public.rereply_platform_compliance_tombstone_guard()
	`).Error; err != nil {
		return fmt.Errorf("remove legacy organization identity tombstone guard: %w", err)
	}
	return nil
}

func normalizePlatformComplianceCreatorACL(tx *gorm.DB) error {
	var grantees []string
	if err := tx.Raw(`
		SELECT DISTINCT role.rolname::text
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(
			procedure.proacl,
			pg_catalog.acldefault('f', procedure.proowner)
		)) AS privilege
		JOIN pg_catalog.pg_roles AS role ON role.oid = privilege.grantee
		WHERE namespace.nspname = 'public'
		  AND procedure.proname = ?
		  AND procedure.oid = pg_catalog.to_regprocedure(CAST(? AS text))
		  AND privilege.privilege_type = 'EXECUTE'
	`, platformComplianceCreatorFunction,
		"public."+platformComplianceCreatorFunction+"(uuid,text,boolean,boolean)").Scan(&grantees).Error; err != nil {
		return fmt.Errorf("inspect platform compliance creator ACL: %w", err)
	}
	signature := fmt.Sprintf(
		"public.%s(uuid, text, boolean, boolean)",
		quoteIdentifier(platformComplianceCreatorFunction),
	)
	if err := tx.Exec(fmt.Sprintf("REVOKE ALL ON FUNCTION %s FROM PUBLIC", signature)).Error; err != nil {
		return fmt.Errorf("restrict platform compliance creator from PUBLIC: %w", err)
	}
	for _, grantee := range grantees {
		if err := validateIdentifier(grantee); err != nil {
			return fmt.Errorf("invalid platform compliance creator grantee %q: %w", grantee, err)
		}
		if err := tx.Exec(fmt.Sprintf(
			"REVOKE ALL ON FUNCTION %s FROM %s", signature, quoteIdentifier(grantee),
		)).Error; err != nil {
			return fmt.Errorf("clear platform compliance creator grant from %s: %w", grantee, err)
		}
	}
	return nil
}

func installPlatformComplianceGuards(tx *gorm.DB, migrationRole, runtimeRole string) error {
	if tx == nil {
		return errors.New("database transaction is required")
	}
	var isolation string
	if err := tx.Raw("SHOW transaction_isolation").Scan(&isolation).Error; err != nil {
		return fmt.Errorf("inspect platform compliance guard installation isolation: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(isolation), "read committed") {
		return errors.New("platform compliance guard installation requires READ COMMITTED isolation")
	}
	rules, err := PlatformComplianceTableRules()
	if err != nil {
		return err
	}
	organizations, err := inspectPublicTable(tx, "organizations")
	if err != nil {
		return err
	}
	if !organizations.Exists {
		return errors.New("platform compliance guard requires public.organizations")
	}
	// Serialize registry creation/backfill with every organization birth,
	// mutation and deletion. The registry claim trigger is installed before the
	// lock is released at commit, so there is no unguarded UUID reuse window.
	if err := tx.Exec("LOCK TABLE public.organizations IN ACCESS EXCLUSIVE MODE").Error; err != nil {
		return fmt.Errorf("lock organization identity registry installation boundary: %w", err)
	}
	if err := installPlatformComplianceIdentityRegistry(tx, migrationRole, runtimeRole); err != nil {
		return err
	}
	for _, rule := range rules {
		relation, inspectErr := inspectPublicTable(tx, rule.Table)
		if inspectErr != nil {
			return inspectErr
		}
		if !relation.Exists {
			return fmt.Errorf("platform compliance guard requires public.%s", rule.Table)
		}
	}

	scanSQL := platformComplianceScanGuardSQL(rules)
	writeSQL := platformComplianceWriteGuardSQL(rules)
	identitySQL := platformComplianceIdentityRegistryGuardSQL()
	classificationSQL := platformComplianceClassificationGuardSQL(rules)
	creationAuditSQL := platformComplianceCreationAuditSQL()
	creatorSQL := platformComplianceCreatorSQL(rules)
	contractFingerprint := platformComplianceContractFingerprint(
		rules, scanSQL, writeSQL, identitySQL, classificationSQL, creationAuditSQL, creatorSQL,
	)
	contractSQL := platformComplianceContractSQL(contractFingerprint)
	if err := tx.Exec(scanSQL).Error; err != nil {
		return fmt.Errorf("install platform compliance scan authority guard: %w", err)
	}
	if err := tx.Exec(writeSQL).Error; err != nil {
		return fmt.Errorf("install platform compliance write guard: %w", err)
	}
	if err := tx.Exec(identitySQL).Error; err != nil {
		return fmt.Errorf("install organization identity registry guard: %w", err)
	}
	if err := tx.Exec(classificationSQL).Error; err != nil {
		return fmt.Errorf("install platform compliance classification guard: %w", err)
	}
	if err := tx.Exec(creationAuditSQL).Error; err != nil {
		return fmt.Errorf("install platform compliance creation audit: %w", err)
	}
	if err := tx.Exec(creatorSQL).Error; err != nil {
		return fmt.Errorf("install atomic platform compliance creator: %w", err)
	}
	if err := tx.Exec(contractSQL).Error; err != nil {
		return fmt.Errorf("install platform compliance contract fingerprint: %w", err)
	}

	migrator := quoteIdentifier(migrationRole)
	for _, function := range []string{
		platformComplianceScanGuardFunction,
		platformComplianceWriteGuardFunction,
		platformComplianceIdentityGuardFunction,
		platformComplianceClassifyFunction,
		platformComplianceCreationAuditFunction,
		platformComplianceContractFunction,
	} {
		if err := tx.Exec(fmt.Sprintf(
			"ALTER FUNCTION public.%s() OWNER TO %s", quoteIdentifier(function), migrator,
		)).Error; err != nil {
			return fmt.Errorf("own platform compliance function %s: %w", function, err)
		}
		if err := normalizePlatformComplianceFunctionACL(tx, function); err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.%s() TO %s", quoteIdentifier(function), migrator,
		)).Error; err != nil {
			return fmt.Errorf("grant platform compliance function %s: %w", function, err)
		}
	}
	creatorSignature := fmt.Sprintf(
		"public.%s(uuid, text, boolean, boolean)",
		quoteIdentifier(platformComplianceCreatorFunction),
	)
	if err := tx.Exec(fmt.Sprintf(
		"ALTER FUNCTION %s OWNER TO %s", creatorSignature, migrator,
	)).Error; err != nil {
		return fmt.Errorf("own atomic platform compliance creator: %w", err)
	}
	if err := normalizePlatformComplianceCreatorACL(tx); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(
		"GRANT EXECUTE ON FUNCTION %s TO %s", creatorSignature, migrator,
	)).Error; err != nil {
		return fmt.Errorf("grant atomic platform compliance creator: %w", err)
	}
	if err := tx.Exec(fmt.Sprintf(
		"GRANT EXECUTE ON FUNCTION public.%s() TO %s",
		quoteIdentifier(platformComplianceContractFunction), quoteIdentifier(runtimeRole),
	)).Error; err != nil {
		return fmt.Errorf("grant platform compliance contract fingerprint to runtime: %w", err)
	}

	for _, rule := range rules {
		table := quoteIdentifier(rule.Table)
		trigger := quoteIdentifier(platformComplianceWriteTrigger)
		if err := tx.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.%s", trigger, table)).Error; err != nil {
			return err
		}
		triggerEvent := "BEFORE INSERT OR UPDATE OF organization_id"
		if strings.TrimSpace(rule.AllowedRowPredicate) != "" {
			triggerEvent = "BEFORE INSERT OR UPDATE OR DELETE"
		}
		if err := tx.Exec(fmt.Sprintf(`
			CREATE TRIGGER %s
			%s ON public.%s
			FOR EACH ROW EXECUTE FUNCTION public.%s()
		`, trigger, triggerEvent, table, quoteIdentifier(platformComplianceWriteGuardFunction))).Error; err != nil {
			return fmt.Errorf("install platform compliance write trigger on %s: %w", rule.Table, err)
		}
	}
	if err := normalizePlatformComplianceTableACL(tx, "organizations", migrationRole, "TRIGGER"); err != nil {
		return err
	}
	for _, rule := range rules {
		if err := normalizePlatformComplianceTableACL(tx, rule.Table, migrationRole, "TRIGGER"); err != nil {
			return err
		}
	}
	classifyTrigger := quoteIdentifier(platformComplianceClassifyTrigger)
	if err := tx.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.organizations", classifyTrigger)).Error; err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE INSERT OR UPDATE OR DELETE ON public.organizations
		FOR EACH ROW EXECUTE FUNCTION public.%s()
	`, classifyTrigger, quoteIdentifier(platformComplianceClassifyFunction))).Error; err != nil {
		return fmt.Errorf("install platform compliance classification trigger: %w", err)
	}
	auditTrigger := quoteIdentifier(platformComplianceCreationAuditTrigger)
	if err := tx.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.organizations", auditTrigger)).Error; err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		AFTER INSERT ON public.organizations
		FOR EACH ROW EXECUTE FUNCTION public.%s()
	`, auditTrigger, quoteIdentifier(platformComplianceCreationAuditFunction))).Error; err != nil {
		return fmt.Errorf("install platform compliance creation audit trigger: %w", err)
	}
	truncateTrigger := quoteIdentifier(platformComplianceTruncateTrigger)
	if err := tx.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.organizations", truncateTrigger)).Error; err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE TRUNCATE ON public.organizations
		FOR EACH STATEMENT EXECUTE FUNCTION public.%s()
	`, truncateTrigger, quoteIdentifier(platformComplianceClassifyFunction))).Error; err != nil {
		return fmt.Errorf("install organization identity truncate guard: %w", err)
	}
	identityWriteTrigger := quoteIdentifier(platformComplianceIdentityWriteTrigger)
	identityTable := quoteIdentifier(platformComplianceIdentityRegistryTable)
	if err := tx.Exec(fmt.Sprintf(
		"DROP TRIGGER IF EXISTS %s ON public.%s", identityWriteTrigger, identityTable,
	)).Error; err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE INSERT OR UPDATE OR DELETE ON public.%s
		FOR EACH ROW EXECUTE FUNCTION public.%s()
	`, identityWriteTrigger, identityTable,
		quoteIdentifier(platformComplianceIdentityGuardFunction))).Error; err != nil {
		return fmt.Errorf("install organization identity registry write guard: %w", err)
	}
	identityTruncateTrigger := quoteIdentifier(platformComplianceIdentityTruncateTrigger)
	if err := tx.Exec(fmt.Sprintf(
		"DROP TRIGGER IF EXISTS %s ON public.%s", identityTruncateTrigger, identityTable,
	)).Error; err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE TRUNCATE ON public.%s
		FOR EACH STATEMENT EXECUTE FUNCTION public.%s()
	`, identityTruncateTrigger, identityTable,
		quoteIdentifier(platformComplianceIdentityGuardFunction))).Error; err != nil {
		return fmt.Errorf("install organization identity registry truncate guard: %w", err)
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE UNIQUE INDEX IF NOT EXISTS %s
		ON public.audit_logs (organization_id)
		WHERE resource_type = 'platform_compliance'
		  AND changes @> '[{"field":"platform_compliance_tenant","old_value":null,"new_value":true}]'::jsonb
	`, quoteIdentifier(platformComplianceCreationEvidenceIndex))).Error; err != nil {
		return fmt.Errorf("install unique platform compliance creation evidence index: %w", err)
	}
	if err := tx.Exec(fmt.Sprintf(`
		CREATE UNIQUE INDEX IF NOT EXISTS %s
		ON public.audit_logs (organization_id, user_name)
		WHERE resource_type = 'platform_compliance'
	`, quoteIdentifier(platformComplianceAuditRunIndex))).Error; err != nil {
		return fmt.Errorf("install unique platform compliance audit run index: %w", err)
	}

	if err := VerifyPlatformComplianceScanAuthority(tx); err != nil {
		return err
	}
	if err := verifyNoOrdinaryPlatformComplianceMarkers(tx); err != nil {
		return err
	}
	if err := verifyExistingPlatformComplianceOrganizations(tx); err != nil {
		return err
	}
	return verifyPlatformComplianceGuards(tx, runtimeRole)
}

func platformComplianceContractFingerprint(
	rules []PlatformComplianceTableRule,
	functionSQL ...string,
) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "platform-compliance-guard-v%d\n", platformComplianceGuardVersion)
	for _, rule := range rules {
		mode := "organization_id-update-only"
		if strings.TrimSpace(rule.AllowedRowPredicate) != "" {
			mode = "all-update-row-shape"
		}
		_, _ = fmt.Fprintf(hash, "%s|%t|%s|%s\n", rule.Table, rule.ForceTenantRLS, mode, rule.AllowedRowPredicate)
	}
	for _, statement := range functionSQL {
		_, _ = hash.Write([]byte(statement))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("v%d:%s", platformComplianceGuardVersion, hex.EncodeToString(hash.Sum(nil)))
}

func platformComplianceContractSQL(fingerprint string) string {
	return fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION public.%s()
		RETURNS text
		LANGUAGE sql
		IMMUTABLE
		SET search_path = pg_catalog, public
		AS $function$
			SELECT '%s'::text
		$function$;
	`, quoteIdentifier(platformComplianceContractFunction), fingerprint)
}

func platformComplianceScanGuardSQL(rules []PlatformComplianceTableRule) string {
	var values strings.Builder
	for index, rule := range rules {
		if index > 0 {
			values.WriteString(",")
		}
		fmt.Fprintf(&values, "('%s'::text, %t::boolean)", rule.Table, rule.ForceTenantRLS)
	}
	return fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION public.%s()
		RETURNS void
		LANGUAGE plpgsql
		SECURITY DEFINER
		SET search_path = pg_catalog, public
		AS $function$
		DECLARE
			table_rule record;
			state record;
		BEGIN
			FOR table_rule IN SELECT * FROM (VALUES %s) AS rules(table_name, force_rls)
			LOOP
				WITH relation AS (
					SELECT relation.*
					FROM pg_class AS relation
					JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
					WHERE namespace.nspname = 'public' AND relation.relname = table_rule.table_name
				), current_role_oid AS (
					SELECT role.oid
					FROM pg_roles AS role
					WHERE role.rolname = current_user
				), relation_policy AS (
					SELECT policy.*
					FROM pg_policy AS policy
					JOIN relation ON relation.oid = policy.polrelid
				)
				SELECT
					EXISTS (SELECT 1 FROM relation) AS relation_exists,
					COALESCE((SELECT
						relkind = 'r' AND NOT EXISTS (
							SELECT 1 FROM pg_inherits AS inheritance
							WHERE inheritance.inhparent = relation.oid OR inheritance.inhrelid = relation.oid
						)
					FROM relation), false) AS expected_relkind,
					COALESCE((SELECT has_table_privilege(current_user, relation.oid, 'SELECT') FROM relation), false) AS can_select,
					COALESCE((SELECT relrowsecurity FROM relation), false) AS row_security,
					COALESCE((SELECT relforcerowsecurity FROM relation), false) AS force_row_security,
					EXISTS (
						SELECT 1 FROM relation_policy AS policy
						WHERE policy.polname = 'rereply_migration_access'
						  AND policy.polroles = ARRAY[(SELECT oid FROM current_role_oid)]::oid[]
						  AND policy.polpermissive AND policy.polcmd = '*'
						  AND pg_get_expr(policy.polqual, policy.polrelid) = 'true'
						  AND pg_get_expr(policy.polwithcheck, policy.polrelid) = 'true'
					) AS permissive_true,
					EXISTS (
						SELECT 1 FROM relation_policy AS policy
						WHERE policy.polpermissive AND policy.polcmd IN ('*', 'r')
						  AND pg_get_expr(policy.polqual, policy.polrelid) = 'true'
						  AND NOT (
							policy.polname = 'rereply_migration_access'
							AND policy.polroles = ARRAY[(SELECT oid FROM current_role_oid)]::oid[]
							AND policy.polcmd = '*'
							AND pg_get_expr(policy.polwithcheck, policy.polrelid) = 'true'
						  )
					) AS unexpected_true,
					EXISTS (
						SELECT 1 FROM relation_policy AS policy
						WHERE NOT policy.polpermissive AND policy.polcmd IN ('*', 'r')
						  AND EXISTS (
							SELECT 1
							FROM pg_catalog.unnest(policy.polroles) AS policy_role(oid)
							WHERE policy_role.oid = 0::oid
							   OR pg_catalog.pg_has_role(current_user, policy_role.oid, 'MEMBER')
						  )
						  AND COALESCE(pg_get_expr(policy.polqual, policy.polrelid), 'true') <> 'true'
					) AS restrictive_not_true
				INTO state;

				IF state.relation_exists IS DISTINCT FROM true
					OR state.expected_relkind IS DISTINCT FROM true
					OR state.can_select IS DISTINCT FROM true THEN
					RAISE EXCEPTION 'platform compliance scan authority is incomplete on %%', table_rule.table_name USING ERRCODE = '42501';
				END IF;
				IF table_rule.force_rls AND (
					state.row_security IS DISTINCT FROM true
					OR state.force_row_security IS DISTINCT FROM true
					OR state.permissive_true IS DISTINCT FROM true
					OR state.unexpected_true IS TRUE
					OR state.restrictive_not_true IS TRUE
				) THEN
					RAISE EXCEPTION 'platform compliance FORCE RLS scan policy is incomplete on %%', table_rule.table_name USING ERRCODE = '42501';
				END IF;
				IF NOT table_rule.force_rls AND (
					state.row_security IS TRUE OR state.force_row_security IS TRUE
				) THEN
					RAISE EXCEPTION 'platform compliance exemption unexpectedly uses RLS on %%', table_rule.table_name USING ERRCODE = '42501';
				END IF;
			END LOOP;
		END
		$function$;
	`, quoteIdentifier(platformComplianceScanGuardFunction), values.String())
}

func platformComplianceLiveWritePredicate(rule PlatformComplianceTableRule) string {
	newPredicate := platformCompliancePredicate(rule, "row_data")
	switch rule.Table {
	case "audit_logs":
		// Evidence may only be emitted by the nested organization trigger in the
		// same transaction as the creation or feature transition it attests.
		return `(TG_OP = 'INSERT'
			AND pg_catalog.pg_trigger_depth() = 2
			AND (` + newPredicate + `)
			AND EXISTS (
				SELECT 1 FROM pg_catalog.jsonb_array_elements(row_data->'changes') AS item
				WHERE item->>'field' = 'database_current_user'
				  AND item->>'new_value' = current_user::text
			)
			AND EXISTS (
				SELECT 1 FROM pg_catalog.jsonb_array_elements(row_data->'changes') AS item
				WHERE item->>'field' = 'database_session_user'
				  AND item->>'new_value' = session_user::text
			))`
	case "privacy_requests", "privacy_request_events":
		// These retained workflow/evidence rows have no reviewed update or
		// disposition path in this phase. In particular, a GORM soft delete is
		// an UPDATE and must not hide them.
		return `(TG_OP = 'INSERT' AND instagram_feature_enabled AND (` + newPredicate + `))`
	case "meta_instagram_data_deletion_events":
		oldPredicate := platformCompliancePredicate(rule, "old_row_data")
		parentPredicate := strings.ReplaceAll(
			platformCompliancePrivacyRequestPredicate,
			platformComplianceRowToken,
			"pg_catalog.to_jsonb(parent_request)",
		)
		return `(instagram_feature_enabled AND (
			(
				TG_OP = 'INSERT'
				AND row_data->>'state' = 'verified'
				AND (` + newPredicate + `)
			)
			OR (
				TG_OP = 'UPDATE'
				AND old_row_data->>'state' = 'verified'
				AND row_data->>'state' = 'completed'
				AND (` + oldPredicate + `)
				AND (` + newPredicate + `)
				AND (
					old_row_data - ARRAY['state', 'privacy_request_id', 'request_number', 'last_attempt_at', 'completed_at']::text[]
				) = (
					row_data - ARRAY['state', 'privacy_request_id', 'request_number', 'last_attempt_at', 'completed_at']::text[]
				)
				AND row_data->'last_attempt_at' = row_data->'completed_at'
				AND (row_data->>'completed_at')::timestamptz >= (old_row_data->>'verified_at')::timestamptz
				AND EXISTS (
					SELECT 1
					FROM public.privacy_requests AS parent_request
					WHERE parent_request.id = (row_data->>'privacy_request_id')::uuid
					  AND parent_request.organization_id = (row_data->>'organization_id')::uuid
					  AND parent_request.request_number = row_data->>'request_number'
					  AND parent_request.subject_key = row_data->>'authorizing_user_id'
					  AND parent_request.request_details->>'target_resolution' = row_data->>'target_resolution'
					  AND (` + parentPredicate + `)
				)
			)
		))`
	default:
		return "false"
	}
}

func platformComplianceWriteGuardSQL(rules []PlatformComplianceTableRule) string {
	var cases strings.Builder
	auditLivePredicate := "false"
	for _, rule := range rules {
		if rule.Table == "audit_logs" {
			auditLivePredicate = platformComplianceLiveWritePredicate(rule)
		}
		fmt.Fprintf(
			&cases,
			"\n\t\t\t\tWHEN '%s' THEN allowed_row := (%s);",
			rule.Table,
			platformComplianceLiveWritePredicate(rule),
		)
	}
	return fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION public.%s()
		RETURNS trigger
		LANGUAGE plpgsql
		SECURITY DEFINER
		SET search_path = pg_catalog, public
		AS $function$
		DECLARE
			purpose_enabled boolean := false;
			old_purpose_enabled boolean := false;
			instagram_feature_enabled boolean := false;
			allowed_row boolean := false;
			row_data jsonb;
			old_row_data jsonb;
			organization_id uuid;
		BEGIN
			IF TG_OP = 'DELETE' THEN
				row_data := to_jsonb(OLD);
				old_row_data := row_data;
				organization_id := OLD.organization_id;
			ELSE
				row_data := to_jsonb(NEW);
				organization_id := NEW.organization_id;
				IF TG_OP = 'UPDATE' THEN
					old_row_data := to_jsonb(OLD);
				END IF;
			END IF;
			IF organization_id IS NULL THEN
				IF TG_OP = 'DELETE' THEN
					RETURN OLD;
				END IF;
				RETURN NEW;
			END IF;
			IF TG_OP = 'UPDATE' AND OLD.organization_id IS DISTINCT FROM NEW.organization_id
				AND OLD.organization_id IS NOT NULL THEN
				PERFORM 1 FROM public.organizations WHERE id = OLD.organization_id FOR SHARE;
				IF NOT FOUND THEN
					RAISE EXCEPTION 'former organization %% does not exist for table %%', OLD.organization_id, TG_TABLE_NAME USING ERRCODE = '23503';
				END IF;
				SELECT COALESCE(
					jsonb_typeof(settings -> '%s') = 'boolean'
					AND settings -> '%s' = 'true'::jsonb,
					false
				)
				INTO old_purpose_enabled
				FROM public.organizations
				WHERE id = OLD.organization_id;
				IF old_purpose_enabled THEN
					RAISE EXCEPTION 'table %% rejects moving a platform compliance row out of organization %%', TG_TABLE_NAME, OLD.organization_id USING ERRCODE = '23514';
				END IF;
			END IF;

			PERFORM 1 FROM public.organizations WHERE id = organization_id FOR SHARE;
			IF NOT FOUND THEN
				RAISE EXCEPTION 'organization %% does not exist for table %%', organization_id, TG_TABLE_NAME USING ERRCODE = '23503';
			END IF;
			SELECT
				COALESCE(
					jsonb_typeof(settings -> '%s') = 'boolean'
					AND settings -> '%s' = 'true'::jsonb,
					false
				),
				COALESCE(
					jsonb_typeof(settings -> '%s') = 'boolean'
					AND settings -> '%s' = 'true'::jsonb,
					false
				)
			INTO purpose_enabled, instagram_feature_enabled
			FROM public.organizations
			WHERE id = organization_id;
			-- Exact control-plane audit evidence is append-only and must be emitted
			-- by the nested organization trigger while the purpose row exists.
			IF TG_TABLE_NAME = 'audit_logs'
				AND (
					row_data->>'resource_type' = 'platform_compliance'
					OR COALESCE(old_row_data->>'resource_type', '') = 'platform_compliance'
				) THEN
				IF NOT purpose_enabled OR NOT (%s) THEN
					RAISE EXCEPTION 'platform compliance audit evidence is transaction-bound and append-only' USING ERRCODE = '23514';
				END IF;
				RETURN NEW;
			END IF;
			IF NOT purpose_enabled THEN
				IF TG_OP = 'DELETE' THEN
					RETURN OLD;
				END IF;
				RETURN NEW;
			END IF;
			IF TG_OP = 'DELETE' THEN
				RAISE EXCEPTION 'table %% rejects deleting retained platform compliance evidence for organization %%', TG_TABLE_NAME, organization_id USING ERRCODE = '23514';
			END IF;

			CASE TG_TABLE_NAME%s
				ELSE allowed_row := false;
			END CASE;
			IF NOT allowed_row THEN
				RAISE EXCEPTION 'table %% rejects rows for platform compliance organization %%', TG_TABLE_NAME, organization_id USING ERRCODE = '23514';
			END IF;
			RETURN NEW;
		END
		$function$;
	`, quoteIdentifier(platformComplianceWriteGuardFunction),
		PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey,
		PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey,
		PlatformComplianceInstagramMarkerKey, PlatformComplianceInstagramMarkerKey,
		auditLivePredicate,
		cases.String())
}

func platformComplianceIdentityRegistryGuardSQL() string {
	sql := `
		CREATE OR REPLACE FUNCTION public.{{identity_function}}()
		RETURNS trigger
		LANGUAGE plpgsql
		SECURITY DEFINER
		SET search_path = pg_catalog, public
		AS $function$
		DECLARE
			intent text;
		BEGIN
			IF TG_OP = 'TRUNCATE' THEN
				RAISE EXCEPTION 'organization identity registry is append-only' USING ERRCODE = '23514';
			END IF;
			IF TG_OP <> 'INSERT' THEN
				RAISE EXCEPTION 'organization identity registry is immutable' USING ERRCODE = '23514';
			END IF;
			intent := COALESCE(current_setting('{{identity_intent}}', true), '');
			IF pg_catalog.pg_trigger_depth() <> 2
				OR intent <> NEW.organization_id::text
				OR NEW.claimed_at IS NULL
				OR NEW.database_current_user IS DISTINCT FROM current_user::text
				OR NEW.database_session_user IS DISTINCT FROM session_user::text THEN
				RAISE EXCEPTION 'organization identities may only be claimed by the organization insert guard' USING ERRCODE = '42501';
			END IF;
			RETURN NEW;
		END
		$function$;
	`
	return strings.NewReplacer(
		"{{identity_function}}", quoteIdentifier(platformComplianceIdentityGuardFunction),
		"{{identity_intent}}", platformComplianceIdentityIntentSetting,
	).Replace(sql)
}

func platformComplianceClassificationGuardSQL(rules []PlatformComplianceTableRule) string {
	var scans strings.Builder
	for _, rule := range rules {
		fmt.Fprintf(&scans, `
				IF EXISTS (
					SELECT 1 FROM public.%s AS candidate
					WHERE candidate.organization_id = NEW.id
				) THEN
					RAISE EXCEPTION 'organization %% has historical rows in %s', NEW.id USING ERRCODE = '23514';
				END IF;
`, quoteIdentifier(rule.Table), rule.Table)
	}
	sql := `
		CREATE OR REPLACE FUNCTION public.{{classify_function}}()
		RETURNS trigger
		LANGUAGE plpgsql
		SECURITY DEFINER
		SET search_path = pg_catalog, public
		AS $function$
		DECLARE
			old_purpose boolean := false;
			new_purpose boolean := false;
			instagram_changed boolean := false;
			threads_changed boolean := false;
			operation text := '';
			operator_run_id text := '';
			creation_intent jsonb;
			expected_settings jsonb;
			changes jsonb;
			platform_reseller_id uuid;
		BEGIN
			IF TG_OP = 'TRUNCATE' THEN
				RAISE EXCEPTION 'organization identity registry cannot be truncated' USING ERRCODE = '23514';
			END IF;
			IF TG_OP = 'DELETE' THEN
				old_purpose := COALESCE(
					jsonb_typeof(OLD.settings -> '{{purpose_marker}}') = 'boolean'
					AND OLD.settings -> '{{purpose_marker}}' = 'true'::jsonb,
					false
				);
				IF old_purpose THEN
					RAISE EXCEPTION 'platform compliance organization lifecycle is immutable' USING ERRCODE = '23514';
				END IF;
				RETURN OLD;
			END IF;
			IF TG_OP = 'INSERT' THEN
				PERFORM pg_catalog.set_config('{{identity_intent}}', NEW.id::text, true);
				INSERT INTO public.{{identity_table}} (
					organization_id, claimed_at, database_current_user, database_session_user
				) VALUES (
					NEW.id, pg_catalog.clock_timestamp(), current_user::text, session_user::text
				);
				PERFORM pg_catalog.set_config('{{identity_intent}}', '', true);
				IF NOT (
					COALESCE(NEW.settings, '{}'::jsonb) ? '{{purpose_marker}}'
					OR COALESCE(NEW.settings, '{}'::jsonb) ? '{{instagram_marker}}'
					OR COALESCE(NEW.settings, '{}'::jsonb) ? '{{threads_marker}}'
				) THEN
					RETURN NEW;
				END IF;
				new_purpose := COALESCE(
					jsonb_typeof(NEW.settings -> '{{purpose_marker}}') = 'boolean'
					AND NEW.settings -> '{{purpose_marker}}' = 'true'::jsonb,
					false
				);
				IF NOT new_purpose
					OR (NEW.settings ? '{{instagram_marker}}' AND NEW.settings -> '{{instagram_marker}}' IS DISTINCT FROM 'true'::jsonb)
					OR (NEW.settings ? '{{threads_marker}}' AND NEW.settings -> '{{threads_marker}}' IS DISTINCT FROM 'true'::jsonb) THEN
					RAISE EXCEPTION 'platform compliance control markers are invalid on organization insert' USING ERRCODE = '23514';
				END IF;
				IF session_user::text IS DISTINCT FROM current_user::text THEN
					RAISE EXCEPTION 'atomic platform compliance creation requires the direct migration owner session' USING ERRCODE = '42501';
				END IF;
				BEGIN
					creation_intent := NULLIF(current_setting('{{creation_intent}}', true), '')::jsonb;
				EXCEPTION WHEN OTHERS THEN
					RAISE EXCEPTION 'atomic platform compliance creation intent is invalid' USING ERRCODE = '42501';
				END;
				IF creation_intent IS NULL
					OR (
						SELECT pg_catalog.array_agg(key ORDER BY key)
						FROM pg_catalog.jsonb_object_keys(creation_intent) AS keys(key)
					) IS DISTINCT FROM ARRAY[
						'instagram', 'name', 'operator_run_id', 'organization_id',
						'phase', 'reseller_id', 'slug', 'threads', 'token'
					]::text[]
					OR COALESCE(creation_intent->>'phase', '') <> 'insert'
					OR COALESCE(creation_intent->>'token', '') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
					OR COALESCE(creation_intent->>'operator_run_id', '') !~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$'
					OR creation_intent->>'organization_id' IS DISTINCT FROM NEW.id::text
					OR creation_intent->>'reseller_id' IS DISTINCT FROM NEW.reseller_id::text
					OR creation_intent->>'name' IS DISTINCT FROM NEW.name
					OR creation_intent->>'slug' IS DISTINCT FROM NEW.slug
					OR pg_catalog.jsonb_typeof(creation_intent->'instagram') IS DISTINCT FROM 'boolean'
					OR pg_catalog.jsonb_typeof(creation_intent->'threads') IS DISTINCT FROM 'boolean'
					OR NEW.deleted_at IS NOT NULL
					OR NEW.created_at IS NULL
					OR NEW.updated_at IS DISTINCT FROM NEW.created_at THEN
					RAISE EXCEPTION 'atomic platform compliance creation payload does not match inserted organization' USING ERRCODE = '42501';
				END IF;
				expected_settings := pg_catalog.jsonb_build_object('{{purpose_marker}}', true);
				IF (creation_intent->>'instagram')::boolean THEN
					expected_settings := expected_settings || pg_catalog.jsonb_build_object('{{instagram_marker}}', true);
				END IF;
				IF (creation_intent->>'threads')::boolean THEN
					expected_settings := expected_settings || pg_catalog.jsonb_build_object('{{threads_marker}}', true);
				END IF;
				IF COALESCE(NEW.settings, '{}'::jsonb) IS DISTINCT FROM expected_settings THEN
					RAISE EXCEPTION 'atomic platform compliance organization settings are not canonical' USING ERRCODE = '23514';
				END IF;
				IF NEW.name IS DISTINCT FROM 'Platform Compliance ' || NEW.id::text
					OR NEW.slug IS DISTINCT FROM 'platform-compliance-' || NEW.id::text THEN
					RAISE EXCEPTION 'atomic platform compliance organization identity is not canonical' USING ERRCODE = '23514';
				END IF;
				PERFORM public.{{scan_function}}();
{{footprint_scans}}
				SELECT reseller.id
				INTO platform_reseller_id
				FROM public.resellers AS reseller
				WHERE reseller.id = NEW.reseller_id
				  AND reseller.slug = 'platform-direct'
				  AND reseller.status = 'active'
				  AND reseller.deleted_at IS NULL
				FOR SHARE;
				IF platform_reseller_id IS NULL THEN
					RAISE EXCEPTION 'atomic platform compliance organization requires the active platform-direct reseller' USING ERRCODE = '23514';
				END IF;
				creation_intent := pg_catalog.jsonb_set(creation_intent, '{phase}', '"audit"'::jsonb, false);
				PERFORM pg_catalog.set_config('{{creation_intent}}', creation_intent::text, true);
				RETURN NEW;
			END IF;

			IF OLD.id IS DISTINCT FROM NEW.id THEN
				RAISE EXCEPTION 'organization UUID identity is immutable' USING ERRCODE = '23514';
			END IF;
			old_purpose := COALESCE(
				jsonb_typeof(OLD.settings -> '{{purpose_marker}}') = 'boolean'
				AND OLD.settings -> '{{purpose_marker}}' = 'true'::jsonb,
				false
			);
			new_purpose := COALESCE(
				jsonb_typeof(NEW.settings -> '{{purpose_marker}}') = 'boolean'
				AND NEW.settings -> '{{purpose_marker}}' = 'true'::jsonb,
				false
			);
			instagram_changed :=
				(OLD.settings ? '{{instagram_marker}}') IS DISTINCT FROM (NEW.settings ? '{{instagram_marker}}')
				OR OLD.settings -> '{{instagram_marker}}' IS DISTINCT FROM NEW.settings -> '{{instagram_marker}}';
			threads_changed :=
				(OLD.settings ? '{{threads_marker}}') IS DISTINCT FROM (NEW.settings ? '{{threads_marker}}')
				OR OLD.settings -> '{{threads_marker}}' IS DISTINCT FROM NEW.settings -> '{{threads_marker}}';
			IF NOT old_purpose THEN
				IF new_purpose
					OR (OLD.settings ? '{{purpose_marker}}') IS DISTINCT FROM (NEW.settings ? '{{purpose_marker}}')
					OR OLD.settings -> '{{purpose_marker}}' IS DISTINCT FROM NEW.settings -> '{{purpose_marker}}'
					OR instagram_changed OR threads_changed
					OR NEW.settings ? '{{instagram_marker}}'
					OR NEW.settings ? '{{threads_marker}}' THEN
					RAISE EXCEPTION 'existing ordinary organizations cannot acquire or mutate platform compliance markers' USING ERRCODE = '23514';
				END IF;
				RETURN NEW;
			END IF;
			IF NOT new_purpose THEN
				RAISE EXCEPTION 'platform compliance purpose classification is immutable' USING ERRCODE = '23514';
			END IF;
			IF (
				(to_jsonb(NEW) - ARRAY['settings', 'updated_at']::text[])
				IS DISTINCT FROM
				(to_jsonb(OLD) - ARRAY['settings', 'updated_at']::text[])
			) THEN
				RAISE EXCEPTION 'platform compliance organization identity and lifecycle are immutable' USING ERRCODE = '23514';
			END IF;
			IF (COALESCE(NEW.settings, '{}'::jsonb) - ARRAY['{{purpose_marker}}', '{{instagram_marker}}', '{{threads_marker}}']::text[])
				IS DISTINCT FROM
				(COALESCE(OLD.settings, '{}'::jsonb) - ARRAY['{{purpose_marker}}', '{{instagram_marker}}', '{{threads_marker}}']::text[]) THEN
				RAISE EXCEPTION 'platform compliance organization settings are immutable outside reviewed feature markers' USING ERRCODE = '23514';
			END IF;
			IF NOT instagram_changed AND NOT threads_changed THEN
				RAISE EXCEPTION 'platform compliance organization rows are immutable outside reviewed feature transitions' USING ERRCODE = '23514';
			END IF;
			IF NEW.settings ? '{{instagram_marker}}'
				AND NEW.settings -> '{{instagram_marker}}' IS DISTINCT FROM 'true'::jsonb THEN
				RAISE EXCEPTION 'Instagram compliance feature marker must be exact boolean true or absent' USING ERRCODE = '23514';
			END IF;
			IF NEW.settings ? '{{threads_marker}}'
				AND NEW.settings -> '{{threads_marker}}' IS DISTINCT FROM 'true'::jsonb THEN
				RAISE EXCEPTION 'Threads compliance feature marker must be exact boolean true or absent' USING ERRCODE = '23514';
			END IF;
			IF session_user::text IS DISTINCT FROM current_user::text THEN
				RAISE EXCEPTION 'platform compliance feature transitions require the direct migration owner session' USING ERRCODE = '42501';
			END IF;
			operator_run_id := COALESCE(current_setting('{{operator_intent}}', true), '');
			IF operator_run_id !~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$' THEN
				RAISE EXCEPTION 'platform compliance operator run identity is missing or invalid' USING ERRCODE = '42501';
			END IF;
			operation := COALESCE(current_setting('{{operation_intent}}', true), '');
			IF operation !~ '^(enable|remove):(instagram|threads)(,(enable|remove):(instagram|threads))?$'
				OR (
					SELECT pg_catalog.count(*) <> pg_catalog.count(DISTINCT token)
					FROM pg_catalog.unnest(pg_catalog.string_to_array(operation, ',')) AS selected(token)
				)
				OR (operation ~ '(^|,)enable:instagram(,|$)' AND operation ~ '(^|,)remove:instagram(,|$)')
				OR (operation ~ '(^|,)enable:threads(,|$)' AND operation ~ '(^|,)remove:threads(,|$)') THEN
				RAISE EXCEPTION 'platform compliance feature operation intent is invalid' USING ERRCODE = '42501';
			END IF;
			IF instagram_changed AND (
				(NEW.settings ? '{{instagram_marker}}' AND operation !~ '(^|,)enable:instagram(,|$)')
				OR (NOT (NEW.settings ? '{{instagram_marker}}') AND operation !~ '(^|,)remove:instagram(,|$)')
			) THEN
				RAISE EXCEPTION 'Instagram compliance feature transition does not match operator intent' USING ERRCODE = '42501';
			END IF;
			IF NOT instagram_changed AND operation ~ '(^|,)(enable|remove):instagram(,|$)' THEN
				RAISE EXCEPTION 'Instagram compliance operation has no matching marker transition' USING ERRCODE = '42501';
			END IF;
			IF threads_changed AND (
				(NEW.settings ? '{{threads_marker}}' AND operation !~ '(^|,)enable:threads(,|$)')
				OR (NOT (NEW.settings ? '{{threads_marker}}') AND operation !~ '(^|,)remove:threads(,|$)')
			) THEN
				RAISE EXCEPTION 'Threads compliance feature transition does not match operator intent' USING ERRCODE = '42501';
			END IF;
			IF NOT threads_changed AND operation ~ '(^|,)(enable|remove):threads(,|$)' THEN
				RAISE EXCEPTION 'Threads compliance operation has no matching marker transition' USING ERRCODE = '42501';
			END IF;
			PERFORM public.{{scan_function}}();
			changes := '[]'::jsonb;
			IF instagram_changed THEN
				changes := changes || pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
					'field', '{{instagram_marker}}',
					'old_value', CASE WHEN OLD.settings ? '{{instagram_marker}}' THEN true ELSE NULL END,
					'new_value', CASE WHEN NEW.settings ? '{{instagram_marker}}' THEN true ELSE NULL END
				));
			END IF;
			IF threads_changed THEN
				changes := changes || pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
					'field', '{{threads_marker}}',
					'old_value', CASE WHEN OLD.settings ? '{{threads_marker}}' THEN true ELSE NULL END,
					'new_value', CASE WHEN NEW.settings ? '{{threads_marker}}' THEN true ELSE NULL END
				));
			END IF;
			changes := changes || pg_catalog.jsonb_build_array(
				pg_catalog.jsonb_build_object(
					'field', 'operator_run_id', 'old_value', NULL, 'new_value', operator_run_id
				),
				pg_catalog.jsonb_build_object(
					'field', 'database_current_user', 'old_value', NULL, 'new_value', current_user::text
				),
				pg_catalog.jsonb_build_object(
					'field', 'database_session_user', 'old_value', NULL, 'new_value', session_user::text
				),
				pg_catalog.jsonb_build_object(
					'field', 'control_plane_operation', 'old_value', NULL, 'new_value', operation
				)
			);
			INSERT INTO public.audit_logs (
				id, organization_id, resource_type, resource_id,
				user_id, user_name, action, changes, created_at
			) VALUES (
				pg_catalog.gen_random_uuid(), NEW.id, 'platform_compliance', NEW.id,
				'00000000-0000-0000-0000-000000000000'::uuid,
				'platform-compliance-bootstrap/' || operator_run_id,
				'updated', changes, pg_catalog.clock_timestamp()
			);
			RETURN NEW;
		END
		$function$;
	`
	return strings.NewReplacer(
		"{{classify_function}}", quoteIdentifier(platformComplianceClassifyFunction),
		"{{scan_function}}", quoteIdentifier(platformComplianceScanGuardFunction),
		"{{identity_table}}", quoteIdentifier(platformComplianceIdentityRegistryTable),
		"{{purpose_marker}}", PlatformCompliancePurposeMarkerKey,
		"{{instagram_marker}}", PlatformComplianceInstagramMarkerKey,
		"{{threads_marker}}", PlatformComplianceThreadsMarkerKey,
		"{{operator_intent}}", platformComplianceOperatorRunIDSetting,
		"{{operation_intent}}", platformComplianceOperationIntentSetting,
		"{{creation_intent}}", platformComplianceCreationIntentSetting,
		"{{identity_intent}}", platformComplianceIdentityIntentSetting,
		"{{footprint_scans}}", scans.String(),
	).Replace(sql)
}

func platformComplianceCreatorSQL(rules []PlatformComplianceTableRule) string {
	var scans strings.Builder
	for _, rule := range rules {
		fmt.Fprintf(&scans, `
			IF EXISTS (
				SELECT 1 FROM public.%s AS candidate
				WHERE candidate.organization_id = requested_organization_id
			) THEN
				RAISE EXCEPTION 'organization %% has historical rows in %s', requested_organization_id USING ERRCODE = '23514';
			END IF;
`, quoteIdentifier(rule.Table), rule.Table)
	}
	sql := `
		CREATE OR REPLACE FUNCTION public.{{creator_function}}(
			requested_organization_id uuid,
			operator_run_id text,
			enable_instagram boolean,
			enable_threads boolean
		)
		RETURNS uuid
		LANGUAGE plpgsql
		SECURITY DEFINER
		SET search_path = pg_catalog, public
		AS $function$
		DECLARE
			platform_reseller_id uuid;
			creation_token uuid;
			creation_intent jsonb;
			created_at timestamptz;
			organization_name text;
			organization_slug text;
			organization_settings jsonb;
		BEGIN
			IF session_user::text IS DISTINCT FROM current_user::text THEN
				RAISE EXCEPTION 'atomic platform compliance creation requires the direct migration owner session' USING ERRCODE = '42501';
			END IF;
			IF requested_organization_id IS NULL
				OR requested_organization_id = '00000000-0000-0000-0000-000000000000'::uuid THEN
				RAISE EXCEPTION 'platform compliance organization UUID is invalid' USING ERRCODE = '22023';
			END IF;
			IF operator_run_id !~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$' THEN
				RAISE EXCEPTION 'platform compliance operator run identity is invalid' USING ERRCODE = '22023';
			END IF;
			IF enable_instagram IS NULL OR enable_threads IS NULL THEN
				RAISE EXCEPTION 'platform compliance feature selection is invalid' USING ERRCODE = '22023';
			END IF;
			PERFORM public.{{scan_function}}();
			IF EXISTS (
				SELECT 1 FROM public.organizations
				WHERE id = requested_organization_id
			) OR EXISTS (
				SELECT 1 FROM public.{{identity_table}}
				WHERE organization_id = requested_organization_id
			) THEN
				RAISE EXCEPTION 'platform compliance organization UUID already exists or was previously used' USING ERRCODE = '23505';
			END IF;
{{footprint_scans}}
			SELECT reseller.id
			INTO STRICT platform_reseller_id
			FROM public.resellers AS reseller
			WHERE reseller.slug = 'platform-direct'
			  AND reseller.status = 'active'
			  AND reseller.deleted_at IS NULL
			FOR SHARE;

			creation_token := pg_catalog.gen_random_uuid();
			created_at := pg_catalog.clock_timestamp();
			organization_name := 'Platform Compliance ' || requested_organization_id::text;
			organization_slug := 'platform-compliance-' || requested_organization_id::text;
			organization_settings := pg_catalog.jsonb_build_object('{{purpose_marker}}', true);
			IF enable_instagram THEN
				organization_settings := organization_settings || pg_catalog.jsonb_build_object('{{instagram_marker}}', true);
			END IF;
			IF enable_threads THEN
				organization_settings := organization_settings || pg_catalog.jsonb_build_object('{{threads_marker}}', true);
			END IF;
			creation_intent := pg_catalog.jsonb_build_object(
				'token', creation_token::text,
				'phase', 'insert',
				'organization_id', requested_organization_id::text,
				'operator_run_id', operator_run_id,
				'instagram', enable_instagram,
				'threads', enable_threads,
				'name', organization_name,
				'slug', organization_slug,
				'reseller_id', platform_reseller_id::text
			);
			PERFORM pg_catalog.set_config('{{creation_intent}}', creation_intent::text, true);
			INSERT INTO public.organizations (
				id, created_at, updated_at, deleted_at,
				reseller_id, name, slug, settings
			) VALUES (
				requested_organization_id, created_at, created_at, NULL,
				platform_reseller_id, organization_name, organization_slug, organization_settings
			);
			creation_intent := NULLIF(current_setting('{{creation_intent}}', true), '')::jsonb;
			IF creation_intent->>'phase' IS DISTINCT FROM 'complete'
				OR creation_intent->>'token' IS DISTINCT FROM creation_token::text THEN
				RAISE EXCEPTION 'platform compliance creation audit did not complete' USING ERRCODE = '23514';
			END IF;
			PERFORM pg_catalog.set_config('{{creation_intent}}', '', true);
			RETURN requested_organization_id;
		EXCEPTION WHEN OTHERS THEN
			PERFORM pg_catalog.set_config('{{creation_intent}}', '', true);
			RAISE;
		END
		$function$;
	`
	return strings.NewReplacer(
		"{{creator_function}}", quoteIdentifier(platformComplianceCreatorFunction),
		"{{scan_function}}", quoteIdentifier(platformComplianceScanGuardFunction),
		"{{identity_table}}", quoteIdentifier(platformComplianceIdentityRegistryTable),
		"{{purpose_marker}}", PlatformCompliancePurposeMarkerKey,
		"{{instagram_marker}}", PlatformComplianceInstagramMarkerKey,
		"{{threads_marker}}", PlatformComplianceThreadsMarkerKey,
		"{{creation_intent}}", platformComplianceCreationIntentSetting,
		"{{footprint_scans}}", scans.String(),
	).Replace(sql)
}

func platformComplianceCreationAuditSQL() string {
	sql := `
		CREATE OR REPLACE FUNCTION public.{{audit_function}}()
		RETURNS trigger
		LANGUAGE plpgsql
		SECURITY DEFINER
		SET search_path = pg_catalog, public
		AS $function$
		DECLARE
			creation_intent jsonb;
			expected_settings jsonb;
			operation text := 'create:purpose';
			changes jsonb;
		BEGIN
			IF NOT (
				COALESCE(NEW.settings, '{}'::jsonb) ? '{{purpose_marker}}'
				OR COALESCE(NEW.settings, '{}'::jsonb) ? '{{instagram_marker}}'
				OR COALESCE(NEW.settings, '{}'::jsonb) ? '{{threads_marker}}'
			) THEN
				RETURN NEW;
			END IF;
			IF session_user::text IS DISTINCT FROM current_user::text THEN
				RAISE EXCEPTION 'platform compliance creation audit requires the direct migration owner session' USING ERRCODE = '42501';
			END IF;
			BEGIN
				creation_intent := NULLIF(current_setting('{{creation_intent}}', true), '')::jsonb;
			EXCEPTION WHEN OTHERS THEN
				RAISE EXCEPTION 'platform compliance creation audit intent is invalid' USING ERRCODE = '42501';
			END;
			IF creation_intent IS NULL
				OR creation_intent->>'phase' IS DISTINCT FROM 'audit'
				OR creation_intent->>'organization_id' IS DISTINCT FROM NEW.id::text
				OR creation_intent->>'reseller_id' IS DISTINCT FROM NEW.reseller_id::text
				OR creation_intent->>'name' IS DISTINCT FROM NEW.name
				OR creation_intent->>'slug' IS DISTINCT FROM NEW.slug
				OR COALESCE(creation_intent->>'operator_run_id', '') !~ '^[A-Za-z0-9][A-Za-z0-9._:@/-]{7,95}$' THEN
				RAISE EXCEPTION 'platform compliance creation audit payload does not match organization' USING ERRCODE = '42501';
			END IF;
			expected_settings := pg_catalog.jsonb_build_object('{{purpose_marker}}', true);
			changes := pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
				'field', '{{purpose_marker}}', 'old_value', NULL, 'new_value', true
			));
			IF (creation_intent->>'instagram')::boolean THEN
				expected_settings := expected_settings || pg_catalog.jsonb_build_object('{{instagram_marker}}', true);
				operation := operation || ',enable:instagram';
				changes := changes || pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
					'field', '{{instagram_marker}}', 'old_value', NULL, 'new_value', true
				));
			END IF;
			IF (creation_intent->>'threads')::boolean THEN
				expected_settings := expected_settings || pg_catalog.jsonb_build_object('{{threads_marker}}', true);
				operation := operation || ',enable:threads';
				changes := changes || pg_catalog.jsonb_build_array(pg_catalog.jsonb_build_object(
					'field', '{{threads_marker}}', 'old_value', NULL, 'new_value', true
				));
			END IF;
			IF COALESCE(NEW.settings, '{}'::jsonb) IS DISTINCT FROM expected_settings THEN
				RAISE EXCEPTION 'platform compliance creation audit settings are not canonical' USING ERRCODE = '23514';
			END IF;
			changes := changes || pg_catalog.jsonb_build_array(
				pg_catalog.jsonb_build_object(
					'field', 'operator_run_id', 'old_value', NULL,
					'new_value', creation_intent->>'operator_run_id'
				),
				pg_catalog.jsonb_build_object(
					'field', 'database_current_user', 'old_value', NULL,
					'new_value', current_user::text
				),
				pg_catalog.jsonb_build_object(
					'field', 'database_session_user', 'old_value', NULL,
					'new_value', session_user::text
				),
				pg_catalog.jsonb_build_object(
					'field', 'control_plane_operation', 'old_value', NULL,
					'new_value', operation
				)
			);
			INSERT INTO public.audit_logs (
				id, organization_id, resource_type, resource_id,
				user_id, user_name, action, changes, created_at
			) VALUES (
				pg_catalog.gen_random_uuid(), NEW.id, 'platform_compliance', NEW.id,
				'00000000-0000-0000-0000-000000000000'::uuid,
				'platform-compliance-bootstrap/' || (creation_intent->>'operator_run_id'),
				'updated', changes, pg_catalog.clock_timestamp()
			);
			creation_intent := pg_catalog.jsonb_set(creation_intent, '{phase}', '"complete"'::jsonb, false);
			PERFORM pg_catalog.set_config('{{creation_intent}}', creation_intent::text, true);
			RETURN NEW;
		END
		$function$;
	`
	return strings.NewReplacer(
		"{{audit_function}}", quoteIdentifier(platformComplianceCreationAuditFunction),
		"{{purpose_marker}}", PlatformCompliancePurposeMarkerKey,
		"{{instagram_marker}}", PlatformComplianceInstagramMarkerKey,
		"{{threads_marker}}", PlatformComplianceThreadsMarkerKey,
		"{{creation_intent}}", platformComplianceCreationIntentSetting,
	).Replace(sql)
}

// CreatePlatformComplianceOrganization invokes the exact migration-owned,
// atomic creator. The SQL function performs a strict insert and emits its audit
// evidence through the verified AFTER INSERT trigger in the same transaction.
func CreatePlatformComplianceOrganization(
	tx *gorm.DB,
	organizationID uuid.UUID,
	operatorRunID string,
	enableInstagram, enableThreads bool,
) error {
	if tx == nil {
		return errors.New("database transaction is required")
	}
	var createdIDText string
	result := tx.Raw(fmt.Sprintf(
		"SELECT public.%s(?, ?, ?, ?)",
		quoteIdentifier(platformComplianceCreatorFunction),
	), organizationID, operatorRunID, enableInstagram, enableThreads).Scan(&createdIDText)
	if result.Error != nil {
		return fmt.Errorf("create atomic platform compliance organization: %w", result.Error)
	}
	createdID, parseErr := uuid.Parse(createdIDText)
	if parseErr != nil || result.RowsAffected != 1 || createdID != organizationID {
		return errors.New("create atomic platform compliance organization: creator returned an invalid result")
	}
	return nil
}
func verifyNoOrdinaryPlatformComplianceMarkers(tx *gorm.DB) error {
	var organizationID string
	result := tx.Raw(`
		SELECT id
		FROM public.organizations
		WHERE (
			NOT COALESCE(
				pg_catalog.jsonb_typeof(settings -> CAST(? AS text)) = 'boolean'
				AND settings -> CAST(? AS text) = 'true'::jsonb,
				false
			)
			AND (
				pg_catalog.jsonb_exists(settings, CAST(? AS text))
				OR pg_catalog.jsonb_exists(settings, CAST(? AS text))
				OR pg_catalog.jsonb_exists(settings, CAST(? AS text))
			)
		) OR (
			COALESCE(
				pg_catalog.jsonb_typeof(settings -> CAST(? AS text)) = 'boolean'
				AND settings -> CAST(? AS text) = 'true'::jsonb,
				false
			)
			AND (
				(pg_catalog.jsonb_exists(settings, CAST(? AS text)) AND settings -> CAST(? AS text) IS DISTINCT FROM 'true'::jsonb)
				OR (pg_catalog.jsonb_exists(settings, CAST(? AS text)) AND settings -> CAST(? AS text) IS DISTINCT FROM 'true'::jsonb)
			)
		)
		ORDER BY id
		LIMIT 1
	`,
		PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey,
		PlatformCompliancePurposeMarkerKey,
		PlatformComplianceInstagramMarkerKey,
		PlatformComplianceThreadsMarkerKey,
		PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey,
		PlatformComplianceInstagramMarkerKey, PlatformComplianceInstagramMarkerKey,
		PlatformComplianceThreadsMarkerKey, PlatformComplianceThreadsMarkerKey,
	).Scan(&organizationID)
	if result.Error != nil {
		return fmt.Errorf("inspect pre-existing platform compliance control markers: %w", result.Error)
	}
	if result.RowsAffected != 0 && organizationID != "" {
		return fmt.Errorf("%w: organization %s has unreviewed platform compliance control markers", ErrPlatformComplianceFootprint, organizationID)
	}
	return nil
}

func verifyExistingPlatformComplianceOrganizations(tx *gorm.DB) error {
	var organizationIDs []uuid.UUID
	if err := tx.Raw(`
		SELECT id
		FROM public.organizations
		WHERE COALESCE(
			pg_catalog.jsonb_typeof(settings -> CAST(? AS text)) = 'boolean'
			AND settings -> CAST(? AS text) = 'true'::jsonb,
			false
		)
	`, PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey).Scan(&organizationIDs).Error; err != nil {
		return fmt.Errorf("list existing platform compliance organizations: %w", err)
	}
	for _, organizationID := range organizationIDs {
		if err := verifyPlatformComplianceOrganizationState(tx, organizationID, nil, true); err != nil {
			return err
		}
	}
	return nil
}

type platformComplianceExpectedCreation struct {
	operatorRunID string
	instagram     bool
	threads       bool
}

func verifyPlatformComplianceOrganizationState(
	tx *gorm.DB,
	organizationID uuid.UUID,
	expected *platformComplianceExpectedCreation,
	lockReseller bool,
) error {
	locking := ""
	if lockReseller {
		locking = " FOR SHARE OF reseller"
	}
	var canonical bool
	if err := tx.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM public.organizations AS organization
			JOIN public.resellers AS reseller ON reseller.id = organization.reseller_id
			WHERE organization.id = ?
			  AND organization.deleted_at IS NULL
			  AND organization.name = 'Platform Compliance ' || organization.id::text
			  AND organization.slug = 'platform-compliance-' || organization.id::text
			  AND reseller.slug = 'platform-direct'
			  AND reseller.status = 'active'
			  AND reseller.deleted_at IS NULL
			  AND EXISTS (
				SELECT 1 FROM public.organization_identity_registry AS identity
				WHERE identity.organization_id = organization.id
			  )
			  AND pg_catalog.jsonb_exists(COALESCE(organization.settings, '{}'::jsonb), CAST(? AS text))
			  AND organization.settings -> CAST(? AS text) = 'true'::jsonb
			  AND NOT EXISTS (
				SELECT 1
				FROM pg_catalog.jsonb_object_keys(COALESCE(organization.settings, '{}'::jsonb)) AS key(name)
				WHERE key.name NOT IN (CAST(? AS text), CAST(? AS text), CAST(? AS text))
			  )
			  AND (
				NOT pg_catalog.jsonb_exists(organization.settings, CAST(? AS text))
				OR organization.settings -> CAST(? AS text) = 'true'::jsonb
			  )
			  AND (
				NOT pg_catalog.jsonb_exists(organization.settings, CAST(? AS text))
				OR organization.settings -> CAST(? AS text) = 'true'::jsonb
			  )
			`+locking+`
		)
	`, organizationID,
		PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey,
		PlatformCompliancePurposeMarkerKey, PlatformComplianceInstagramMarkerKey, PlatformComplianceThreadsMarkerKey,
		PlatformComplianceInstagramMarkerKey, PlatformComplianceInstagramMarkerKey,
		PlatformComplianceThreadsMarkerKey, PlatformComplianceThreadsMarkerKey,
	).Scan(&canonical).Error; err != nil {
		return fmt.Errorf("verify canonical platform compliance organization %s: %w", organizationID, err)
	}
	if !canonical {
		return fmt.Errorf("%w: %s is not a canonical active platform-direct purpose organization", ErrPlatformComplianceFootprint, organizationID)
	}
	table, err := FindPlatformComplianceDisallowedFootprint(tx, organizationID)
	if err != nil {
		return err
	}
	if table != "" {
		return fmt.Errorf("%w: %s has rows in %s", ErrPlatformComplianceFootprint, organizationID, table)
	}
	auditPredicate := strings.ReplaceAll(
		platformComplianceAuditPredicateV7,
		platformComplianceRowToken,
		"pg_catalog.to_jsonb(audit_entry)",
	)
	var creationEvidenceCount int64
	creationQuery := fmt.Sprintf(`
		SELECT pg_catalog.count(*)
		FROM public.audit_logs AS audit_entry
		WHERE audit_entry.organization_id = ?
		  AND (%s)
		  AND EXISTS (
			SELECT 1 FROM pg_catalog.jsonb_array_elements(audit_entry.changes) AS item
			WHERE item->>'field' = 'platform_compliance_tenant'
			  AND item->'old_value' = 'null'::jsonb
			  AND item->'new_value' = 'true'::jsonb
		  )
		  AND EXISTS (
			SELECT 1 FROM pg_catalog.jsonb_array_elements(audit_entry.changes) AS item
			WHERE item->>'field' = 'control_plane_operation'
			  AND item->>'new_value' ~ '^create:purpose(,enable:instagram)?(,enable:threads)?$'
		  )
	`, auditPredicate)
	if err := tx.Raw(creationQuery, organizationID).Scan(&creationEvidenceCount).Error; err != nil {
		return fmt.Errorf("verify atomic creation evidence for %s: %w", organizationID, err)
	}
	if creationEvidenceCount != 1 {
		return fmt.Errorf(
			"%w: %s has %d exact atomic creation evidence rows",
			ErrPlatformComplianceFootprint, organizationID, creationEvidenceCount,
		)
	}
	if expected == nil {
		return nil
	}
	expectedOperation := "create:purpose"
	if expected.instagram {
		expectedOperation += ",enable:instagram"
	}
	if expected.threads {
		expectedOperation += ",enable:threads"
	}
	var exact bool
	exactQuery := fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1
			FROM public.organizations AS organization
			WHERE organization.id = ?
			  AND pg_catalog.jsonb_exists(organization.settings, '%s') = %t
			  AND pg_catalog.jsonb_exists(organization.settings, '%s') = %t
		) AND EXISTS (
			SELECT 1
			FROM public.audit_logs AS audit_entry
			WHERE audit_entry.organization_id = ?
			  AND audit_entry.user_name = 'platform-compliance-bootstrap/' || CAST(? AS text)
			  AND EXISTS (
				SELECT 1 FROM pg_catalog.jsonb_array_elements(audit_entry.changes) AS item
				WHERE item->>'field' = 'control_plane_operation'
				  AND item->>'new_value' = CAST(? AS text)
			  )
			  AND (%s)
		)
	`, PlatformComplianceInstagramMarkerKey, expected.instagram,
		PlatformComplianceThreadsMarkerKey, expected.threads,
		auditPredicate)
	if err := tx.Raw(exactQuery,
		organizationID,
		organizationID, expected.operatorRunID, expectedOperation,
	).Scan(&exact).Error; err != nil {
		return fmt.Errorf("verify exact atomic creation idempotence for %s: %w", organizationID, err)
	}
	if !exact {
		return fmt.Errorf("%w: %s", ErrPlatformComplianceCreationMismatch, organizationID)
	}
	return nil
}

// VerifyPlatformComplianceAtomicCreation revalidates the complete at-rest
// contract before the operator CLI reports a same-run create as idempotent.
func VerifyPlatformComplianceAtomicCreation(
	tx *gorm.DB,
	organizationID uuid.UUID,
	operatorRunID string,
	enableInstagram, enableThreads bool,
	lockReseller bool,
) error {
	if tx == nil {
		return errors.New("database transaction is required")
	}
	return verifyPlatformComplianceOrganizationState(tx, organizationID, &platformComplianceExpectedCreation{
		operatorRunID: operatorRunID,
		instagram:     enableInstagram,
		threads:       enableThreads,
	}, lockReseller)
}

// VerifyPlatformComplianceGuardContract proves the exact migration-installed
// functions, ownership, ACLs, fingerprints, triggers, and runtime-role
// separation. Operator control-plane transactions call this before acquiring
// or mutating a compliance organization; runtime startup reaches the same
// verifier through VerifyTenantRLS.
func VerifyPlatformComplianceGuardContract(db *gorm.DB, runtimeRole string) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	return verifyPlatformComplianceGuards(db, runtimeRole)
}

func verifyPlatformComplianceRelationOwnership(
	db *gorm.DB,
	rules []PlatformComplianceTableRule,
	expectedOwner, runtimeRole string,
) error {
	if err := validateIdentifier(expectedOwner); err != nil {
		return fmt.Errorf("invalid platform compliance relation owner: %w", err)
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	tables := make(map[string]bool, len(rules)+len(RelatedTenantTables)+2)
	tables["organizations"] = true
	tables[platformComplianceIdentityRegistryTable] = true
	for _, rule := range rules {
		tables[rule.Table] = true
	}
	for table := range RelatedTenantTables {
		if _, registered := tables[table]; !registered {
			tables[table] = false
		}
	}
	ordered := make([]string, 0, len(tables))
	for table := range tables {
		ordered = append(ordered, table)
	}
	sort.Strings(ordered)
	for _, table := range ordered {
		var state struct {
			Exists                 bool   `gorm:"column:exists"`
			Owner                  string `gorm:"column:owner_name"`
			RuntimeEffectiveOwner  bool   `gorm:"column:runtime_effective_owner"`
			RuntimeTrigger         bool   `gorm:"column:runtime_trigger"`
			UnexpectedTriggerGrant bool   `gorm:"column:unexpected_trigger_grant"`
		}
		result := db.Raw(`
			SELECT
				true AS exists,
				owner.rolname::text AS owner_name,
				pg_catalog.pg_has_role(runtime.oid, relation.relowner, 'MEMBER') AS runtime_effective_owner,
				pg_catalog.has_table_privilege(runtime.oid, relation.oid, 'TRIGGER') AS runtime_trigger,
				EXISTS (
					SELECT 1
					FROM pg_catalog.aclexplode(COALESCE(
						relation.relacl, pg_catalog.acldefault('r', relation.relowner)
					)) AS privilege
					WHERE privilege.privilege_type = 'TRIGGER'
					  AND privilege.grantee <> relation.relowner
				) AS unexpected_trigger_grant
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			JOIN pg_catalog.pg_roles AS owner ON owner.oid = relation.relowner
			JOIN pg_catalog.pg_roles AS runtime ON runtime.rolname = CAST(? AS text)
			WHERE namespace.nspname = 'public' AND relation.relname = CAST(? AS text)
		`, runtimeRole, table).Scan(&state)
		if result.Error != nil {
			return fmt.Errorf("inspect platform compliance relation owner public.%s: %w", table, result.Error)
		}
		if result.RowsAffected == 0 && !tables[table] {
			continue
		}
		if result.RowsAffected != 1 || !state.Exists {
			return fmt.Errorf("platform compliance relation public.%s is missing", table)
		}
		if state.Owner != expectedOwner || state.RuntimeEffectiveOwner ||
			(tables[table] && (state.RuntimeTrigger || state.UnexpectedTriggerGrant)) {
			return fmt.Errorf(
				"platform compliance relation public.%s owner/trigger ACL contract is invalid (owner=%q expected=%q runtime_effective_owner=%t runtime_trigger=%t unexpected_trigger_grant=%t)",
				table, state.Owner, expectedOwner, state.RuntimeEffectiveOwner,
				state.RuntimeTrigger, state.UnexpectedTriggerGrant,
			)
		}
	}
	return nil
}

func verifyPlatformComplianceIdentityRegistryContract(
	db *gorm.DB,
	expectedOwner, runtimeRole string,
) error {
	var state struct {
		Exists              bool   `gorm:"column:exists"`
		Columns             string `gorm:"column:columns"`
		PrimaryKey          string `gorm:"column:primary_key"`
		TotalChecks         int64  `gorm:"column:total_checks"`
		ExactChecks         int64  `gorm:"column:exact_checks"`
		RowSecurity         bool   `gorm:"column:row_security"`
		ForceRowSecurity    bool   `gorm:"column:force_row_security"`
		Persistence         string `gorm:"column:persistence"`
		RuntimePrivileges   bool   `gorm:"column:runtime_privileges"`
		UnexpectedPrivilege bool   `gorm:"column:unexpected_privilege"`
		UserRules           int64  `gorm:"column:user_rules"`
		TriggerCount        int64  `gorm:"column:trigger_count"`
		ExactTriggers       int64  `gorm:"column:exact_triggers"`
	}
	result := db.Raw(`
		WITH relation AS (
			SELECT relation.*
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
			  AND relation.relname = CAST(? AS text)
			  AND relation.relkind = 'r'
		), owner AS (
			SELECT role.oid FROM pg_catalog.pg_roles AS role
			WHERE role.rolname = CAST(? AS text)
		)
		SELECT
			EXISTS (SELECT 1 FROM relation) AS exists,
			COALESCE((
				SELECT pg_catalog.string_agg(
					attribute.attname || ':' || pg_catalog.format_type(attribute.atttypid, attribute.atttypmod)
					|| ':' || attribute.attnotnull::text,
					',' ORDER BY attribute.attnum
				)
				FROM relation
				JOIN pg_catalog.pg_attribute AS attribute ON attribute.attrelid = relation.oid
				WHERE attribute.attnum > 0 AND NOT attribute.attisdropped
			), '') AS columns,
			COALESCE((
				SELECT pg_catalog.string_agg(attribute.attname, ',' ORDER BY key_column.ordinality)
				FROM relation
				JOIN pg_catalog.pg_constraint AS constraint_state
				  ON constraint_state.conrelid = relation.oid AND constraint_state.contype = 'p'
				JOIN LATERAL pg_catalog.unnest(constraint_state.conkey) WITH ORDINALITY AS key_column(attnum, ordinality) ON true
				JOIN pg_catalog.pg_attribute AS attribute
				  ON attribute.attrelid = relation.oid AND attribute.attnum = key_column.attnum
			), '') AS primary_key,
			COALESCE((
				SELECT pg_catalog.count(*)
				FROM relation
				JOIN pg_catalog.pg_constraint AS constraint_state ON constraint_state.conrelid = relation.oid
				WHERE constraint_state.contype = 'c'
			), 0) AS total_checks,
			COALESCE((
				SELECT pg_catalog.count(*)
				FROM relation
				JOIN pg_catalog.pg_constraint AS constraint_state ON constraint_state.conrelid = relation.oid
				WHERE constraint_state.contype = 'c' AND constraint_state.convalidated
				  AND (
					(constraint_state.conname = 'organization_identity_registry_current_user_format'
					 AND pg_catalog.pg_get_constraintdef(constraint_state.oid, true) =
						'CHECK (database_current_user ~ ''^[A-Za-z_][A-Za-z0-9_]{0,62}$''::text)')
					OR
					(constraint_state.conname = 'organization_identity_registry_session_user_format'
					 AND pg_catalog.pg_get_constraintdef(constraint_state.oid, true) =
						'CHECK (database_session_user ~ ''^[A-Za-z_][A-Za-z0-9_]{0,62}$''::text)')
				  )
			), 0) AS exact_checks,
			COALESCE((SELECT relrowsecurity FROM relation), false) AS row_security,
			COALESCE((SELECT relforcerowsecurity FROM relation), false) AS force_row_security,
			COALESCE((SELECT relpersistence::text FROM relation), '') AS persistence,
			COALESCE((SELECT pg_catalog.has_table_privilege(
				CAST(? AS text), relation.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE'
			) FROM relation), true) AS runtime_privileges,
			EXISTS (
				SELECT 1
				FROM relation
				CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(
					relation.relacl, pg_catalog.acldefault('r', relation.relowner)
				)) AS privilege
				WHERE privilege.grantee <> (SELECT oid FROM owner)
			) AS unexpected_privilege,
			COALESCE((
				SELECT pg_catalog.count(*)
				FROM relation
				JOIN pg_catalog.pg_rewrite AS rule ON rule.ev_class = relation.oid
			), 0) AS user_rules,
			COALESCE((
				SELECT pg_catalog.count(*)
				FROM relation
				JOIN pg_catalog.pg_trigger AS trigger ON trigger.tgrelid = relation.oid
				WHERE NOT trigger.tgisinternal
			), 0) AS trigger_count,
			COALESCE((
				SELECT pg_catalog.count(*)
				FROM relation
				JOIN pg_catalog.pg_trigger AS trigger ON trigger.tgrelid = relation.oid
				JOIN pg_catalog.pg_proc AS procedure ON procedure.oid = trigger.tgfoid
				JOIN pg_catalog.pg_roles AS function_owner ON function_owner.oid = procedure.proowner
				WHERE NOT trigger.tgisinternal
				  AND trigger.tgenabled = 'O'
				  AND trigger.tgnargs = 0
				  AND trigger.tgargs = ''::bytea
				  AND trigger.tgconstraint = 0
				  AND trigger.tgconstrrelid = 0
				  AND NOT trigger.tgdeferrable
				  AND NOT trigger.tginitdeferred
				  AND trigger.tgqual IS NULL
				  AND trigger.tgattr::text = ''
				  AND trigger.tgoldtable IS NULL
				  AND trigger.tgnewtable IS NULL
				  AND function_owner.rolname = CAST(? AS text)
				  AND (
					(trigger.tgname = CAST(? AS text) AND trigger.tgtype = 31
					 AND procedure.proname = CAST(? AS text) AND procedure.pronargs = 0
					 AND procedure.prorettype = 'trigger'::pg_catalog.regtype)
					OR
					(trigger.tgname = CAST(? AS text) AND trigger.tgtype = 34
					 AND procedure.proname = CAST(? AS text) AND procedure.pronargs = 0
					 AND procedure.prorettype = 'trigger'::pg_catalog.regtype)
				  )
			), 0) AS exact_triggers
	`, platformComplianceIdentityRegistryTable, expectedOwner, runtimeRole,
		expectedOwner,
		platformComplianceIdentityWriteTrigger, platformComplianceIdentityGuardFunction,
		platformComplianceIdentityTruncateTrigger, platformComplianceIdentityGuardFunction,
	).Scan(&state)
	if result.Error != nil {
		return fmt.Errorf("inspect organization identity registry contract: %w", result.Error)
	}
	if result.RowsAffected != 1 || !state.Exists ||
		state.Columns != "organization_id:uuid:true,claimed_at:timestamp with time zone:true,database_current_user:text:true,database_session_user:text:true" ||
		state.PrimaryKey != "organization_id" || state.TotalChecks != 2 || state.ExactChecks != 2 ||
		state.RowSecurity || state.ForceRowSecurity || state.Persistence != "p" ||
		state.RuntimePrivileges || state.UnexpectedPrivilege || state.UserRules != 0 ||
		state.TriggerCount != 2 || state.ExactTriggers != 2 {
		return errors.New("organization identity registry contract is invalid")
	}
	return nil
}

type platformComplianceProductTrigger struct {
	name          string
	function      string
	typeBits      int
	updateColumns []string
	functionBody  string
}

var platformComplianceProductTriggers = map[string][]platformComplianceProductTrigger{
	"customer_activity_events": {
		{
			name: "trg_customer_activity_events_append_only", function: "rereply_reject_customer_activity_mutation", typeBits: 27,
			functionBody: `BEGIN
			RAISE EXCEPTION 'customer_activity_events is append-only';
		END;`,
		},
		{
			name: "trg_customer_activity_automation_receipt", function: "rereply_create_automation_event_receipt", typeBits: 5,
			functionBody: `BEGIN
			INSERT INTO automation_event_receipts (
				id,
				organization_id,
				activity_event_id,
				status,
				attempts,
				max_attempts,
				ingested_at,
				available_at,
				created_at,
				updated_at
			) VALUES (
				gen_random_uuid(),
				NEW.organization_id,
				NEW.id,
				'pending',
				0,
				10,
				clock_timestamp(),
				clock_timestamp(),
				clock_timestamp(),
				clock_timestamp()
			)
			ON CONFLICT (organization_id, activity_event_id) DO NOTHING;
			RETURN NEW;
		END;`,
		},
	},
	"automation_policy_versions": {
		{
			name: "trg_automation_policy_versions_append_only", function: "rereply_reject_automation_policy_version_mutation", typeBits: 27,
			functionBody: `BEGIN
			RAISE EXCEPTION 'automation_policy_versions is append-only';
		END;`,
		},
	},
	"automation_policy_activations": {
		{
			name: "trg_automation_policy_activations_guard", function: "rereply_guard_automation_policy_activation", typeBits: 27,
			functionBody: `BEGIN
			IF TG_OP = 'UPDATE'
			   AND OLD.active_until IS NULL
			   AND NEW.active_until IS NOT NULL
			   AND OLD.closed_by_id IS NULL
			   AND NEW.closed_by_id IS NOT NULL
			   AND NEW.id IS NOT DISTINCT FROM OLD.id
			   AND NEW.organization_id IS NOT DISTINCT FROM OLD.organization_id
			   AND NEW.policy_id IS NOT DISTINCT FROM OLD.policy_id
			   AND NEW.policy_version_id IS NOT DISTINCT FROM OLD.policy_version_id
			   AND NEW.policy_version_number IS NOT DISTINCT FROM OLD.policy_version_number
			   AND NEW.active_from IS NOT DISTINCT FROM OLD.active_from
			   AND NEW.created_by_id IS NOT DISTINCT FROM OLD.created_by_id
			   AND NEW.created_at IS NOT DISTINCT FROM OLD.created_at
			THEN
				RETURN NEW;
			END IF;
			RAISE EXCEPTION 'automation_policy_activations is immutable except for its first close transition';
		END;`,
		},
	},
	"messages": {
		{
			name:          "trg_messages_ingestion_order",
			function:      "rereply_set_message_ingestion_order",
			typeBits:      23,
			updateColumns: []string{"inbox_conversation_id"},
			functionBody:  functionBodyFromCreateSQL(messageIngestionOrderFunctionSQL),
		},
		{
			name:         "trg_messages_cleanup_read_cursors",
			function:     "rereply_cleanup_deleted_message_read_cursors",
			typeBits:     11,
			functionBody: functionBodyFromCreateSQL(deletedMessageReadCursorCleanupFunctionSQL),
		},
	},
	"conversation_reads": {
		{
			name:          "trg_conversation_reads_ingestion_order",
			function:      "rereply_set_conversation_read_ingestion_order",
			typeBits:      23,
			updateColumns: []string{"last_read_message_id", "last_read_at"},
			functionBody:  functionBodyFromCreateSQL(conversationReadIngestionOrderFunctionSQL),
		},
	},
}

func normalizePlatformComplianceFunctionBody(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func verifyPlatformComplianceExactTableTriggerSet(
	db *gorm.DB,
	table, expectedOwner string,
) error {
	expectedProducts := platformComplianceProductTriggers[table]
	var state struct {
		TriggerCount int64 `gorm:"column:trigger_count"`
		RuleCount    int64 `gorm:"column:rule_count"`
	}
	if err := db.Raw(`
		SELECT
			(SELECT pg_catalog.count(*)
			 FROM pg_catalog.pg_trigger AS trigger
			 WHERE trigger.tgrelid = relation.oid AND NOT trigger.tgisinternal) AS trigger_count,
			(SELECT pg_catalog.count(*)
			 FROM pg_catalog.pg_rewrite AS rule
			 WHERE rule.ev_class = relation.oid) AS rule_count
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public' AND relation.relname = CAST(? AS text)
		  AND relation.relkind = 'r'
	`, table).Scan(&state).Error; err != nil {
		return fmt.Errorf("inspect exact trigger/rule set on public.%s: %w", table, err)
	}
	if state.TriggerCount != int64(1+len(expectedProducts)) || state.RuleCount != 0 {
		return fmt.Errorf("platform compliance trigger/rule set is not exact on public.%s", table)
	}
	for _, expected := range expectedProducts {
		expectedUpdateColumns := append([]string(nil), expected.updateColumns...)
		sort.Strings(expectedUpdateColumns)
		var actual struct {
			Source               string `gorm:"column:source"`
			Owner                string `gorm:"column:owner_name"`
			Language             string `gorm:"column:language"`
			SecurityDefiner      bool   `gorm:"column:security_definer"`
			UpdateColumns        string `gorm:"column:update_columns"`
			UpdateAttributeCount int    `gorm:"column:update_attribute_count"`
			Valid                bool   `gorm:"column:valid"`
		}
		result := db.Raw(`
			SELECT
				procedure.prosrc AS source,
				owner.rolname::text AS owner_name,
				language.lanname AS language,
				procedure.prosecdef AS security_definer,
				COALESCE(pg_catalog.cardinality(trigger.tgattr::smallint[]), 0) AS update_attribute_count,
				COALESCE((
					SELECT pg_catalog.string_agg(attribute.attname, ',' ORDER BY attribute.attname)
					FROM pg_catalog.unnest(trigger.tgattr::smallint[])
						WITH ORDINALITY AS trigger_column(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute
					  ON attribute.attrelid = relation.oid
					 AND attribute.attnum = trigger_column.attnum
					 AND NOT attribute.attisdropped
				), '') AS update_columns,
				(
					trigger.tgenabled = 'O'
					AND trigger.tgtype = CAST(? AS smallint)
					AND trigger.tgnargs = 0
					AND trigger.tgargs = ''::bytea
					AND trigger.tgconstraint = 0
					AND trigger.tgconstrrelid = 0
					AND NOT trigger.tgdeferrable
					AND NOT trigger.tginitdeferred
					AND trigger.tgqual IS NULL
					AND trigger.tgoldtable IS NULL
					AND trigger.tgnewtable IS NULL
					AND procedure.pronargs = 0
					AND procedure.prorettype = 'trigger'::pg_catalog.regtype
				) AS valid
			FROM pg_catalog.pg_trigger AS trigger
			JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			JOIN pg_catalog.pg_proc AS procedure ON procedure.oid = trigger.tgfoid
			JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
			JOIN pg_catalog.pg_language AS language ON language.oid = procedure.prolang
			WHERE namespace.nspname = 'public'
			  AND relation.relname = CAST(? AS text)
			  AND trigger.tgname = CAST(? AS text)
			  AND NOT trigger.tgisinternal
			  AND procedure.pronamespace = namespace.oid
			  AND procedure.proname = CAST(? AS text)
		`, expected.typeBits, table, expected.name, expected.function).Scan(&actual)
		if result.Error != nil {
			return fmt.Errorf("inspect product trigger %s on public.%s: %w", expected.name, table, result.Error)
		}
		if result.RowsAffected != 1 || !actual.Valid || actual.Owner != expectedOwner ||
			actual.Language != "plpgsql" || actual.SecurityDefiner ||
			actual.UpdateAttributeCount != len(expectedUpdateColumns) ||
			actual.UpdateColumns != strings.Join(expectedUpdateColumns, ",") ||
			normalizePlatformComplianceFunctionBody(actual.Source) != normalizePlatformComplianceFunctionBody(expected.functionBody) {
			return fmt.Errorf("product trigger %q on public.%s does not match the platform compliance contract", expected.name, table)
		}
	}
	return nil
}

func verifyPlatformComplianceAuditIndexes(db *gorm.DB) error {
	expected := []struct {
		name      string
		keys      string
		predicate string
	}{
		{
			name:      platformComplianceCreationEvidenceIndex,
			keys:      "organization_id",
			predicate: `(((resource_type)::text = 'platform_compliance'::text) AND (changes @> '[{"field": "platform_compliance_tenant", "new_value": true, "old_value": null}]'::jsonb))`,
		},
		{
			name:      platformComplianceAuditRunIndex,
			keys:      "organization_id,user_name",
			predicate: `((resource_type)::text = 'platform_compliance'::text)`,
		},
	}
	for _, expectedIndex := range expected {
		var state struct {
			Keys      string `gorm:"column:keys"`
			Predicate string `gorm:"column:predicate"`
			Valid     bool   `gorm:"column:valid"`
		}
		result := db.Raw(`
			SELECT
				COALESCE((
					SELECT pg_catalog.string_agg(attribute.attname, ',' ORDER BY key.ordinality)
					FROM pg_catalog.unnest(index_state.indkey::smallint[]) WITH ORDINALITY AS key(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute
					  ON attribute.attrelid = table_relation.oid AND attribute.attnum = key.attnum
				), '') AS keys,
				pg_catalog.pg_get_expr(index_state.indpred, index_state.indrelid) AS predicate,
				(
					index_relation.relkind = 'i'
					AND index_relation.relpersistence = 'p'
					AND access_method.amname = 'btree'
					AND index_state.indisunique
					AND index_state.indisvalid
					AND index_state.indisready
					AND index_state.indislive
					AND index_state.indimmediate
					AND NOT index_state.indisprimary
					AND NOT index_state.indisexclusion
					-- indnullsnotdistinct is a PostgreSQL 15+ catalog column. Reading
					-- the catalog row through JSON keeps PostgreSQL 14 supported (a
					-- missing key is NULL/default false) while still rejecting a
					-- PG15+ UNIQUE NULLS NOT DISTINCT index.
					AND NOT COALESCE(
						(pg_catalog.to_jsonb(index_state)->>'indnullsnotdistinct')::boolean,
						false
					)
					AND index_state.indnkeyatts = index_state.indnatts
					AND index_state.indexprs IS NULL
					AND index_state.indpred IS NOT NULL
				) AS valid
			FROM pg_catalog.pg_class AS index_relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = index_relation.relnamespace
			JOIN pg_catalog.pg_index AS index_state ON index_state.indexrelid = index_relation.oid
			JOIN pg_catalog.pg_class AS table_relation ON table_relation.oid = index_state.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace ON table_namespace.oid = table_relation.relnamespace
			JOIN pg_catalog.pg_am AS access_method ON access_method.oid = index_relation.relam
			WHERE namespace.nspname = 'public'
			  AND index_relation.relname = CAST(? AS text)
			  AND table_namespace.nspname = 'public'
			  AND table_relation.relname = 'audit_logs'
		`, expectedIndex.name).Scan(&state)
		if result.Error != nil {
			return fmt.Errorf("inspect platform compliance audit index %s: %w", expectedIndex.name, result.Error)
		}
		if result.RowsAffected != 1 || !state.Valid || state.Keys != expectedIndex.keys ||
			strings.TrimSpace(state.Predicate) != expectedIndex.predicate {
			return fmt.Errorf("platform compliance audit index %q does not match the exact contract", expectedIndex.name)
		}
	}
	return nil
}

func verifyPlatformComplianceGuards(db *gorm.DB, runtimeRole string) error {
	rules, err := PlatformComplianceTableRules()
	if err != nil {
		return err
	}
	scanSQL := platformComplianceScanGuardSQL(rules)
	writeSQL := platformComplianceWriteGuardSQL(rules)
	identitySQL := platformComplianceIdentityRegistryGuardSQL()
	classificationSQL := platformComplianceClassificationGuardSQL(rules)
	creationAuditSQL := platformComplianceCreationAuditSQL()
	creatorSQL := platformComplianceCreatorSQL(rules)
	fingerprint := platformComplianceContractFingerprint(
		rules, scanSQL, writeSQL, identitySQL, classificationSQL, creationAuditSQL, creatorSQL,
	)
	contractSQL := platformComplianceContractSQL(fingerprint)
	expectedFunctions := map[string]struct {
		body            string
		securityDefiner bool
		language        string
		volatility      string
	}{
		platformComplianceScanGuardFunction: {
			body: functionBodyFromCreateSQL(scanSQL), securityDefiner: true, language: "plpgsql", volatility: "v",
		},
		platformComplianceWriteGuardFunction: {
			body: functionBodyFromCreateSQL(writeSQL), securityDefiner: true, language: "plpgsql", volatility: "v",
		},
		platformComplianceIdentityGuardFunction: {
			body: functionBodyFromCreateSQL(identitySQL), securityDefiner: true, language: "plpgsql", volatility: "v",
		},
		platformComplianceClassifyFunction: {
			body: functionBodyFromCreateSQL(classificationSQL), securityDefiner: true, language: "plpgsql", volatility: "v",
		},
		platformComplianceCreationAuditFunction: {
			body: functionBodyFromCreateSQL(creationAuditSQL), securityDefiner: true, language: "plpgsql", volatility: "v",
		},
		platformComplianceContractFunction: {
			body: functionBodyFromCreateSQL(contractSQL), securityDefiner: false, language: "sql", volatility: "i",
		},
	}
	var functionState struct {
		Count          int64  `gorm:"column:function_count"`
		OwnerCount     int64  `gorm:"column:owner_count"`
		OwnerName      string `gorm:"column:owner_name"`
		UnpinnedSearch bool   `gorm:"column:unpinned_search"`
	}
	if err := db.Raw(`
		SELECT
			COUNT(*) AS function_count,
			COUNT(DISTINCT procedure.proowner) AS owner_count,
			COALESCE(pg_catalog.min(owner.rolname::text), '') AS owner_name,
			COALESCE(pg_catalog.bool_or(NOT COALESCE(procedure.proconfig, '{}'::text[]) @> ARRAY['search_path=pg_catalog, public']), false) AS unpinned_search
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
		WHERE namespace.nspname = 'public'
		  AND procedure.proname IN (?, ?, ?, ?, ?, ?)
		  AND procedure.pronargs = 0
	`, platformComplianceScanGuardFunction, platformComplianceWriteGuardFunction,
		platformComplianceIdentityGuardFunction, platformComplianceClassifyFunction, platformComplianceCreationAuditFunction,
		platformComplianceContractFunction).Scan(&functionState).Error; err != nil {
		return fmt.Errorf("inspect platform compliance functions: %w", err)
	}
	if functionState.Count != int64(len(expectedFunctions)) || functionState.OwnerCount != 1 ||
		functionState.OwnerName == "" || functionState.OwnerName == runtimeRole ||
		functionState.UnpinnedSearch {
		return errors.New("platform compliance guard functions are incomplete")
	}
	if err := verifyPlatformComplianceRelationOwnership(
		db, rules, functionState.OwnerName, runtimeRole,
	); err != nil {
		return err
	}
	if err := verifyPlatformComplianceIdentityRegistryContract(
		db, functionState.OwnerName, runtimeRole,
	); err != nil {
		return err
	}
	for name, expected := range expectedFunctions {
		var actual struct {
			Source                string `gorm:"column:source"`
			SecurityDefiner       bool   `gorm:"column:security_definer"`
			Language              string `gorm:"column:language"`
			Volatility            string `gorm:"column:volatility"`
			OwnerExecute          bool   `gorm:"column:owner_execute"`
			RuntimeExecute        bool   `gorm:"column:runtime_execute"`
			UnexpectedExplicit    bool   `gorm:"column:unexpected_explicit"`
			UnexpectedInheritance bool   `gorm:"column:unexpected_inheritance"`
		}
		result := db.Raw(`
			WITH RECURSIVE role_membership(member, inherited_role) AS (
				SELECT membership.member, membership.roleid
				FROM pg_catalog.pg_auth_members AS membership
				UNION
				SELECT role_membership.member, membership.roleid
				FROM role_membership
				JOIN pg_catalog.pg_auth_members AS membership
				  ON membership.member = role_membership.inherited_role
			)
			SELECT
				procedure.prosrc AS source,
				procedure.prosecdef AS security_definer,
				language.lanname AS language,
				procedure.provolatile::text AS volatility,
				EXISTS (
					SELECT 1
					FROM pg_catalog.aclexplode(COALESCE(
						procedure.proacl,
						pg_catalog.acldefault('f', procedure.proowner)
					)) AS privilege
					WHERE privilege.grantee = procedure.proowner
					  AND privilege.privilege_type = 'EXECUTE'
				) AS owner_execute,
				pg_catalog.has_function_privilege(CAST(? AS text), procedure.oid, 'EXECUTE') AS runtime_execute,
				EXISTS (
					SELECT 1
					FROM pg_catalog.aclexplode(COALESCE(
						procedure.proacl,
						pg_catalog.acldefault('f', procedure.proowner)
					)) AS privilege
					LEFT JOIN pg_catalog.pg_roles AS grantee ON grantee.oid = privilege.grantee
					WHERE privilege.privilege_type = 'EXECUTE'
					  AND privilege.grantee <> procedure.proowner
					  AND NOT (
						CAST(? AS boolean)
						AND COALESCE(grantee.rolname = CAST(? AS text), false)
					  )
				) AS unexpected_explicit,
				EXISTS (
					SELECT 1
					FROM role_membership
					LEFT JOIN pg_catalog.pg_roles AS inherited
					  ON inherited.oid = role_membership.inherited_role
					LEFT JOIN pg_catalog.pg_roles AS member
					  ON member.oid = role_membership.member
					WHERE (
						role_membership.inherited_role = procedure.proowner
						OR (CAST(? AS boolean) AND inherited.rolname = CAST(? AS text))
					)
					  AND role_membership.member <> procedure.proowner
					  AND NOT (CAST(? AS boolean) AND member.rolname = CAST(? AS text))
				) AS unexpected_inheritance
			FROM pg_catalog.pg_proc AS procedure
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
			JOIN pg_catalog.pg_language AS language ON language.oid = procedure.prolang
			WHERE namespace.nspname = 'public' AND procedure.proname = ? AND procedure.pronargs = 0
		`, runtimeRole,
			name == platformComplianceContractFunction, runtimeRole,
			name == platformComplianceContractFunction, runtimeRole,
			name == platformComplianceContractFunction, runtimeRole,
			name).Scan(&actual)
		if result.Error != nil {
			return fmt.Errorf("inspect platform compliance function %s: %w", name, result.Error)
		}
		if result.RowsAffected != 1 || strings.TrimSpace(actual.Source) != expected.body ||
			actual.SecurityDefiner != expected.securityDefiner || actual.Language != expected.language ||
			actual.Volatility != expected.volatility || !actual.OwnerExecute ||
			actual.UnexpectedExplicit || actual.UnexpectedInheritance ||
			actual.RuntimeExecute != (name == platformComplianceContractFunction) {
			return fmt.Errorf("platform compliance function %q does not match contract %s", name, fingerprint)
		}
	}
	var creatorState struct {
		Source                string `gorm:"column:source"`
		SecurityDefiner       bool   `gorm:"column:security_definer"`
		Language              string `gorm:"column:language"`
		Volatility            string `gorm:"column:volatility"`
		Owner                 string `gorm:"column:owner_name"`
		OwnerExecute          bool   `gorm:"column:owner_execute"`
		RuntimeExecute        bool   `gorm:"column:runtime_execute"`
		UnexpectedExplicit    bool   `gorm:"column:unexpected_explicit"`
		UnexpectedInheritance bool   `gorm:"column:unexpected_inheritance"`
		UnpinnedSearch        bool   `gorm:"column:unpinned_search"`
	}
	creatorResult := db.Raw(`
		WITH RECURSIVE role_membership(member, inherited_role) AS (
			SELECT membership.member, membership.roleid
			FROM pg_catalog.pg_auth_members AS membership
			UNION
			SELECT role_membership.member, membership.roleid
			FROM role_membership
			JOIN pg_catalog.pg_auth_members AS membership
			  ON membership.member = role_membership.inherited_role
		)
		SELECT
			procedure.prosrc AS source,
			procedure.prosecdef AS security_definer,
			language.lanname AS language,
			procedure.provolatile::text AS volatility,
			owner.rolname::text AS owner_name,
			pg_catalog.has_function_privilege(owner.oid, procedure.oid, 'EXECUTE') AS owner_execute,
			pg_catalog.has_function_privilege(CAST(? AS text), procedure.oid, 'EXECUTE') AS runtime_execute,
			EXISTS (
				SELECT 1
				FROM pg_catalog.aclexplode(COALESCE(
					procedure.proacl, pg_catalog.acldefault('f', procedure.proowner)
				)) AS privilege
				WHERE privilege.privilege_type = 'EXECUTE'
				  AND privilege.grantee <> procedure.proowner
			) AS unexpected_explicit,
			EXISTS (
				SELECT 1 FROM role_membership
				WHERE role_membership.inherited_role = procedure.proowner
				  AND role_membership.member <> procedure.proowner
			) AS unexpected_inheritance,
			NOT COALESCE(procedure.proconfig, '{}'::text[])
				@> ARRAY['search_path=pg_catalog, public'] AS unpinned_search
		FROM pg_catalog.pg_proc AS procedure
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
		JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
		JOIN pg_catalog.pg_language AS language ON language.oid = procedure.prolang
		WHERE namespace.nspname = 'public'
		  AND procedure.proname = ?
		  AND procedure.oid = pg_catalog.to_regprocedure(CAST(? AS text))
	`, runtimeRole, platformComplianceCreatorFunction,
		"public."+platformComplianceCreatorFunction+"(uuid,text,boolean,boolean)").Scan(&creatorState)
	if creatorResult.Error != nil {
		return fmt.Errorf("inspect atomic platform compliance creator: %w", creatorResult.Error)
	}
	if creatorResult.RowsAffected != 1 ||
		strings.TrimSpace(creatorState.Source) != functionBodyFromCreateSQL(creatorSQL) ||
		!creatorState.SecurityDefiner || creatorState.Language != "plpgsql" ||
		creatorState.Volatility != "v" || creatorState.Owner != functionState.OwnerName ||
		!creatorState.OwnerExecute || creatorState.RuntimeExecute ||
		creatorState.UnexpectedExplicit || creatorState.UnexpectedInheritance ||
		creatorState.UnpinnedSearch {
		return fmt.Errorf(
			"atomic platform compliance creator does not match contract %s (rows=%d source=%t security_definer=%t language=%q volatility=%q owner=%q expected_owner=%q owner_execute=%t runtime_execute=%t unexpected_explicit=%t unexpected_inheritance=%t unpinned_search=%t)",
			fingerprint, creatorResult.RowsAffected,
			strings.TrimSpace(creatorState.Source) == functionBodyFromCreateSQL(creatorSQL),
			creatorState.SecurityDefiner, creatorState.Language, creatorState.Volatility,
			creatorState.Owner, functionState.OwnerName, creatorState.OwnerExecute,
			creatorState.RuntimeExecute, creatorState.UnexpectedExplicit,
			creatorState.UnexpectedInheritance, creatorState.UnpinnedSearch,
		)
	}
	var installedFingerprint string
	if err := db.Raw(fmt.Sprintf(
		"SELECT public.%s()", quoteIdentifier(platformComplianceContractFunction),
	)).Scan(&installedFingerprint).Error; err != nil {
		return fmt.Errorf("read platform compliance contract fingerprint: %w", err)
	}
	if installedFingerprint != fingerprint {
		return fmt.Errorf("platform compliance contract fingerprint mismatch: got %q", installedFingerprint)
	}

	for _, rule := range rules {
		var valid bool
		if err := db.Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				JOIN pg_catalog.pg_proc AS procedure ON procedure.oid = trigger.tgfoid
				JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
				JOIN pg_catalog.pg_attribute AS organization_attribute
				  ON organization_attribute.attrelid = relation.oid
				 AND organization_attribute.attname = 'organization_id'
				 AND NOT organization_attribute.attisdropped
				WHERE namespace.nspname = 'public' AND relation.relname = ?
				  AND trigger.tgname = ? AND NOT trigger.tgisinternal AND trigger.tgenabled = 'O'
				  AND trigger.tgtype = CASE WHEN CAST(? AS boolean) THEN 31 ELSE 23 END
				  AND trigger.tgnargs = 0 AND trigger.tgargs = ''::bytea
				  AND trigger.tgconstraint = 0 AND trigger.tgconstrrelid = 0
				  AND NOT trigger.tgdeferrable AND NOT trigger.tginitdeferred
				  AND trigger.tgqual IS NULL
				  AND trigger.tgoldtable IS NULL AND trigger.tgnewtable IS NULL
				  AND relation.relkind = 'r'
				  AND NOT EXISTS (
					SELECT 1 FROM pg_catalog.pg_inherits AS inheritance
					WHERE inheritance.inhparent = relation.oid OR inheritance.inhrelid = relation.oid
				  )
				  AND procedure.pronamespace = namespace.oid
				  AND procedure.proname = ? AND procedure.pronargs = 0
				  AND procedure.prorettype = 'trigger'::pg_catalog.regtype AND owner.rolname = ?
				  AND (
					(? AND trigger.tgattr::text = '')
					OR (NOT ? AND trigger.tgattr::text = organization_attribute.attnum::text)
				  )
				  AND (
					NOT ?
					OR EXISTS (
						SELECT 1 FROM pg_catalog.pg_policy AS policy
						WHERE policy.polrelid = relation.oid
						  AND policy.polname = 'rereply_migration_access'
						  AND procedure.proowner = ANY(policy.polroles)
						  AND policy.polpermissive AND policy.polcmd = '*'
						  AND pg_catalog.pg_get_expr(policy.polqual, policy.polrelid) = 'true'
					)
				  )
			)
		`, rule.Table, platformComplianceWriteTrigger, rule.AllowedRowPredicate != "",
			platformComplianceWriteGuardFunction,
			functionState.OwnerName, rule.AllowedRowPredicate != "", rule.AllowedRowPredicate != "",
			rule.ForceTenantRLS).Scan(&valid).Error; err != nil {
			return fmt.Errorf("inspect platform compliance trigger on %s: %w", rule.Table, err)
		}
		if !valid {
			return fmt.Errorf("platform compliance write guard is incomplete on %q", rule.Table)
		}
		if err := verifyPlatformComplianceExactTableTriggerSet(
			db, rule.Table, functionState.OwnerName,
		); err != nil {
			return err
		}
	}
	var classifyValid bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_trigger AS trigger
			JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			JOIN pg_catalog.pg_proc AS procedure ON procedure.oid = trigger.tgfoid
			JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
			WHERE namespace.nspname = 'public' AND relation.relname = 'organizations'
			  AND trigger.tgname = ? AND NOT trigger.tgisinternal AND trigger.tgenabled = 'O'
			  AND trigger.tgtype = 31 AND trigger.tgnargs = 0 AND trigger.tgargs = ''::bytea
			  AND trigger.tgconstraint = 0 AND trigger.tgconstrrelid = 0
			  AND NOT trigger.tgdeferrable AND NOT trigger.tginitdeferred
			  AND trigger.tgqual IS NULL
			  AND trigger.tgoldtable IS NULL AND trigger.tgnewtable IS NULL
			  AND relation.relkind = 'r'
			  AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_inherits AS inheritance
				WHERE inheritance.inhparent = relation.oid OR inheritance.inhrelid = relation.oid
			  )
			  AND trigger.tgattr::text = ''
			  AND procedure.pronamespace = namespace.oid
			  AND procedure.proname = ? AND procedure.pronargs = 0
			  AND procedure.prorettype = 'trigger'::pg_catalog.regtype AND owner.rolname = ?
		)
	`, platformComplianceClassifyTrigger, platformComplianceClassifyFunction,
		functionState.OwnerName).Scan(&classifyValid).Error; err != nil {
		return fmt.Errorf("inspect platform compliance classification trigger: %w", err)
	}
	if !classifyValid {
		return errors.New("platform compliance classification guard is incomplete")
	}
	for _, expected := range []struct {
		trigger  string
		function string
		typeBits int
	}{
		{platformComplianceCreationAuditTrigger, platformComplianceCreationAuditFunction, 5},
		{platformComplianceTruncateTrigger, platformComplianceClassifyFunction, 34},
	} {
		var valid bool
		if err := db.Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				JOIN pg_catalog.pg_proc AS procedure ON procedure.oid = trigger.tgfoid
				JOIN pg_catalog.pg_roles AS owner ON owner.oid = procedure.proowner
				WHERE namespace.nspname = 'public' AND relation.relname = 'organizations'
				  AND trigger.tgname = ? AND NOT trigger.tgisinternal AND trigger.tgenabled = 'O'
				  AND trigger.tgtype = CAST(? AS smallint)
				  AND trigger.tgnargs = 0 AND trigger.tgargs = ''::bytea
				  AND trigger.tgconstraint = 0 AND trigger.tgconstrrelid = 0
				  AND NOT trigger.tgdeferrable AND NOT trigger.tginitdeferred
				  AND trigger.tgqual IS NULL
				  AND trigger.tgattr::text = ''
				  AND trigger.tgoldtable IS NULL AND trigger.tgnewtable IS NULL
				  AND procedure.pronamespace = namespace.oid
				  AND procedure.proname = ? AND procedure.pronargs = 0
				  AND procedure.prorettype = 'trigger'::pg_catalog.regtype
				  AND owner.rolname = ?
			)
		`, expected.trigger, expected.typeBits, expected.function,
			functionState.OwnerName).Scan(&valid).Error; err != nil {
			return fmt.Errorf("inspect organization compliance trigger %s: %w", expected.trigger, err)
		}
		if !valid {
			return fmt.Errorf("organization compliance trigger %q is incomplete", expected.trigger)
		}
	}
	var organizationTriggerState struct {
		TriggerCount int64 `gorm:"column:trigger_count"`
		RuleCount    int64 `gorm:"column:rule_count"`
	}
	if err := db.Raw(`
		SELECT
			(SELECT pg_catalog.count(*) FROM pg_catalog.pg_trigger AS trigger
			 WHERE trigger.tgrelid = relation.oid AND NOT trigger.tgisinternal) AS trigger_count,
			(SELECT pg_catalog.count(*) FROM pg_catalog.pg_rewrite AS rule
			 WHERE rule.ev_class = relation.oid) AS rule_count
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public' AND relation.relname = 'organizations'
		  AND relation.relkind = 'r'
	`).Scan(&organizationTriggerState).Error; err != nil {
		return fmt.Errorf("count organization compliance triggers: %w", err)
	}
	if organizationTriggerState.TriggerCount != 3 || organizationTriggerState.RuleCount != 0 {
		return errors.New("organization compliance trigger/rule set is not exact")
	}
	if err := verifyPlatformComplianceAuditIndexes(db); err != nil {
		return err
	}
	return nil
}

func functionBodyFromCreateSQL(statement string) string {
	parts := strings.Split(statement, "$function$")
	if len(parts) < 3 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func removePlatformComplianceGuards(tx *gorm.DB) error {
	if tx == nil {
		return nil
	}
	// This lock is deliberately the first relation-visible statement in the
	// root READ COMMITTED removal transaction. It serializes organization
	// INSERT/UPDATE/DELETE and guarantees the purpose check below receives a
	// fresh post-wait snapshot.
	if err := tx.Exec("LOCK TABLE public.organizations IN ACCESS EXCLUSIVE MODE").Error; err != nil {
		return fmt.Errorf("lock platform compliance rollback boundary: %w", err)
	}
	organizations, err := inspectPublicTable(tx, "organizations")
	if err != nil {
		return err
	}
	if !organizations.Exists {
		return errors.New("platform compliance rollback requires public.organizations")
	}
	if organizations.Exists {
		var purposeExists bool
		if err := tx.Raw(`
			SELECT EXISTS (
				SELECT 1 FROM public.organizations
				WHERE COALESCE(
					pg_catalog.jsonb_typeof(settings -> CAST(? AS text)) = 'boolean'
					AND settings -> CAST(? AS text) = 'true'::jsonb,
					false
				)
			)
		`, PlatformCompliancePurposeMarkerKey, PlatformCompliancePurposeMarkerKey).Scan(&purposeExists).Error; err != nil {
			return fmt.Errorf("check platform compliance rollback eligibility: %w", err)
		}
		if purposeExists {
			return ErrPlatformComplianceRollbackBlocked
		}
	}
	// Compliance guards are additive schema invariants, not RLS policies. Keep
	// them installed through a tenant-policy rollback so ordinary organizations
	// still cannot preseed markers and a later classifier cannot silently reopen
	// purpose writes. ApplyTenantRLS will verify/reconcile the same contract.
	return nil
}
