//go:build linux

package mediaretirementcmd

import (
	"bytes"
	"context"
	"os"
	"time"

	"github.com/shridarpatil/whatomate/internal/mediaretirement"
	"golang.org/x/sys/unix"
)

const mediaOwnerReady = "media-reference-inventory:owner-input-ready-v1\n"

// readMediaOwnerInput is reserved for the short-lived standalone maintenance
// command's explicit owner-stdin mode. It must run before reading credentials.
// The ordinary server and the default inventory connection never call it.
func readMediaOwnerInput(parent context.Context, terminal *os.File) ([]byte, error) {
	if parent == nil || parent.Err() != nil || terminal == nil {
		return nil, mediaretirement.ErrIncomplete
	}
	if _, ok := parent.Deadline(); !ok {
		return nil, mediaretirement.ErrIncomplete
	}
	if protectMediaOwnerProcess() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	return captureMediaOwnerTerminal(parent, terminal)
}

// captureMediaOwnerTerminal consumes one newline-framed JSON envelope after a
// fixed readiness marker. The caller must wait for that marker before sending
// input. No credential is accepted through an argument, environment or file.
// The terminal is owned exclusively by this command for the duration. A pipe,
// redirected file, read-only terminal, or missing deadline is refused.
func captureMediaOwnerTerminal(parent context.Context, terminal *os.File) (raw []byte, err error) {
	if parent == nil || parent.Err() != nil || terminal == nil {
		return nil, mediaretirement.ErrIncomplete
	}
	if _, ok := parent.Deadline(); !ok {
		return nil, mediaretirement.ErrIncomplete
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	fd := int(terminal.Fd())
	if fd < 0 || int64(fd) > 1<<31-1 {
		return nil, mediaretirement.ErrIncomplete
	}
	var st unix.Stat_t
	flags, flagErr := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	before, termErr := unix.IoctlGetTermios(fd, unix.TCGETS)
	if flagErr != nil || termErr != nil || unix.Fstat(fd, &st) != nil ||
		st.Mode&unix.S_IFMT != unix.S_IFCHR || flags&unix.O_ACCMODE != unix.O_RDWR {
		return nil, mediaretirement.ErrIncomplete
	}
	quiet := *before
	quiet.Lflag &^= unix.ECHO | unix.ECHONL | unix.ECHOE | unix.ECHOK | unix.ECHOCTL | unix.ECHOPRT | unix.ECHOKE | unix.ICANON | unix.ISIG | unix.IEXTEN
	quiet.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	quiet.Oflag &^= unix.OPOST
	quiet.Cflag &^= unix.CSIZE | unix.PARENB
	quiet.Cflag |= unix.CS8
	quiet.Cc[unix.VMIN], quiet.Cc[unix.VTIME] = 1, 0
	// Register restoration before the first mutation. Flush queued input while
	// echo is still off, including on cancellation, malformed data or overflow.
	defer func() {
		flushErr := unix.IoctlSetTermios(fd, unix.TCSETSF, &quiet)
		restoreErr := unix.IoctlSetTermios(fd, unix.TCSETS, before)
		_, flagsErr := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
		if flushErr != nil || restoreErr != nil || flagsErr != nil {
			clear(raw)
			raw, err = nil, mediaretirement.ErrIncomplete
		}
	}()
	// TCSETSF applies the quiet settings and discards pending input. Some
	// hosted PTYs support this operation but reject the separate TCFLSH ioctl.
	// Both entry and cleanup still require a successful input flush.
	if unix.IoctlSetTermios(fd, unix.TCSETSF, &quiet) != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	if _, e := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags|unix.O_NONBLOCK); e != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	current, e := unix.IoctlGetTermios(fd, unix.TCGETS)
	if e != nil || current.Lflag&(unix.ECHO|unix.ECHONL|unix.ICANON|unix.ISIG) != 0 {
		return nil, mediaretirement.ErrIncomplete
	}
	ready := []byte(mediaOwnerReady)
	for len(ready) > 0 {
		if !mediaOwnerPoll(ctx, fd, unix.POLLOUT) {
			return nil, mediaretirement.ErrIncomplete
		}
		n, e := unix.Write(fd, ready)
		if e == unix.EAGAIN || e == unix.EINTR {
			continue
		}
		if e != nil || n <= 0 {
			return nil, mediaretirement.ErrIncomplete
		}
		ready = ready[n:]
	}
	buffer := make([]byte, 0, mediaOwnerInputByteLimit+1)
	defer func() {
		if err != nil {
			clear(buffer)
		}
	}()
	var chunk [1024]byte
	defer clear(chunk[:])
	for {
		if !mediaOwnerPoll(ctx, fd, unix.POLLIN) {
			return nil, mediaretirement.ErrIncomplete
		}
		n, e := unix.Read(fd, chunk[:])
		if e == unix.EAGAIN || e == unix.EINTR {
			continue
		}
		if e != nil || n <= 0 {
			return nil, mediaretirement.ErrIncomplete
		}
		if len(buffer)+n > mediaOwnerInputByteLimit+1 {
			return nil, mediaretirement.ErrIncomplete
		}
		buffer = append(buffer, chunk[:n]...)
		if end := bytes.IndexByte(buffer, '\n'); end >= 0 {
			if end == 0 || end != len(buffer)-1 || ctx.Err() != nil {
				return nil, mediaretirement.ErrIncomplete
			}
			// Validate inside the protected capture window; raw errors and values
			// never reach the terminal. The caller clears the returned buffer.
			if _, e := decodeMediaOwnerCredentials(buffer[:end]); e != nil {
				return nil, mediaretirement.ErrIncomplete
			}
			return buffer[:end], nil
		}
	}
}

func mediaOwnerPoll(ctx context.Context, fd int, events int16) bool {
	for ctx.Err() == nil {
		fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(fds, 50)
		if err == unix.EINTR {
			continue
		}
		if err != nil || fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return false
		}
		if n > 0 && fds[0].Revents&events != 0 {
			return ctx.Err() == nil
		}
	}
	return false
}
