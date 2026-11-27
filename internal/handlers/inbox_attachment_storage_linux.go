//go:build linux

package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/shridarpatil/whatomate/internal/storage"
	"golang.org/x/sys/unix"
)

const inboxAttachmentLocalSupported = true

func readInboxAttachmentLocal(ctx context.Context, base string, parts []string, maxBytes int64) ([]byte, error) {
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	// Open every component from the filesystem root. O_NOFOLLOW on a
	// multi-component path protects only its final component.
	var baseParts []string
	if base != "/" {
		baseParts = strings.Split(strings.TrimPrefix(base, "/"), "/")
	}
	names := append(baseParts, parts...)
	directoryFlags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	rootFD, err := unix.Open("/", directoryFlags, 0)
	if err != nil {
		return nil, inboxAttachmentLocalOpenError(err)
	}
	dirs := []int{rootFD}
	defer func() {
		for _, fd := range dirs {
			_ = unix.Close(fd)
		}
	}()
	var anchorStat unix.Stat_t
	if err := unix.Fstat(rootFD, &anchorStat); err != nil {
		return nil, err
	}

	for i, name := range names[:len(names)-1] {
		fd, err := unix.Openat(dirs[len(dirs)-1], name, directoryFlags, 0)
		if err != nil {
			return nil, inboxAttachmentLocalOpenError(err)
		}
		dirs = append(dirs, fd)
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			return nil, err
		}
		// The configured media directory can itself be on a mounted volume.
		if i+1 == len(baseParts) {
			anchorStat = info
		} else if i >= len(baseParts) && info.Dev != anchorStat.Dev {
			return nil, errInvalidStoredMediaKey
		}
	}
	parent, name := dirs[len(dirs)-1], names[len(names)-1]
	var before unix.Stat_t
	if err := unix.Fstatat(parent, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if !inboxAttachmentRegularFile(before, anchorStat) {
		return nil, errInvalidStoredMediaKey
	}
	if before.Size > maxBytes {
		return nil, storage.ErrObjectTooLarge
	}

	// The path can be replaced with a FIFO after the stat. A nonblocking,
	// nofollow open never waits for a writer and must still match the stat.
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, inboxAttachmentLocalOpenError(err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close() //nolint:errcheck
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, err
	}
	if !sameInboxAttachmentFile(before, opened) {
		return nil, errInvalidStoredMediaKey
	}
	body, err := storage.ReadLimited(ctx, file, maxBytes)
	if err != nil {
		return nil, err
	}

	var fdAfter, pathAfter unix.Stat_t
	if err := unix.Fstat(fd, &fdAfter); err != nil {
		return nil, err
	}
	if err := unix.Fstatat(parent, name, &pathAfter, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if !sameInboxAttachmentFile(opened, fdAfter) || !sameInboxAttachmentFile(opened, pathAfter) ||
		int64(len(body)) != opened.Size {
		return nil, errInvalidStoredMediaKey
	}

	// Rejoin the entire retained chain after reading.
	for i, fd := range dirs[1:] {
		var pinned, current unix.Stat_t
		if err := unix.Fstat(fd, &pinned); err != nil {
			return nil, err
		}
		if err := unix.Fstatat(dirs[i], names[i], &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, err
		}
		if !sameInboxAttachmentDirectory(pinned, current) {
			return nil, errInvalidStoredMediaKey
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

func sameInboxAttachmentDirectory(a, b unix.Stat_t) bool {
	return b.Mode&unix.S_IFMT == unix.S_IFDIR && a.Dev == b.Dev && a.Ino == b.Ino
}

func inboxAttachmentRegularFile(info, anchor unix.Stat_t) bool {
	return info.Mode&unix.S_IFMT == unix.S_IFREG && info.Nlink == 1 && info.Dev == anchor.Dev && info.Size >= 0
}

func sameInboxAttachmentFile(a, b unix.Stat_t) bool {
	return inboxAttachmentRegularFile(b, a) && a.Ino == b.Ino && a.Size == b.Size &&
		a.Mtim == b.Mtim && a.Ctim == b.Ctim
}

func inboxAttachmentLocalOpenError(err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return errInvalidStoredMediaKey
	}
	return err
}
