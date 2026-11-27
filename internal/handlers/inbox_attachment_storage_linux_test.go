//go:build linux

package handlers

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestInboxAttachmentReadsPinnedLocalTenantAndProvedCoexistenceFiles(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	app := &App{Config: &config.Config{Storage: config.StorageConfig{LocalPath: base}}}
	org, message, epoch := uuid.New(), uuid.New(), uuid.New()
	native := path.Join("native", org.String(), message.String(), epoch.String()+".bin")
	name, err := coexistenceMediaFilename(coexistenceMediaStorageRevisionID(message, "fixture-revision"), "synthetic-media")
	require.NoError(t, err)
	coex := path.Join("coexistence", name)

	for _, tc := range []struct{ key, proof string }{{native, ""}, {coex, coex}} {
		file := filepath.Join(base, filepath.FromSlash(tc.key))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0700))
		require.NoError(t, os.WriteFile(file, []byte("pdf"), 0600))
		require.True(t, isInboxAttachmentStorageKey(org, tc.key, tc.proof))
		body, mime, err := app.readInboxAttachment(ctx, org, tc.key, 3, tc.proof)
		require.NoError(t, err)
		require.Equal(t, []byte("pdf"), body)
		require.Empty(t, mime)
		body, mime, err = app.readInboxAttachment(ctx, org, tc.key, 2, tc.proof)
		require.ErrorIs(t, err, storage.ErrObjectTooLarge)
		require.Nil(t, body)
		require.Empty(t, mime)

		// A refused read leaves the stored file available for a valid retry.
		body, _, err = app.readInboxAttachment(ctx, org, tc.key, 3, tc.proof)
		require.NoError(t, err)
		require.Equal(t, []byte("pdf"), body)
	}

	for _, tc := range []struct {
		org        uuid.UUID
		key, proof string
	}{
		{uuid.New(), native, ""},
		{org, coex, ""},
		{org, coex, "coexistence/" + uuid.NewString() + ".bin"},
		{org, "documents/" + uuid.NewString() + ".pdf", ""},
		{org, "native/" + org.String() + "/../" + native, ""},
	} {
		require.False(t, isInboxAttachmentStorageKey(tc.org, tc.key, tc.proof))
		_, _, err := app.readInboxAttachment(ctx, tc.org, tc.key, 3, tc.proof)
		require.ErrorIs(t, err, errInvalidStoredMediaKey)
	}
}

func TestInboxAttachmentRefusesLocalLinksAndFIFO(t *testing.T) {
	for _, mode := range []string{"leaf_symlink", "other_tenant_symlink_directory", "configured_base_ancestor_symlink", "hardlink", "fifo"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			ctx := context.Background()
			app := &App{Config: &config.Config{Storage: config.StorageConfig{LocalPath: base}}}
			org, other, message, epoch := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			key := path.Join("native", org.String(), message.String(), epoch.String()+".bin")
			foreignKey := path.Join("native", other.String(), message.String(), epoch.String()+".bin")
			owned := filepath.Join(base, filepath.FromSlash(key))
			foreign := filepath.Join(base, filepath.FromSlash(foreignKey))
			require.NoError(t, os.MkdirAll(filepath.Dir(foreign), 0700))
			require.NoError(t, os.WriteFile(foreign, []byte("private"), 0600))

			switch mode {
			case "other_tenant_symlink_directory":
				require.NoError(t, os.Symlink(other.String(), filepath.Join(base, "native", org.String())))
			case "configured_base_ancestor_symlink":
				require.NoError(t, os.MkdirAll(filepath.Dir(owned), 0700))
				require.NoError(t, os.WriteFile(owned, []byte("pdf"), 0600))
				alias := filepath.Join(t.TempDir(), "alias")
				require.NoError(t, os.Symlink(filepath.Dir(base), alias))
				app.Config.Storage.LocalPath = filepath.Join(alias, filepath.Base(base))
			default:
				require.NoError(t, os.MkdirAll(filepath.Dir(owned), 0700))
				switch mode {
				case "leaf_symlink":
					require.NoError(t, os.Symlink(foreign, owned))
				case "hardlink":
					require.NoError(t, os.Link(foreign, owned))
				case "fifo":
					require.NoError(t, unix.Mkfifo(owned, 0600))
				}
			}
			body, mime, err := app.readInboxAttachment(ctx, org, key, 100, "")
			require.ErrorIs(t, err, errInvalidStoredMediaKey)
			require.Nil(t, body)
			require.Empty(t, mime)
		})
	}
}
