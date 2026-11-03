//go:build linux

package mediaretirementcmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Every value sent by these tests is synthetic. Open an isolated PTY directly
// so the tests never change the test runner's terminal or require local Docker.
func mediaOwnerTestPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	require.NoError(t, err)
	master := os.NewFile(uintptr(fd), "synthetic-owner-master")
	t.Cleanup(func() { _ = master.Close() })
	require.NoError(t, unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0))
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	require.NoError(t, err)
	sfd, err := unix.Open(fmt.Sprintf("/dev/pts/%d", number), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	slave := os.NewFile(uintptr(sfd), "synthetic-owner-slave")
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func mediaOwnerTestReady(t *testing.T, fd int, ctx context.Context) {
	t.Helper()
	var received []byte
	var chunk [256]byte
	for !bytes.Contains(received, []byte("\n")) {
		require.True(t, mediaOwnerPoll(ctx, fd, unix.POLLIN))
		n, err := unix.Read(fd, chunk[:])
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		require.NoError(t, err)
		require.Positive(t, n)
		received = append(received, chunk[:n]...)
		require.LessOrEqual(t, len(received), len(mediaOwnerReady))
	}
	require.Equal(t, mediaOwnerReady, string(received))
}

func TestMediaOwnerInputTerminalFramingAndRestoration(t *testing.T) {
	valid := `{"version":1,"username":"synthetic_owner","password":"synthetic_password","ca_pem":"synthetic-pem-placeholder"}`
	for _, tc := range []struct {
		name, input string
		success     bool
	}{
		{"valid", valid + "\n", true},
		{"malformed", "synthetic-not-json\n", false},
		{"duplicate", `{"version":1,"version":1}` + "\n", false},
		{"empty", "\n", false},
		{"two-frames", valid + "\n" + valid + "\n", false},
		{"control-byte", valid + "\x03\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := mediaOwnerTestPTY(t)
			mfd, sfd := int(master.Fd()), int(slave.Fd())
			require.NoError(t, unix.SetNonblock(mfd, true))
			before, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
			require.NoError(t, err)
			flags, err := unix.FcntlInt(uintptr(sfd), unix.F_GETFL, 0)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			type result struct {
				raw []byte
				err error
			}
			done := make(chan result, 1)
			go func() { raw, err := captureMediaOwnerTerminal(ctx, slave); done <- result{raw, err} }()
			mediaOwnerTestReady(t, mfd, ctx)
			during, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
			require.NoError(t, err)
			require.Zero(t, during.Lflag&(unix.ECHO|unix.ECHONL|unix.ICANON|unix.ISIG))
			n, err := unix.Write(mfd, []byte(tc.input))
			require.NoError(t, err)
			require.Equal(t, len(tc.input), n)
			var r result
			select {
			case r = <-done:
			case <-ctx.Done():
				t.Fatal("terminal reader did not finish")
			}
			if tc.success {
				require.NoError(t, r.err)
				require.Equal(t, valid, string(r.raw))
			} else {
				require.Error(t, r.err)
				require.Nil(t, r.raw)
			}
			clear(r.raw)
			after, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterFlags, err := unix.FcntlInt(uintptr(sfd), unix.F_GETFL, 0)
			require.NoError(t, err)
			require.Equal(t, flags, afterFlags)
			fds := []unix.PollFd{{Fd: int32(mfd), Events: unix.POLLIN}}
			n, err = unix.Poll(fds, 50)
			require.NoError(t, err)
			require.Zero(t, n, "input must never be echoed")
		})
	}
}

func TestMediaOwnerInputTerminalBoundedFailures(t *testing.T) {
	for _, kind := range []string{"canceled", "deadline", "oversize", "hangup"} {
		t.Run(kind, func(t *testing.T) {
			master, slave := mediaOwnerTestPTY(t)
			mfd, sfd := int(master.Fd()), int(slave.Fd())
			require.NoError(t, unix.SetNonblock(mfd, true))
			before, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				raw, err := captureMediaOwnerTerminal(ctx, slave)
				if len(raw) != 0 {
					err = fmt.Errorf("unexpected synthetic input accepted")
				}
				done <- err
			}()
			mediaOwnerTestReady(t, mfd, ctx)
			start := time.Now()
			switch kind {
			case "canceled":
				cancel()
			case "hangup":
				require.NoError(t, master.Close())
			case "oversize":
				payload := []byte(strings.Repeat("x", mediaOwnerInputByteLimit+2))
				for len(payload) > 0 {
					require.True(t, mediaOwnerPoll(ctx, mfd, unix.POLLOUT))
					n, err := unix.Write(mfd, payload)
					if err == unix.EAGAIN || err == unix.EINTR {
						continue
					}
					require.NoError(t, err)
					require.Positive(t, n)
					payload = payload[n:]
				}
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("terminal refusal was not bounded")
			}
			require.Less(t, time.Since(start), time.Second)
			if kind != "hangup" {
				after, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
		})
	}
}

func TestMediaOwnerInputTerminalRejectsNonTerminalAndUnboundedContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	defer w.Close()
	file, err := os.CreateTemp(t.TempDir(), "synthetic-input")
	require.NoError(t, err)
	defer file.Close()
	for _, f := range []*os.File{nil, r, w, file} {
		raw, err := captureMediaOwnerTerminal(ctx, f)
		require.Error(t, err)
		require.Nil(t, raw)
	}
	_, slave := mediaOwnerTestPTY(t)
	for _, c := range []context.Context{nil, context.Background()} {
		raw, err := captureMediaOwnerTerminal(c, slave)
		require.Error(t, err)
		require.Nil(t, raw)
	}
	cancel()
	raw, err := captureMediaOwnerTerminal(ctx, slave)
	require.Error(t, err)
	require.Nil(t, raw)
}
