//go:build linux

package mediaretirementcmd

import (
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

// This is irreversible for the short-lived maintenance process. Call it before
// accepting any owner credential. The ordinary application server never calls
// it. AllThreadsSyscall6 also covers existing Go runtime threads: NO_NEW_PRIVS
// is per-thread. cgo binaries refuse because Go cannot cover their native
// threads. The released standalone executable is built with CGO_ENABLED=0.
// No capability is granted and no other process is changed.
func protectMediaOwnerProcess() error {
	if _, _, err := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0); err != 0 {
		return mediaretirement.ErrIncomplete
	}
	if _, _, err := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, unix.PR_SET_DUMPABLE, 0, 0, 0, 0, 0); err != 0 {
		return mediaretirement.ErrIncomplete
	}
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || dumpable != 0 {
		return mediaretirement.ErrIncomplete
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || noNewPrivileges != 1 {
		return mediaretirement.ErrIncomplete
	}
	return nil
}
