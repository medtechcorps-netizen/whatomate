package calling

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/callingaudio"
)

// SetAudioStore supplies the same tenant audio store used by uploads and TTS.
// It must be called during startup before any call sessions are accepted.
func (m *Manager) SetAudioStore(store *callingaudio.Store) {
	m.audioStore = store
}

func (m *Manager) resolveCallingAudio(orgID uuid.UUID, filename string) (string, error) {
	if m.audioStore == nil {
		return "", errors.New("calling audio store is not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return m.audioStore.Resolve(ctx, orgID, filename)
}

// resolveNodeAudio changes only the source of the local playback file. RTP
// sequencing, pacing and DTMF interruption stay in the existing AudioPlayer.
func (m *Manager) resolveNodeAudio(session *CallSession, node *IVRNode) string {
	filename, _ := node.Config["audio_file"].(string)
	if filename == "" {
		return ""
	}
	path, err := m.resolveCallingAudio(session.OrganizationID, filename)
	if err != nil {
		m.log.Error("Failed to resolve IVR audio", "error", err, "call_id", session.ID, "node_id", node.ID)
		return ""
	}
	return path
}

// Tenant overrides never fall back to global files after a storage failure.
// Server-configured defaults are already resolved locally when no override
// exists; they are not references a tenant can choose through an IVR graph.
func (m *Manager) resolveOrgAudio(orgID uuid.UUID, settings *orgCallingSettings) {
	if settings.holdMusicReference != "" {
		path, err := m.resolveCallingAudio(orgID, settings.holdMusicReference)
		settings.HoldMusicFile = path
		if err != nil {
			m.log.Error("Failed to resolve organization hold audio", "error", err, "org_id", orgID)
		}
	}
	if settings.ringbackReference != "" {
		path, err := m.resolveCallingAudio(orgID, settings.ringbackReference)
		settings.RingbackFile = path
		settings.ringbackUnavailable = err != nil
		if err != nil {
			m.log.Error("Failed to resolve organization ringback audio", "error", err, "org_id", orgID)
		}
	}
}
