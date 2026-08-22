package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/platformcompliance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDispatchPlatformComplianceBootstrapRejectsPositionalSyntaxBeforeDependencies(t *testing.T) {
	t.Parallel()

	validPrefix := []string{
		"-config", "must-not-be-loaded.toml",
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
		"-feature", "instagram",
	}
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "separate boolean value becomes positional",
			args: append(append([]string{}, validPrefix...), "-apply", "false"),
		},
		{
			name: "terminal flag terminator is forbidden",
			args: append(append([]string{}, validPrefix...), "--"),
		},
		{
			name: "stray positional argument is forbidden",
			args: append(append([]string{}, validPrefix...), "unexpected"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dependenciesReached := false
			err := dispatchPlatformComplianceBootstrap(test.args, func(platformComplianceBootstrapArgs) {
				dependenciesReached = true
			})

			require.Error(t, err)
			assert.False(t, dependenciesReached, "invalid syntax must not reach configuration loading or database access")
		})
	}
}

func TestDispatchPlatformComplianceBootstrapRejectsNonCanonicalOrDuplicateFlags(t *testing.T) {
	t.Parallel()

	valid := []string{
		"-config", "reviewed-config.toml",
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
		"-feature", "instagram",
	}
	tests := []struct {
		name  string
		extra []string
	}{
		{name: "numeric apply alias", extra: []string{"-apply=1"}},
		{name: "short apply alias", extra: []string{"-apply=t"}},
		{name: "uppercase apply alias", extra: []string{"-apply=TRUE"}},
		{name: "numeric create alias", extra: []string{"-create-purpose=1"}},
		{name: "double hyphen alias", extra: []string{"--apply"}},
		{name: "duplicate config", extra: []string{"-config", "other.toml"}},
		{name: "duplicate organization", extra: []string{"-organization-id", "22222222-2222-4222-8222-222222222222"}},
		{name: "duplicate run", extra: []string{"-operator-run-id", "reviewed-run-02"}},
		{name: "duplicate create", extra: []string{"-create-purpose=false", "-create-purpose"}},
		{name: "duplicate apply", extra: []string{"-apply=false", "-apply"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			called := false
			args := append(append([]string{}, valid...), test.extra...)
			err := dispatchPlatformComplianceBootstrap(args, func(platformComplianceBootstrapArgs) {
				called = true
			})
			require.Error(t, err)
			assert.False(t, called, "invalid grammar must not reach dependencies")
		})
	}
}

func TestDispatchPlatformComplianceBootstrapRejectsBlankSeparateValuesBeforeDependencies(t *testing.T) {
	t.Parallel()

	base := []string{
		"-config", "reviewed-config.toml",
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
		"-feature", "instagram",
	}
	valueFlags := []string{
		"-config",
		"-organization-id",
		"-operator-run-id",
		"-feature",
		"-remove-feature",
	}
	blanks := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace", value: " \t "},
	}

	for _, flagName := range valueFlags {
		for _, blank := range blanks {
			t.Run(strings.TrimPrefix(flagName, "-")+"/"+blank.name, func(t *testing.T) {
				t.Parallel()
				args := append([]string{}, base...)
				if flagName == "-remove-feature" {
					args = append(args, flagName, blank.value)
				} else {
					for index := range args {
						if args[index] == flagName {
							args[index+1] = blank.value
							break
						}
					}
				}

				called := false
				err := dispatchPlatformComplianceBootstrap(args, func(platformComplianceBootstrapArgs) {
					called = true
				})
				require.Error(t, err)
				assert.False(t, called, "blank values must not reach configuration loading or database access")
			})
		}
	}
}

