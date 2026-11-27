//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/mediaretirementdb"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func mediaOwnerInstalledPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
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

func runMediaOwnerInstalled(t *testing.T, args []string, frame []byte, action string) (string, bool) {
	t.Helper()
	master, slave := mediaOwnerInstalledPTY(t)
	fd := int(master.Fd())
	require.NoError(t, unix.SetNonblock(fd, true))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/work/media-reference-inventory", args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + t.TempDir(), "LANG=C.UTF-8"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("synthetic maintenance child did not reap")
			}
		}
	})
	var output bytes.Buffer
	read := func() bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, 50)
		if err == unix.EINTR {
			return true
		}
		require.NoError(t, err)
		if fds[0].Revents&unix.POLLIN == 0 {
			return false
		}
		var chunk [1024]byte
		n, err := unix.Read(fd, chunk[:])
		if err == unix.EINTR || err == unix.EAGAIN {
			return true
		}
		require.NoError(t, err)
		require.Positive(t, n)
		require.LessOrEqual(t, output.Len()+n, 8192, "synthetic child output bound")
		_, _ = output.Write(chunk[:n])
		return true
	}
	const ready = "media-reference-inventory:owner-input-ready-v1\n"
	for !strings.Contains(output.String(), ready) {
		require.NoError(t, ctx.Err(), "synthetic child readiness deadline")
		read()
		select {
		case <-done:
			reaped = true
			t.Fatal("synthetic maintenance child exited before protected input")
		default:
		}
	}
	require.True(t, output.String() == ready, "only the fixed readiness marker may precede input")
	if action == "cancel" {
		require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	} else if action != "timeout" {
		for remaining := frame; len(remaining) > 0; {
			require.NoError(t, ctx.Err())
			n, err := unix.Write(fd, remaining)
			if err == unix.EINTR || err == unix.EAGAIN {
				time.Sleep(time.Millisecond)
				continue
			}
			require.NoError(t, err)
			require.Positive(t, n)
			remaining = remaining[n:]
		}
	}
	var result error
	for !reaped {
		require.NoError(t, ctx.Err(), "synthetic child completion deadline")
		read()
		select {
		case result = <-done:
			reaped = true
		default:
		}
	}
	for read() {
	}
	return strings.ReplaceAll(output.String(), "\r\n", "\n"), result == nil
}

