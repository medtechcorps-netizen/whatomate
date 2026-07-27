package database

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryMigratedOrganizationModelIsRegisteredForTenantRLS(t *testing.T) {
	t.Parallel()

	registered := make(map[string]struct{}, len(DirectTenantTables))
	for _, table := range DirectTenantTables {
		registered[table] = struct{}{}
	}

	type tableNamer interface {
		TableName() string
	}

	for _, migration := range GetMigrationModels() {
		modelType := reflect.TypeOf(migration.Model)
		require.NotNil(t, modelType, "migration %s has a nil model", migration.Name)
		if modelType.Kind() == reflect.Pointer {
			modelType = modelType.Elem()
		}
		if modelType.Kind() != reflect.Struct {
			continue
		}
		if _, hasOrganizationID := modelType.FieldByName("OrganizationID"); !hasOrganizationID {
			continue
		}

		namer, ok := migration.Model.(tableNamer)
		require.True(t, ok, "tenant migration model %s must define TableName", migration.Name)
		tableName := namer.TableName()
		if reason, exempt := DirectTenantTableExemptions[tableName]; exempt {
			assert.NotEmpty(t, reason, "RLS exemption for %s must explain the lifecycle", tableName)
			continue
		}
		_, ok = registered[tableName]
		assert.True(
			t,
			ok,
			"migrated tenant model %s (%s) is missing from DirectTenantTables",
			migration.Name,
			tableName,
		)
	}
}
