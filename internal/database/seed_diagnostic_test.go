package database

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

func TestSeedDiagnosticPermissionProjectionIsClosedAndBounded(t *testing.T) {
	pairs, err := seedDiagnosticExpectedPermissions(models.DefaultPermissions())
	require.NoError(t, err)
	require.Len(t, pairs, 141)
	for i, p := range pairs {
		require.False(t, p.DeletedConflict)
		if i > 0 {
			if pairs[i-1].Resource == p.Resource {
				require.Less(t, pairs[i-1].Action, p.Action)
			} else {
				require.Less(t, pairs[i-1].Resource, p.Resource)
			}
		}
	}
	for _, invalid := range [][]models.Permission{
		nil,
		{{Resource: "users", Action: "read"}, {Resource: "users", Action: "read"}},
		{{Resource: "private@example.invalid", Action: "read"}},
		{{Resource: "users\nprivate", Action: "read"}},
		{{Resource: "users", Action: ""}},
		make([]models.Permission, 142),
	} {
		result, err := seedDiagnosticExpectedPermissions(invalid)
		require.Error(t, err)
		require.Nil(t, result)
	}
}
