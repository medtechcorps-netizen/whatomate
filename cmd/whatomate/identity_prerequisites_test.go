package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/require"
)

func identityPrerequisiteArguments() []string {
	return []string{"-mode", "json", "-identity-prerequisites"}
}

func TestIdentityPrerequisitesArgumentsAndPrivacy(t *testing.T) {
	args, err := parseCatalogSnapshotArgs(identityPrerequisiteArguments())
	require.NoError(t, err)
	require.NoError(t, validateIdentityPrerequisiteArguments(args))
	for _, bad := range [][]string{
		{"-identity-prerequisites"},
		{"-mode", "json", "-identity-prerequisites=false"},
		{"-mode", "json", "-identity-prerequisites", "-identity-inventory=false"},
		{"-mode", "json", "-identity-prerequisites", "-seed-counts=false"},
		{"-mode", "json", "-identity-prerequisites", "-seed-diff"},
		{"-mode", "json", "-identity-prerequisites", "-list-unknown=false"},
		{"-mode", "json", "-identity-prerequisites", "-organization-id=private"},
		{"-mode", "json", "-identity-prerequisites", "-account-id=private"},
		{"-mode", "json", "-identity-prerequisites", "--identity-prerequisites"},
		{"-mode", "json", "-identity-prerequisites", "-mode=json"},
		{"-mode", "json", "-identity-prerequisites", "-dsn=private"},
		{"-mode", "json", "-identity-prerequisites", "positional-private"},
	} {
		var stdout, stderr bytes.Buffer
		code := catalogSnapshotCommand(context.Background(), bad, &stdout, &stderr,
			func(string) (*config.Config, error) {
				t.Fatal("invalid arguments reached configuration")
				return nil, nil
			},
			func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
				t.Fatal("invalid arguments reached collection")
				return nil, nil
			})
		require.Equal(t, 2, code)
		require.Empty(t, stdout.String())
		require.Equal(t, "catalog-snapshot:invalid-arguments; use -help\n", stderr.String())
	}
	var stdout, stderr bytes.Buffer
	code := catalogSnapshotCommand(context.Background(), identityPrerequisiteArguments(), &stdout, &stderr,
		func(string) (*config.Config, error) { return &config.Config{}, nil },
		func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
			return []byte("partial-private"), errors.New("private-driver-host-role")
		})
	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "catalog-snapshot:capture-failed\n", stderr.String())
	for _, old := range [][]string{nil, {"-mode", "json"}, {"-mode", "json", "-seed-diff"}, identityInventoryArguments()} {
		_, err := parseCatalogSnapshotArgs(old)
		require.NoError(t, err)
	}
	inventory, err := parseCatalogSnapshotArgs(identityInventoryArguments())
	require.NoError(t, err)
	inventory.identityPrerequisites = true
	require.Error(t, validateIdentityInventoryArguments(inventory))
	require.Error(t, validateIdentityPrerequisiteArguments(inventory))
}

func TestIdentityPrerequisitesRefusesOverridesAndCanceledWork(t *testing.T) {
	args, err := parseCatalogSnapshotArgs(identityPrerequisiteArguments())
	require.NoError(t, err)
	for _, key := range []string{"options", "role"} {
		cfg := config.DatabaseConfig{URL: "postgres://private-user:private-password@private-host/private-database?" + key + "=private-value", MigrationURL: "must-not-be-used"}
		raw, err := collectIdentityPrerequisites(context.Background(), cfg, args)
		require.EqualError(t, err, "identity prerequisites unavailable")
		require.Nil(t, raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, err := collectIdentityPrerequisites(ctx, config.DatabaseConfig{}, args)
	require.EqualError(t, err, "identity prerequisites unavailable")
	require.Nil(t, raw)
}
