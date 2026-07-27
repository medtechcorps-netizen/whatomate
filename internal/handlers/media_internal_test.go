package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingObjectStore struct {
	objects map[string][]byte
}

var _ storage.ObjectStore = (*recordingObjectStore)(nil)

func (s *recordingObjectStore) Put(_ context.Context, key string, data []byte, _ string) error {
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[key] = append([]byte(nil), data...)
	return nil
}

func (s *recordingObjectStore) Get(_ context.Context, key string) ([]byte, string, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, "", fmt.Errorf("object %q not found", key)
	}
	return append([]byte(nil), data...), "", nil
}

func (s *recordingObjectStore) Delete(_ context.Context, _ string) error {
	return nil
}

func (s *recordingObjectStore) ListPrefix(_ context.Context, _ string) ([]storage.ObjectInfo, error) {
	return nil, nil
}

func TestSaveTenantMedia_UsesOrganizationPrefixedObjectKey(t *testing.T) {
	orgID := uuid.New()
	store := &recordingObjectStore{}
	app := &App{
		Config:      &config.Config{Storage: config.StorageConfig{Type: "s3"}},
		Log:         testutil.NopLogger(),
		ObjectStore: store,
	}

	key, err := app.saveTenantMedia(
		context.Background(),
		orgID,
		"messages/images",
		"images",
		"sample.jpg",
		[]byte("image-data"),
		"image/jpeg",
	)
	require.NoError(t, err)

	expectedKey := "organizations/" + orgID.String() + "/messages/images/sample.jpg"
	assert.Equal(t, expectedKey, key)
	assert.Equal(t, []byte("image-data"), store.objects[expectedKey])
}
