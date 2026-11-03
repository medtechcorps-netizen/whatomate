package mediaretirementcmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaInventoryPrivateOutputAtomicAndExclusive(t *testing.T) {
	for _, scenario := range []string{"success", "existing", "existing-symlink", "ancestor-symlink", "public-parent", "cancelled", "collision-after-open"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0700))
			path := filepath.Join(dir, "manifest.json")
			switch scenario {
			case "existing":
				require.NoError(t, os.WriteFile(path, []byte("KEEP"), 0600))
			case "existing-symlink":
				require.NoError(t, os.Symlink("missing-target", path))
			case "ancestor-symlink":
				target := dir
				alias := filepath.Join(t.TempDir(), "linked")
				require.NoError(t, os.Symlink(target, alias))
				path = filepath.Join(alias, "manifest.json")
			case "public-parent":
				require.NoError(t, os.Chmod(dir, 0755))
			}
			out, err := openMediaInventoryOutput(path)
			if scenario == "existing" || scenario == "existing-symlink" || scenario == "ancestor-symlink" || scenario == "public-parent" {
				require.Error(t, err)
				require.Nil(t, out)
				return
			}
			require.NoError(t, err)
			_, err = os.Stat(path)
			require.True(t, os.IsNotExist(err), "final name must be absent before complete publication")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			if scenario == "collision-after-open" {
				require.NoError(t, os.WriteFile(path, []byte("KEEP"), 0600))
			}
			published, err := out.publish(ctx, []byte(`{"complete":true}`))
			if scenario == "success" {
				require.NoError(t, err)
				require.True(t, published)
			} else {
				require.Error(t, err)
				require.False(t, published)
			}
			require.NoError(t, out.close())
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			if scenario == "cancelled" {
				require.Empty(t, entries)
				return
			}
			require.Len(t, entries, 1)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			if scenario == "collision-after-open" {
				require.Equal(t, "KEEP", string(raw))
			} else {
				require.JSONEq(t, `{"complete":true}`, string(raw))
				st, err := os.Stat(path)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0600), st.Mode().Perm())
			}
		})
	}
}

func TestMediaInventoryFailedCaptureRemovesPrivateTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	out, err := openMediaInventoryOutput(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, out.close())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