func TestMediaInventoryPGExplicitOwnerStdinTLSAndRefusals(t *testing.T) {
	if os.Getenv("MEDIA_OWNER_TLS_PG_FIXTURE") != "1" {
		t.Skip("isolated owner-input TLS fixture not enabled")
	}
	ca, err := os.ReadFile("/tmp/pg-media-owner-tls/root.crt")
	require.NoError(t, err)
	wrongCA, err := os.ReadFile("/tmp/pg-media-owner-tls/other-root.crt")
	require.NoError(t, err)
	f := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var org string
	planPGOK(t, f.admin.QueryRow(ctx, "SELECT id::text FROM public.organizations WHERE deleted_at IS NULL AND reseller_id IS NOT NULL ORDER BY id LIMIT 1").Scan(&org), "select synthetic organization")
	account := uuid.New()
	identityInventoryExec(t, f, "INSERT INTO public.whatsapp_accounts(id,created_at,updated_at,organization_id,name,phone_id,business_id,access_token,is_smb,status) VALUES($1,now(),now(),$2::uuid,'owner-input-fixture','synthetic-phone','synthetic-business','DO_NOT_TRANSFER',true,'active')", account, org)
	shadow := uuid.New()
	identityInventoryExec(t, f, "INSERT INTO public.channel_accounts(id,created_at,updated_at,organization_id,channel,provider,name,external_account_id,status,metadata) VALUES($1,now(),now(),$2::uuid,'whatsapp',$3,'synthetic-legacy-shadow','legacy-account:'||$4::text,'active',jsonb_build_object('legacy_account_id',$4::text))", shadow, org, channelapi.LegacyMetaProvider, account.String())
	ownerURL, err := url.Parse(f.cfg.MigrationURL)
	require.NoError(t, err)
	runtimeURL, err := url.Parse(f.cfg.URL)
	require.NoError(t, err)
	ownerPassword, ok := ownerURL.User.Password()
	require.True(t, ok)
	runtimePassword, ok := runtimeURL.User.Password()
	require.True(t, ok)
	for _, scenario := range []string{"owner", "runtime-role", "wrong-password", "wrong-ca", "wrong-hostname", "plaintext", "missing-rls", "cancellation", "input-timeout", "nonlegacy-meta-provider", "owner-after-refusals"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			outDir := filepath.Join(base, "output")
			require.NoError(t, os.Mkdir(outDir, 0700))
			outputPath := filepath.Join(outDir, "manifest.json")
			configured := *runtimeURL
			if scenario == "wrong-hostname" {
				configured.Host = "localhost:" + configured.Port()
			}
			if scenario == "plaintext" {
				q := configured.Query()
				q.Set("sslmode", "disable")
				configured.RawQuery = q.Encode()
			}
			configBytes := []byte(fmt.Sprintf("[app]\nenvironment=\"test\"\n[database]\nurl=%q\nmigration_url=\"postgres://forbidden:unused@other.invalid/unused\"\n", configured.String()))
			configPath := filepath.Join(base, "config.toml")
			require.NoError(t, os.WriteFile(configPath, configBytes, 0600))
			if scenario == "nonlegacy-meta-provider" {
				identityInventoryExec(t, f, "UPDATE public.channel_accounts SET provider='meta' WHERE id=$1", shadow)
				t.Cleanup(func() {
					identityInventoryExec(t, f, "UPDATE public.channel_accounts SET provider=$1 WHERE id=$2", channelapi.LegacyMetaProvider, shadow)
				})
			}
			username, password, trust := f.owner, ownerPassword, string(ca)
			if scenario == "runtime-role" {
				username, password = f.runtime, runtimePassword
			}
			if scenario == "wrong-password" {
				password = "synthetic-wrong-password"
			}
			if scenario == "wrong-ca" {
				trust = string(wrongCA)
			}
			if scenario == "missing-rls" {
				identityInventoryExec(t, f, "ALTER TABLE public.messages DISABLE ROW LEVEL SECURITY")
				defer identityInventoryExec(t, f, "ALTER TABLE public.messages ENABLE ROW LEVEL SECURITY; ALTER TABLE public.messages FORCE ROW LEVEL SECURITY")
			}
			frame, err := json.Marshal(map[string]any{"version": 1, "username": username, "password": password, "ca_pem": trust})
			require.NoError(t, err)
			frame = append(frame, '\n')
			defer clear(frame)
			args := []string{"-config", configPath, "-organization-id", org, "-account-id", account.String(), "-output", outputPath, "-owner-stdin"}
			action := "send"
			if scenario == "cancellation" {
				action = "cancel"
			}
			if scenario == "input-timeout" {
				action = "timeout"
			}
			transcript, success := runMediaOwnerInstalled(t, args, frame, action)
			for _, private := range []string{username, password, trust, org, account.String(), outputPath, "DO_NOT_TRANSFER"} {
				require.False(t, strings.Contains(transcript, private), "terminal must not contain synthetic private values")
			}
			wantSuccess := scenario == "owner" || scenario == "owner-after-refusals"
			require.Equal(t, wantSuccess, success, "synthetic maintenance exit status")
			ending := "media-reference-inventory:capture-failed\n"
			if wantSuccess {
				ending = "media-reference-inventory:saved-private-manifest\n"
			}
			require.True(t, transcript == "media-reference-inventory:owner-input-ready-v1\n"+ending, "terminal output must contain only fixed markers")
			after, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.True(t, bytes.Equal(configBytes, after), "runtime configuration must remain unchanged")
			entries, err := os.ReadDir(outDir)
			require.NoError(t, err)
			if wantSuccess {
				require.Len(t, entries, 1)
				info, err := os.Stat(outputPath)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0600), info.Mode().Perm())
				raw, err := os.ReadFile(outputPath)
				require.NoError(t, err)
				var report struct {
					Version    int      `json:"version"`
					Disposed   bool     `json:"session_disposed"`
					Snapshot   bool     `json:"same_snapshot"`
					Historical bool     `json:"historical_attribution_complete"`
					Families   []string `json:"complete_families"`
				}
				require.NoError(t, json.Unmarshal(raw, &report))
				require.Equal(t, 2, report.Version)
				require.True(t, report.Disposed && report.Snapshot)
				require.False(t, report.Historical)
				require.Len(t, report.Families, 9)
				for _, private := range []string{password, trust, "DO_NOT_TRANSFER"} {
					require.False(t, bytes.Contains(raw, []byte(private)))
				}
			} else {
				require.Empty(t, entries, "refusals leave no final or temporary inventory")
			}
			var remaining int
			planPGOK(t, f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=$1 AND application_name=$2", f.database, mediaretirementdb.MediaInventoryApplication).Scan(&remaining), "observe child connection disposal")
			require.Zero(t, remaining)
		})
	}
}
