package callingaudio

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/stretchr/testify/require"
)

type memoryObjects struct {
	mu       sync.Mutex
	data     map[string][]byte
	gets     int
	putError error
	getError error
}

func (o *memoryObjects) Put(_ context.Context, key string, data []byte, contentType string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.putError != nil {
		return o.putError
	}
	if o.data == nil {
		o.data = make(map[string][]byte)
	}
	o.data[key] = bytes.Clone(data)
	return nil
}
func (o *memoryObjects) Get(_ context.Context, key string) ([]byte, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gets++
	if o.getError != nil {
		return nil, "", o.getError
	}
	data, ok := o.data[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return bytes.Clone(data), "audio/ogg", nil
}
func (o *memoryObjects) Delete(context.Context, string) error { panic("unexpected delete") }
func (o *memoryObjects) ListPrefix(context.Context, string) ([]storage.ObjectInfo, error) {
	panic("unexpected list")
}

func TestColdReplicaResolvesDurableAudioAndCachesPerTenant(t *testing.T) {
	objects := &memoryObjects{}
	org := uuid.New()
	data := []byte("OggS-test-audio")
	first := New(t.TempDir(), objects)
	name, err := first.Save(context.Background(), org, data)
	require.NoError(t, err)
	require.Equal(t, map[string][]byte{"organizations/" + org.String() + "/calling/audio/" + name: data}, objects.data)
	// A fresh process/replica has no original upload directory.
	second := New(t.TempDir(), objects)
	path, err := second.Resolve(context.Background(), org, name)
	require.NoError(t, err)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, actual)
	actual, err = second.Read(context.Background(), org, name)
	require.NoError(t, err)
	require.Equal(t, data, actual)
	require.Equal(t, 1, objects.gets)
	_, err = second.Resolve(context.Background(), uuid.New(), name)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLocalModeAndImmutableReplacement(t *testing.T) {
	store := New(t.TempDir(), nil)
	org := uuid.New()
	first, err := store.Save(context.Background(), org, []byte("OggS-first"))
	require.NoError(t, err)
	second, err := store.Save(context.Background(), org, []byte("OggS-second"))
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	again, err := store.Save(context.Background(), org, []byte("OggS-first"))
	require.NoError(t, err)
	require.Equal(t, first, again)
	data, err := store.Read(context.Background(), org, first)
	require.NoError(t, err)
	require.Equal(t, "OggS-first", string(data))
}

func TestConcurrentColdReadersPublishOneCompleteCacheFile(t *testing.T) {
	objects := &memoryObjects{}
	org := uuid.New()
	data := append([]byte("OggS"), bytes.Repeat([]byte{0x42}, 1024*1024)...)
	name, err := New(t.TempDir(), objects).Save(context.Background(), org, data)
	require.NoError(t, err)
	reader := New(t.TempDir(), objects)
	start := make(chan struct{})
	errors := make(chan error, 24)
	var wg sync.WaitGroup
	for i := 0; i < cap(errors); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := reader.Read(context.Background(), org, name)
			if err == nil && !bytes.Equal(got, data) {
				err = ErrInvalidAudio
			}
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Equal(t, 1, objects.gets)
	entries, err := os.ReadDir(filepath.Dir(reader.path(org, name)))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, name, entries[0].Name())
}

func TestIndependentStoresCanPublishSameImmutableFile(t *testing.T) {
	directory := t.TempDir()
	org := uuid.New()
	data := append([]byte("OggS"), bytes.Repeat([]byte{0x21}, 256*1024)...)
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for i := 0; i < cap(errors); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := New(directory, nil).Save(context.Background(), org, data)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
}

