//go:build linux

package mediaretirementcmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMediaOwnerInputProcessProtection(t *testing.T) {
	if os.Getenv("REREPLY_SYNTHETIC_OWNER_PROCESS_CHILD") == "1" {
		if mediaOwnerTestCGO {
			require.Error(t, protectMediaOwnerProcess())
			return
		}
		// Keep multiple existing OS threads alive across protection, then create
		// more. Both must have the same restrictions before credentials arrive.
		var ready, done sync.WaitGroup
		gate := make(chan struct{})
		results := make(chan bool, 12)
		check := func(wait bool) {
			defer done.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if wait {
				ready.Done()
				<-gate
			}
			d, de := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
			n, ne := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
			results <- de == nil && ne == nil && d == 0 && n == 1
		}
		ready.Add(6)
		done.Add(6)
		for i := 0; i < 6; i++ {
			go check(true)
		}
		ready.Wait()
		require.NoError(t, protectMediaOwnerProcess())
		require.NoError(t, protectMediaOwnerProcess())
		done.Add(6)
		for i := 0; i < 6; i++ {
			go check(false)
		}
		close(gate)
		done.Wait()
		close(results)
		for protected := range results {
			require.True(t, protected)
		}
		dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Zero(t, dumpable)
		noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Equal(t, 1, noNewPrivileges)
		return
	}
	before, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	require.NoError(t, err)
	beforePrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	require.NoError(t, err)
	self, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestMediaOwnerInputProcessProtection$")
	cmd.Env = append(os.Environ(), "REREPLY_SYNTHETIC_OWNER_PROCESS_CHILD=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Run(), "synthetic child must finish with its process guards applied")
	require.NoError(t, ctx.Err())
	after, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	require.NoError(t, err)
	require.Equal(t, beforePrivileges, afterPrivileges)
}
