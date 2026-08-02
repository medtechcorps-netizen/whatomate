package calling

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHangupOutgoingCallRejectsCrossTenantSession(t *testing.T) {
	t.Parallel()

	ownerOrganizationID := uuid.New()
	requestingOrganizationID := uuid.New()
	callLogID := uuid.New()
	session := &CallSession{
		ID:             "synthetic-call-id",
		OrganizationID: ownerOrganizationID,
		CallLogID:      callLogID,
		Direction:      models.CallDirectionOutgoing,
	}
	manager := &Manager{
		sessions: map[string]*CallSession{session.ID: session},
	}

	err := manager.HangupOutgoingCall(requestingOrganizationID, callLogID, uuid.New())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
	assert.Same(t, session, manager.GetSession(session.ID), "rejected hangup must not remove the owner's session")
}

func TestGetSessionByOrganizationAndCallLogIDRequiresBothIdentifiers(t *testing.T) {
	t.Parallel()

	organizationID := uuid.New()
	callLogID := uuid.New()
	session := &CallSession{
		ID:             "synthetic-call-id",
		OrganizationID: organizationID,
		CallLogID:      callLogID,
	}
	manager := &Manager{
		sessions: map[string]*CallSession{session.ID: session},
	}

	assert.Same(t, session, manager.getSessionByOrganizationAndCallLogID(organizationID, callLogID))
	assert.Nil(t, manager.getSessionByOrganizationAndCallLogID(uuid.New(), callLogID))
	assert.Nil(t, manager.getSessionByOrganizationAndCallLogID(organizationID, uuid.New()))
}
