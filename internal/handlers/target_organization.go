package handlers

import (
	"errors"

	"github.com/google/uuid"
	"github.com/zerodha/fastglue"
)

const targetOrganizationIDPathKey = "target_organization_id"

// targetOrganizationID reads the explicit cross-workspace route target. It
// deliberately does not fall back to the authenticated organization context:
// the login workspace, selected workspace, and URL target may all be different.
func targetOrganizationID(r *fastglue.Request) (uuid.UUID, error) {
	switch value := r.RequestCtx.UserValue(targetOrganizationIDPathKey).(type) {
	case uuid.UUID:
		if value != uuid.Nil {
			return value, nil
		}
	case string:
		if parsed, err := uuid.Parse(value); err == nil && parsed != uuid.Nil {
			return parsed, nil
		}
	}
	return uuid.Nil, errors.New("invalid target organization ID")
}