func TestStorageFailuresNeverFallBackToGlobalOrLocalWrite(t *testing.T) {
	org := uuid.New()
	failure := errors.New("provider unavailable")
	objects := &memoryObjects{putError: failure, getError: failure}
	directory := t.TempDir()
	store := New(directory, objects)
	_, err := store.Save(context.Background(), org, []byte("OggS-data"))
	require.ErrorIs(t, err, failure)
	_, err = os.Stat(filepath.Join(directory, "organizations"))
	require.True(t, os.IsNotExist(err))
	legacy := uuid.NewString() + ".ogg"
	require.NoError(t, os.WriteFile(filepath.Join(directory, legacy), []byte("OggS-private-global"), 0600))
	_, err = store.Resolve(context.Background(), org, legacy)
	require.ErrorIs(t, err, failure)
	objects.getError = nil
	_, err = store.Resolve(context.Background(), org, legacy)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = New(directory, nil).Resolve(context.Background(), org, legacy)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMalformedReferencesAndDataCannotEnterCache(t *testing.T) {
	objects := &memoryObjects{}
	store := New(t.TempDir(), objects)
	org := uuid.New()
	for _, name := range []string{"../other.ogg", `..\other.ogg`, "/audio.ogg", "https://host/a.ogg", "file.ogg:stream", ".hidden.ogg", "file.mp3", ""} {
		_, err := store.Resolve(context.Background(), org, name)
		require.ErrorIs(t, err, ErrInvalidReference, name)
	}
	_, err := store.Save(context.Background(), uuid.Nil, []byte("OggS-data"))
	require.ErrorIs(t, err, ErrInvalidReference)
	_, err = store.Save(context.Background(), org, []byte("not ogg"))
	require.ErrorIs(t, err, ErrInvalidAudio)
	require.Zero(t, objects.gets)
	name := string(bytes.Repeat([]byte{'a'}, 64)) + ".ogg"
	key, _ := identity(org, name)
	require.NoError(t, objects.Put(context.Background(), key, []byte("OggS-wrong-hash"), "audio/ogg"))
	_, err = store.Resolve(context.Background(), org, name)
	require.ErrorIs(t, err, ErrInvalidAudio)
	_, err = os.Stat(store.path(org, name))
	require.True(t, os.IsNotExist(err))
}

func TestCancellationDoesNotReadProvider(t *testing.T) {
	objects := &memoryObjects{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(t.TempDir(), objects).Resolve(ctx, uuid.New(), "test.ogg")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, objects.gets)
}

func TestCacheSymlinksCannotReadAnotherTenant(t *testing.T) {
	directory := t.TempDir()
	store := New(directory, nil)
	org := uuid.New()
	other := uuid.New()
	name, err := store.Save(context.Background(), other, []byte("OggS-private"))
	require.NoError(t, err)
	alias := filepath.Join(directory, "organizations", org.String())
	if err := os.Symlink(filepath.Join(directory, "organizations", other.String()), alias); err != nil {
		t.Skip("symlink creation unavailable")
	}
	_, err = store.Resolve(context.Background(), org, name)
	require.Error(t, err)
}

type cancelObjects struct {
	*memoryObjects
	started chan struct{}
}

func (o *cancelObjects) Get(ctx context.Context, _ string) ([]byte, string, error) {
	close(o.started)
	<-ctx.Done()
	return nil, "", ctx.Err()
}

func TestInFlightDownloadCancellationPublishesNoCache(t *testing.T) {
	objects := &cancelObjects{memoryObjects: &memoryObjects{}, started: make(chan struct{})}
	directory := t.TempDir()
	store := New(directory, objects)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.Resolve(ctx, uuid.New(), "interrupted.ogg"); done <- err }()
	<-objects.started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries, "canceled download must not create directories, temporary or final files")
}

func TestCorruptCacheFailsClosedWithoutReplacingOrFetching(t *testing.T) {
	objects := &memoryObjects{}
	store := New(t.TempDir(), objects)
	org := uuid.New()
	name, err := store.Save(context.Background(), org, []byte("OggS-complete-data"))
	require.NoError(t, err)
	for _, corrupt := range [][]byte{[]byte("Ogg"), []byte("OggS-changed-data")} {
		require.NoError(t, os.WriteFile(store.path(org, name), corrupt, 0600))
		_, err := store.Resolve(context.Background(), org, name)
		require.ErrorIs(t, err, ErrInvalidAudio)
		actual, err := os.ReadFile(store.path(org, name))
		require.NoError(t, err)
		require.Equal(t, corrupt, actual)
	}
	require.Zero(t, objects.gets)
}
