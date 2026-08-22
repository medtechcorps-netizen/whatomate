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

func TestPlatformComplianceRegistryCoversEveryOrganizationScopedTable(t *testing.T) {
	t.Parallel()

	rules, err := PlatformComplianceTableRules()
	require.NoError(t, err)
	require.Len(t, rules, len(DirectTenantTables)+len(DirectTenantTableExemptions))

	seen := make(map[string]PlatformComplianceTableRule, len(rules))
	for _, rule := range rules {
		assert.NotEmpty(t, rule.ReviewReason, "compliance rule for %s needs a review reason", rule.Table)
		_, duplicate := seen[rule.Table]
		assert.False(t, duplicate, "duplicate compliance rule for %s", rule.Table)
		seen[rule.Table] = rule
	}
	for _, table := range DirectTenantTables {
		rule, exists := seen[table]
		assert.True(t, exists, "direct tenant table %s lacks a compliance rule", table)
		assert.True(t, rule.ForceTenantRLS, "direct tenant table %s must retain FORCE RLS", table)
	}
	for table := range DirectTenantTableExemptions {
		rule, exists := seen[table]
		assert.True(t, exists, "pre-tenant exemption %s lacks a compliance rule", table)
		assert.False(t, rule.ForceTenantRLS, "pre-tenant exemption %s must not be mislabeled FORCE RLS", table)
	}
	for table, writable := range platformComplianceWritableExemptions {
		rule := seen[table]
		assert.NotEmpty(t, writable.reason)
		assert.NotEmpty(t, rule.AllowedRowPredicate, "writable table %s needs an exact row predicate", table)
	}
	assert.Empty(t, seen["threads_platform_bindings"].AllowedRowPredicate)
	assert.Empty(t, seen["retention_policies"].AllowedRowPredicate)
	assert.Empty(t, seen["legal_holds"].AllowedRowPredicate)
}
