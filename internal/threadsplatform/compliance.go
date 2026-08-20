package threadsplatform

import (
	"context"
	"errors"
	"fmt"

	"github.com/shridarpatil/whatomate/internal/database"
	"gorm.io/gorm"
)

const ComplianceOrganizationMarkerKey = "threads_managed_compliance_tenant"

var ErrComplianceOrganizationInvalid = errors.New("managed Threads compliance organization is not an active platform-owned compliance tenant")

// ValidateComplianceOrganization verifies the deployment UUID against durable
// control-plane ownership. Merely naming a clinic UUID in configuration is not
// sufficient: the tenant must belong to the first-party platform reseller and
// carry the explicit compliance marker.
func (r *Runtime) ValidateComplianceOrganization(
	ctx context.Context,
	db *gorm.DB,
) (*ValidatedRuntime, error) {
	if r == nil || !r.enabled {
		return nil, ErrManagedDisabled
	}
	if db == nil {
		return nil, ErrComplianceOrganizationInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var valid bool
	if err := db.WithContext(ctx).Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM organizations AS organization
			JOIN resellers AS reseller ON reseller.id = organization.reseller_id
			WHERE organization.id = ?
			  AND organization.deleted_at IS NULL
			  AND reseller.deleted_at IS NULL
			  AND reseller.status = 'active'
			  AND reseller.slug = ?
			  AND organization.settings @> '{"threads_managed_compliance_tenant":true}'::jsonb
		)
	`, r.complianceOrganizationID, database.PlatformResellerSlug).Scan(&valid).Error; err != nil {
		return nil, fmt.Errorf("validate managed Threads compliance organization: %w", err)
	}
	if !valid {
		return nil, ErrComplianceOrganizationInvalid
	}
	return &ValidatedRuntime{runtime: r}, nil
}
