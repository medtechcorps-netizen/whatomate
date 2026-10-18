// Package callingaudio persists immutable, tenant-scoped OGG audio and resolves
// it to an atomic local cache for the existing RTP player. Legacy files in the
// shared audio directory are not ownership evidence: existing installations
// must verify and re-upload those assets for each tenant when upgrading.
package callingaudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/storage"
)

const MaxAudioBytes = 32 << 20

var (
	ErrInvalidReference = errors.New("invalid calling audio reference")
	ErrNotFound         = errors.New("calling audio not found; legacy global files require tenant-verified re-upload")
	ErrInvalidAudio     = errors.New("invalid calling audio data")
	filenamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,123}\.ogg$`)
	hashPattern         = regexp.MustCompile(`^[a-f0-9]{64}\.ogg$`)
)

// Store uses objects as the source of truth when configured. The local
// directory is then only a cache; nil objects supports local installations.
// The fixed lock table bounds memory while coalescing concurrent cold reads.
type Store struct {
	directory string
	objects   storage.ObjectStore
	locks     [256]sync.Mutex
}

func New(localAudioDir string, objects storage.ObjectStore) *Store {
	if localAudioDir == "" {
		localAudioDir = "./audio"
	}
	return &Store{directory: localAudioDir, objects: objects}
}

// ValidateFilename accepts basenames only. Legacy basenames can be explicitly
// migrated into a tenant namespace, but are never read from the global root.
func ValidateFilename(filename string) error {
	if !filenamePattern.MatchString(filename) {
		return ErrInvalidReference
	}
	return nil
}

func identity(orgID uuid.UUID, filename string) (string, error) {
	if orgID == uuid.Nil {
		return "", ErrInvalidReference
	}
	if err := ValidateFilename(filename); err != nil {
		return "", err
	}
	return "organizations/" + orgID.String() + "/calling/audio/" + filename, nil
}

func (s *Store) lock(key string) func() {
	hash := sha256.Sum256([]byte(key))
	s.locks[hash[0]].Lock()
	return s.locks[hash[0]].Unlock
}

func validateData(filename string, data []byte) error {
	if len(data) < 4 || len(data) > MaxAudioBytes || !bytes.Equal(data[:4], []byte("OggS")) {
		return ErrInvalidAudio
	}
	if hashPattern.MatchString(filename) && filename != fmt.Sprintf("%x.ogg", sha256.Sum256(data)) {
		return ErrInvalidAudio
	}
	return nil
}

// Save returns a content-addressed immutable basename only after durable
// storage and the local cache both succeed. A failed object write never falls
// back to a local-only success, even if a copy was previously cached.
func (s *Store) Save(ctx context.Context, orgID uuid.UUID, data []byte) (string, error) {
	filename := fmt.Sprintf("%x.ogg", sha256.Sum256(data))
	key, err := identity(orgID, filename)
	if err != nil {
		return "", err
	}
	if err := validateData(filename, data); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	unlock := s.lock(key)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.objects != nil {
		if err := s.objects.Put(ctx, key, data, "audio/ogg"); err != nil {
			return "", fmt.Errorf("persist calling audio: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.cache(orgID, filename, data); err != nil {
		return "", err
	}
	return filename, nil
}

// Resolve retrieves only this organization's object. Missing objects and
// provider failures are returned unchanged in the error chain; neither can
// select an unscoped local file or a different organization's cached copy.
// Corrupt existing cache files fail closed; they must be removed before a
// subsequent resolution can retrieve the durable copy.
func (s *Store) Resolve(ctx context.Context, orgID uuid.UUID, filename string) (string, error) {
	key, err := identity(orgID, filename)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	unlock := s.lock(key)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_, err = s.readCache(orgID, filename)
	if err == nil {
		return s.path(orgID, filename), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if s.objects == nil {
		return "", ErrNotFound
	}
	data, _, err := s.objects.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return "", fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return "", fmt.Errorf("load calling audio: %w", err)
	}
	if err := validateData(filename, data); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.cache(orgID, filename, data); err != nil {
		return "", err
	}
	return s.path(orgID, filename), nil
}

func (s *Store) Read(ctx context.Context, orgID uuid.UUID, filename string) ([]byte, error) {
	if _, err := s.Resolve(ctx, orgID, filename); err != nil {
		return nil, err
	}
	return s.readCache(orgID, filename)
}

func (s *Store) path(orgID uuid.UUID, filename string) string {
	return filepath.Join(s.directory, "organizations", orgID.String(), "calling", "audio", filename)
}

// openDirectory rejects symlink aliases at every namespace component and
// uses os.Root for subsequent operations, bounding access to the tenant dir.
func (s *Store) openDirectory(orgID uuid.UUID, create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(s.directory, 0700); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, err
	}
	for _, component := range []string{"organizations", orgID.String(), "calling", "audio"} {
		if create {
			if err := root.Mkdir(component, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				_ = root.Close()
				return nil, err
			}
		}
		info, err := root.Lstat(component)
		if err != nil {
			_ = root.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = root.Close()
			return nil, ErrInvalidReference
		}
		next, err := root.OpenRoot(component)
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func (s *Store) readCache(orgID uuid.UUID, filename string) ([]byte, error) {
	root, err := s.openDirectory(orgID, false)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck
	info, err := root.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxAudioBytes {
		return nil, ErrInvalidAudio
	}
	file, err := root.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, MaxAudioBytes+1))
	if err != nil {
		return nil, err
	}
	if err := validateData(filename, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Store) cache(orgID uuid.UUID, filename string, data []byte) error {
	if existing, err := s.readCache(orgID, filename); err == nil {
		if !bytes.Equal(existing, data) {
			return ErrInvalidAudio
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := s.openDirectory(orgID, true)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck
	temporary := ".audio-" + uuid.NewString() + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary) //nolint:errcheck
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Rename(temporary, filename); err != nil {
		// Windows refuses rename onto an existing file. Another store/process
		// may have atomically published the identical immutable object first.
		if existing, readErr := s.readCache(orgID, filename); readErr == nil && bytes.Equal(existing, data) {
			return nil
		}
		return err
	}
	return nil
}