func TestDispatchPlatformComplianceBootstrapAllowsOnlyDocumentedRepetition(t *testing.T) {
	t.Parallel()

	called := false
	err := dispatchPlatformComplianceBootstrap([]string{
		"-config=reviewed-config.toml",
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
		"-feature", "instagram",
		"-feature=threads",
		"-apply=true",
	}, func(parsed platformComplianceBootstrapArgs) {
		called = true
		assert.True(t, parsed.apply)
		assert.Equal(t, platformComplianceFeatures{"instagram", "threads"}, parsed.features)
	})
	require.NoError(t, err)
	assert.True(t, called)

	called = false
	err = dispatchPlatformComplianceBootstrap([]string{
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-02",
		"-remove-feature", "instagram",
		"-remove-feature=threads",
	}, func(parsed platformComplianceBootstrapArgs) {
		called = true
		assert.Equal(t, platformComplianceFeatures{"instagram", "threads"}, parsed.removeFeatures)
	})
	require.NoError(t, err)
	assert.True(t, called)
}

func TestPlatformComplianceBootstrapFailureMessageDistinguishesCommitOutcome(t *testing.T) {
	t.Parallel()

	indeterminate := platformComplianceBootstrapFailureMessage(
		errors.Join(
			platformcompliance.ErrCommitOutcomeIndeterminate,
			errors.New("postgres://secret-user:secret-password@example.invalid/database"),
		),
		true,
	)
	assert.Contains(t, strings.ToLower(indeterminate), "indeterminate")
	assert.Contains(t, strings.ToLower(indeterminate), "identical")
	assert.NotContains(t, strings.ToLower(indeterminate), "no changes were committed")
	assert.NotContains(t, indeterminate, "secret-password")

	definite := platformComplianceBootstrapFailureMessage(errors.New("validation failed"), true)
	assert.Contains(t, strings.ToLower(definite), "no changes were committed")

	dryRun := platformComplianceBootstrapFailureMessage(errors.New("validation failed"), false)
	assert.Contains(t, strings.ToLower(dryRun), "requested no database changes")
}

func TestRequiredStartupDatabaseContract(t *testing.T) {
	t.Parallel()

	complianceID := "11111111-1111-4111-8111-111111111111"
	clinicID := "22222222-2222-4222-8222-222222222222"
	tests := []struct {
		name string
		cfg  *config.Config
		want startupDatabaseContract
	}{
		{name: "nil", cfg: nil, want: startupDatabaseContractNone},
		{name: "ordinary RLS off", cfg: &config.Config{}, want: startupDatabaseContractNone},
		{name: "Messenger only", cfg: &config.Config{MetaMessenger: config.MetaMessengerConfig{Enabled: true}}, want: startupDatabaseContractNone},
		{name: "reader-first legacy Instagram", cfg: &config.Config{MetaInstagram: config.MetaInstagramConfig{Enabled: true}}, want: startupDatabaseContractPlatformCompliance},
		{name: "managed Instagram compliance", cfg: &config.Config{MetaInstagram: config.MetaInstagramConfig{Enabled: true, AllowedOrganizationIDs: clinicID, DataDeletionComplianceOrganizationID: complianceID}}, want: startupDatabaseContractPlatformCompliance},
		{name: "disabled Instagram config does not activate", cfg: &config.Config{MetaInstagram: config.MetaInstagramConfig{DataDeletionComplianceOrganizationID: complianceID}}, want: startupDatabaseContractNone},
		{name: "reader-first Threads", cfg: &config.Config{ThreadsManaged: config.ThreadsManagedConfig{Enabled: true}}, want: startupDatabaseContractPlatformCompliance},
		{name: "managed Threads compliance", cfg: &config.Config{ThreadsManaged: config.ThreadsManagedConfig{Enabled: true, ComplianceOrganizationID: complianceID}}, want: startupDatabaseContractPlatformCompliance},
		{name: "RLS takes precedence", cfg: &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}, MetaInstagram: config.MetaInstagramConfig{Enabled: true, AllowedOrganizationIDs: clinicID, DataDeletionComplianceOrganizationID: complianceID}}, want: startupDatabaseContractTenantRLS},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, requiredStartupDatabaseContract(test.cfg))
		})
	}
}

func TestValidateServerMigrationMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *config.Config
		migrate bool
		wantErr bool
	}{
		{
			name:    "ordinary server may migrate",
			cfg:     &config.Config{},
			migrate: true,
		},
		{
			name: "Messenger-only server may migrate",
			cfg: &config.Config{
				MetaMessenger: config.MetaMessengerConfig{Enabled: true},
			},
			migrate: true,
		},
		{
			name: "platform compliance server without migration remains allowed",
			cfg: &config.Config{
				MetaInstagram: config.MetaInstagramConfig{Enabled: true},
			},
		},
		{
			name: "tenant RLS server without migration remains allowed",
			cfg: &config.Config{
				Database: config.DatabaseConfig{RLSEnabled: true},
			},
		},
		{
			name: "Instagram compliance blocks server migration",
			cfg: &config.Config{
				MetaInstagram: config.MetaInstagramConfig{Enabled: true},
			},
			migrate: true,
			wantErr: true,
		},
		{
			name: "Threads compliance blocks server migration",
			cfg: &config.Config{
				ThreadsManaged: config.ThreadsManagedConfig{Enabled: true},
			},
			migrate: true,
			wantErr: true,
		},
		{
			name: "tenant RLS blocks server migration",
			cfg: &config.Config{
				Database: config.DatabaseConfig{RLSEnabled: true},
			},
			migrate: true,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateServerMigrationMode(test.cfg, test.migrate)
			if test.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "rls-migrate")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDispatchPlatformComplianceBootstrapAcceptsCanonicalFalseBoolean(t *testing.T) {
	t.Parallel()

	dependenciesReached := false
	var parsed platformComplianceBootstrapArgs
	err := dispatchPlatformComplianceBootstrap([]string{
		"-config", "reviewed-config.toml",
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
		"-feature", "instagram",
		"-apply=false",
	}, func(args platformComplianceBootstrapArgs) {
		dependenciesReached = true
		parsed = args
	})

	require.NoError(t, err)
	assert.True(t, dependenciesReached)
	assert.False(t, parsed.apply)
	assert.Equal(t, "reviewed-config.toml", parsed.configPath)
}

func TestDispatchPlatformComplianceBootstrapAtomicCreationMode(t *testing.T) {
	t.Parallel()

	base := []string{
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
		"-create-purpose",
		"-feature", "instagram",
	}
	t.Run("atomic creation dry run parses without external dependencies", func(t *testing.T) {
		t.Parallel()
		called := false
		err := dispatchPlatformComplianceBootstrap(base, func(parsed platformComplianceBootstrapArgs) {
			called = true
			assert.True(t, parsed.createPurpose)
			assert.False(t, parsed.apply)
			assert.Equal(t, platformComplianceFeatures{"instagram"}, parsed.features)
			assert.Empty(t, parsed.removeFeatures)
		})
		require.NoError(t, err)
		assert.True(t, called)
	})

	t.Run("removal conflict", func(t *testing.T) {
		t.Parallel()
		called := false
		err := dispatchPlatformComplianceBootstrap(
			append(append([]string{}, base...), "-remove-feature", "threads"),
			func(platformComplianceBootstrapArgs) { called = true },
		)
		require.Error(t, err)
		assert.False(t, called, "invalid mixed mode must not reach configuration or databases")
	})
}

func TestDispatchPlatformComplianceBootstrapRejectsEmptyOperationBeforeDependencies(t *testing.T) {
	t.Parallel()
	called := false
	err := dispatchPlatformComplianceBootstrap([]string{
		"-organization-id", "11111111-1111-4111-8111-111111111111",
		"-operator-run-id", "reviewed-run-01",
	}, func(platformComplianceBootstrapArgs) {
		called = true
	})
	require.Error(t, err)
	assert.False(t, called)
}
