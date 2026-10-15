package calling

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v4"
	"github.com/shridarpatil/whatomate/internal/callingaudio"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
)

type callingAudioObjects struct {
	data    map[string][]byte
	keys    []string
	failure error
}

func (s *callingAudioObjects) Put(_ context.Context, key string, data []byte, _ string) error {
	if s.data == nil {
		s.data = map[string][]byte{}
	}
	s.data[key] = append([]byte(nil), data...)
	return nil
}
func (s *callingAudioObjects) Get(_ context.Context, key string) ([]byte, string, error) {
	s.keys = append(s.keys, key)
	if s.failure != nil {
		return nil, "", s.failure
	}
	data, ok := s.data[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return append([]byte(nil), data...), "audio/ogg", nil
}
func (*callingAudioObjects) Delete(context.Context, string) error { panic("unexpected delete") }
func (*callingAudioObjects) ListPrefix(context.Context, string) ([]storage.ObjectInfo, error) {
	panic("unexpected list")
}

// Two header pages and two minimal Opus silence packets exercise the real
// player without ffmpeg, credentials, a database, or a remote peer.
func callingAudioFixture() []byte {
	var data []byte
	for _, payload := range [][]byte{[]byte("OpusHead"), []byte("OpusTags"), {0xf8, 0xff, 0xfe}, {0xf8, 0xff, 0xfe}} {
		header := make([]byte, 27)
		copy(header, "OggS")
		header[26] = 1
		data = append(data, header...)
		data = append(data, byte(len(payload)))
		data = append(data, payload...)
	}
	return data
}

func TestAllAudioIVRNodesPlayFromColdTenantObjectStorage(t *testing.T) {
	for _, kind := range []IVRNodeType{IVRNodeGreeting, IVRNodeMenu, IVRNodeGather, IVRNodeHangup} {
		t.Run(string(kind), func(t *testing.T) {
			objects := &callingAudioObjects{}
			org := uuid.New()
			name, err := callingaudio.New(t.TempDir(), objects).Save(context.Background(), org, callingAudioFixture())
			require.NoError(t, err)
			manager := &Manager{audioStore: callingaudio.New(t.TempDir(), objects), log: testutil.NopLogger(), config: &config.CallingConfig{}}
			session := &CallSession{OrganizationID: org, ID: "synthetic-call", DTMFBuffer: make(chan byte, 1)}
			node := &IVRNode{ID: "synthetic-node", Type: kind, Config: map[string]any{"audio_file": name, "timeout_seconds": 0, "max_retries": 1}}
			ctx := &IVRContext{Variables: map[string]string{}}
			track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "test")
			require.NoError(t, err)
			player := NewAudioPlayer(track)
			player.SetSequence(20, 2000)
			switch kind {
			case IVRNodeGreeting:
				require.Equal(t, "default", manager.executeGreeting(session, node, player))
			case IVRNodeMenu:
				require.Equal(t, "max_retries", manager.executeMenu(session, node, ctx, player))
			case IVRNodeGather:
				require.Equal(t, "max_retries", manager.executeGather(session, node, ctx, player))
			case IVRNodeHangup:
				manager.executeHangup(session, node, ctx, nil, player)
			}
			seq, timestamp := player.Sequence()
			require.EqualValues(t, 23, seq, "existing RTP sequence advances for both downloaded frames")
			require.EqualValues(t, 4880, timestamp)
			require.Equal(t, []string{"organizations/" + org.String() + "/calling/audio/" + name}, objects.keys)
		})
	}
}

func TestOrgHoldAndRingbackResolveColdTenantOverrides(t *testing.T) {
	objects := &callingAudioObjects{}
	org := uuid.New()
	name, err := callingaudio.New(t.TempDir(), objects).Save(context.Background(), org, callingAudioFixture())
	require.NoError(t, err)
	manager := &Manager{audioStore: callingaudio.New(t.TempDir(), objects), log: testutil.NopLogger(), config: &config.CallingConfig{AudioDir: "/server-defaults"}}
	settings := orgCallingSettings{HoldMusicFile: "/server-defaults/hold.ogg", RingbackFile: "/server-defaults/ring.ogg"}
	manager.applyOrgOverrides(&settings, map[string]any{"hold_music_file": name, "ringback_file": name})
	manager.resolveOrgAudio(org, &settings)
	require.NotEmpty(t, settings.HoldMusicFile)
	require.Equal(t, settings.HoldMusicFile, settings.RingbackFile)
	require.Contains(t, settings.HoldMusicFile, org.String())
	require.Equal(t, settings.RingbackFile, settings.transferRingFile())
	data, err := os.ReadFile(settings.HoldMusicFile)
	require.NoError(t, err)
	require.Equal(t, callingAudioFixture(), data)
	require.Len(t, objects.keys, 1)
}

func TestOrgAudioFailureDoesNotSelectGlobalOrDifferentHoldAudio(t *testing.T) {
	objects := &callingAudioObjects{failure: errors.New("storage authentication denied")}
	manager := &Manager{audioStore: callingaudio.New(t.TempDir(), objects), log: testutil.NopLogger(), config: &config.CallingConfig{AudioDir: "/server-defaults"}}
	settings := orgCallingSettings{HoldMusicFile: "/server-defaults/hold.ogg", RingbackFile: "/server-defaults/ring.ogg"}
	manager.applyOrgOverrides(&settings, map[string]any{"ringback_file": "tenant-ring.ogg"})
	manager.resolveOrgAudio(uuid.New(), &settings)
	require.Empty(t, settings.RingbackFile)
	require.Empty(t, settings.transferRingFile(), "a failed configured ringback must not silently select hold/default audio")
	require.Equal(t, "/server-defaults/hold.ogg", settings.HoldMusicFile)
	defaults := orgCallingSettings{HoldMusicFile: "/server-defaults/hold.ogg", RingbackFile: "/server-defaults/ring.ogg"}
	manager.resolveOrgAudio(uuid.New(), &defaults)
	require.Equal(t, "/server-defaults/hold.ogg", defaults.HoldMusicFile)
	require.Equal(t, "/server-defaults/ring.ogg", defaults.RingbackFile)
	require.Len(t, objects.keys, 1, "server defaults do not enter tenant object resolution")
}

func TestNodeAudioCannotResolveOtherTenantOrGlobalPaths(t *testing.T) {
	objects := &callingAudioObjects{}
	owner := uuid.New()
	other := uuid.New()
	name, err := callingaudio.New(t.TempDir(), objects).Save(context.Background(), owner, callingAudioFixture())
	require.NoError(t, err)
	manager := &Manager{audioStore: callingaudio.New(t.TempDir(), objects), log: testutil.NopLogger()}
	session := &CallSession{OrganizationID: other}
	for _, reference := range []string{name, "../" + owner.String() + "/" + name, "/server-defaults/hold.ogg"} {
		path := manager.resolveNodeAudio(session, &IVRNode{Config: map[string]any{"audio_file": reference}})
		require.Empty(t, path)
	}
	require.Len(t, objects.keys, 1)
	require.True(t, strings.HasPrefix(objects.keys[0], "organizations/"+other.String()+"/"))
}
