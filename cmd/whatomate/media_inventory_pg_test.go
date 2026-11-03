package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirementcmd"
	"github.com/stretchr/testify/require"
)

func TestMediaInventoryPGInstalledSchemaPrivateCommand(t *testing.T) {
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var org string
	planPGOK(t, f.admin.QueryRow(ctx, "SELECT id::text FROM public.organizations WHERE deleted_at IS NULL AND reseller_id IS NOT NULL ORDER BY id LIMIT 1").Scan(&org), "select synthetic organization")
	account := uuid.New()
	identityInventoryExec(t, f, "INSERT INTO public.whatsapp_accounts(id,created_at,updated_at,organization_id,name,phone_id,business_id,access_token,is_smb,status) VALUES($1,now(),now(),$2::uuid,'media-inventory-fixture','synthetic-phone','synthetic-business','DO_NOT_TRANSFER',true,'active')", account, org)
	ownerCfg := f.cfg
	ownerCfg.URL = f.cfg.MigrationURL
	ownerCfg.MigrationURL = "must-not-be-used"
	for _, role := range []string{"owner", "runtime"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0700))
			path := filepath.Join(dir, "manifest.json")
			args := []string{"-organization-id", org, "-account-id", account.String(), "-output", path}
			cfg := ownerCfg
			if role == "runtime" {
				cfg = f.cfg
			} // valid owner MigrationURL must not become fallback
			var stdout, stderr bytes.Buffer
			code := mediaretirementcmd.Execute(context.Background(), args, &stdout, &stderr, func(string) (*config.Config, error) { return &config.Config{Database: cfg}, nil })
			require.NotContains(t, stdout.String()+stderr.String(), org)
			require.NotContains(t, stdout.String()+stderr.String(), account.String())
			require.NotContains(t, stdout.String()+stderr.String(), "DO_NOT_TRANSFER")
			if role == "runtime" {
				require.Equal(t, 1, code)
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Empty(t, entries)
				return
			}
			require.Zero(t, code, stderr.String())
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "DO_NOT_TRANSFER")
			var report struct {
				Version          int `json:"version"`
				GlobalProtection struct {
					Complete               bool  `json:"complete"`
					ResellerLogoRows       int64 `json:"reseller_logo_rows"`
					RecoveryCheckpointRows int64 `json:"recovery_checkpoint_rows"`
					ExactOverlapRequired   bool  `json:"exact_overlap_required"`
				} `json:"global_protection"`
				Purpose         string   `json:"purpose"`
				SessionDisposed bool     `json:"session_disposed"`
				SameSnapshot    bool     `json:"same_snapshot"`
				Historical      bool     `json:"historical_attribution_complete"`
				Families        []string `json:"complete_families"`
			}
			require.NoError(t, json.Unmarshal(raw, &report))
			require.Equal(t, "media_reference_inventory", report.Purpose)
			require.True(t, report.SessionDisposed)
			require.True(t, report.SameSnapshot)
			require.False(t, report.Historical)
			require.Len(t, report.Families, 9)
			require.Equal(t, 2, report.Version)
			require.True(t, report.GlobalProtection.Complete)
			require.Zero(t, report.GlobalProtection.ResellerLogoRows)
			require.Zero(t, report.GlobalProtection.RecoveryCheckpointRows)
			require.False(t, report.GlobalProtection.ExactOverlapRequired)
		})
	}
}
