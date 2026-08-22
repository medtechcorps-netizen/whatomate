package main

import (
	"testing"

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
