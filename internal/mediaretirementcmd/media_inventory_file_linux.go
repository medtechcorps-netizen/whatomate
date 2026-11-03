package mediaretirementcmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
	"golang.org/x/sys/unix"
)

type mediaPrivateOutput struct {
	dir              int
	file             *os.File
	temporary, final string
	published        bool
}

// Walk directory handles without following any symlink. The final parent must
// belong to this effective UID and be exactly0700; no chmod or overwrite occurs.
func openMediaInventoryOutput(path string) (mediaInventoryOutput, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, mediaretirement.ErrIncomplete
	}
	dir, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(dir)
		}
	}()
	parent := filepath.Dir(path)
	for _, part := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return nil, mediaretirement.ErrIncomplete
		}
		_ = unix.Close(dir)
		dir = next
	}
	var st unix.Stat_t
	if unix.Fstat(dir, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&07777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		return nil, mediaretirement.ErrIncomplete
	}
	name := filepath.Base(path)
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return nil, mediaretirement.ErrIncomplete
	}
	temporary := ".rereply-media-" + uuid.NewString() + ".tmp"
	fd, err := unix.Openat(dir, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	file := os.NewFile(uintptr(fd), temporary)
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(dir, temporary, 0)
		return nil, mediaretirement.ErrIncomplete
	}
	ok = true
	return &mediaPrivateOutput{dir: dir, file: file, temporary: temporary, final: name}, nil
}

func (o *mediaPrivateOutput) publish(ctx context.Context, raw []byte) (bool, error) {
	if ctx == nil || ctx.Err() != nil || o.published || o.file == nil || len(raw) == 0 || len(raw) > mediaretirement.MaxPrivateBytes {
		return false, mediaretirement.ErrIncomplete
	}
	n, err := o.file.Write(raw)
	if err != nil || n != len(raw) {
		return false, io.ErrShortWrite
	}
	if o.file.Sync() != nil || ctx.Err() != nil {
		return false, mediaretirement.ErrIncomplete
	}
	if err := o.file.Close(); err != nil {
		o.file = nil
		return false, mediaretirement.ErrIncomplete
	}
	o.file = nil
	if ctx.Err() != nil {
		return false, mediaretirement.ErrIncomplete
	}
	// No-replace rename installs only a complete synced file. An existing file
	// created after admission wins; the inventory never overwrites it.
	if unix.Renameat2(o.dir, o.temporary, o.dir, o.final, unix.RENAME_NOREPLACE) != nil {
		return false, mediaretirement.ErrIncomplete
	}
	o.published = true
	if unix.Fsync(o.dir) != nil {
		return true, mediaretirement.ErrIncomplete
	}
	return true, nil
}

func (o *mediaPrivateOutput) close() error {
	var result error
	if o.file != nil {
		if o.file.Close() != nil {
			result = mediaretirement.ErrIncomplete
		}
		o.file = nil
	}
	if o.dir >= 0 {
		if !o.published {
			if err := unix.Unlinkat(o.dir, o.temporary, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				result = mediaretirement.ErrIncomplete
			}
		}
		if unix.Close(o.dir) != nil {
			result = mediaretirement.ErrIncomplete
		}
		o.dir = -1
	}
	return result
}
