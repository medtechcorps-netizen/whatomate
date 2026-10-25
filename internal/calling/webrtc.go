package calling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	cloudflareTURNURL        = "turns:turn.cloudflare.com:443?transport=tcp"
	cloudflareCredentialTTL  = 24 * time.Hour
	cloudflareCacheLifetime  = time.Hour
	cloudflareRequestTimeout = 5 * time.Second
	cloudflareResponseLimit  = 64 * 1024
)

// ICEServer contains only the short-lived client credentials, never the token
// used to obtain them. Static and coturn credentials retain their existing form.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type ICEConfiguration struct {
	Servers         []ICEServer `json:"ice_servers"`
	ExpiresAt       *time.Time  `json:"expires_at,omitempty"`
	TransportPolicy string      `json:"ice_transport_policy"`
}

type turnBundle struct {
	server    ICEServer
	issuedAt  time.Time
	expiresAt time.Time
}

type turnRefresh struct {
	done   chan struct{}
	bundle turnBundle
	err    error
}

// cloudflareTURNProvider shares one refresh between concurrent new calls. It
// does not change already-running peer connections or persist credentials.
type cloudflareTURNProvider struct {
	config  config.CloudflareTURNConfig
	client  *http.Client
	now     func() time.Time
	mu      sync.Mutex
	cached  turnBundle
	refresh *turnRefresh
}

func newCloudflareTURNProvider(cfg config.CloudflareTURNConfig, client *http.Client) *cloudflareTURNProvider {
	var bounded *http.Client
	if client != nil {
		// Reuse the application's SSRF-safe transport, but not cookies, redirect
		// behavior, or its longer timeout. The endpoint is never configurable.
		bounded = &http.Client{
			Transport:     client.Transport,
			Timeout:       cloudflareRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &cloudflareTURNProvider{config: cfg, client: bounded, now: time.Now}
}

func (p *cloudflareTURNProvider) resolve(ctx context.Context) (turnBundle, error) {
	if err := ctx.Err(); err != nil {
		return turnBundle{}, err
	}
	p.mu.Lock()
	now := p.now()
	if !p.cached.issuedAt.IsZero() && !now.Before(p.cached.issuedAt) && now.Before(p.cached.issuedAt.Add(cloudflareCacheLifetime)) && now.Before(p.cached.expiresAt) {
		bundle := cloneTurnBundle(p.cached)
		p.mu.Unlock()
		return bundle, nil
	}
	pending := p.refresh
	if pending == nil {
		pending = &turnRefresh{done: make(chan struct{})}
		p.refresh = pending
		// A canceled caller must not cancel credentials needed by another call.
		// The refresh owns its five-second deadline; each caller can stop waiting.
		go p.completeRefresh(pending)
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return turnBundle{}, ctx.Err()
	case <-pending.done:
		if err := ctx.Err(); err != nil {
			return turnBundle{}, err
		}
		return cloneTurnBundle(pending.bundle), pending.err
	}
}

func (p *cloudflareTURNProvider) completeRefresh(pending *turnRefresh) {
	bundle, err := p.fetch(context.Background())
	p.mu.Lock()
	if err == nil {
		p.cached = bundle
	}
	pending.bundle, pending.err = bundle, err
	p.refresh = nil
	close(pending.done)
	p.mu.Unlock()
}

func cloneTurnBundle(bundle turnBundle) turnBundle {
	bundle.server.URLs = append([]string(nil), bundle.server.URLs...)
	return bundle
}

func (p *cloudflareTURNProvider) fetch(ctx context.Context) (turnBundle, error) {
	if err := p.config.Validate(); err != nil {
		return turnBundle{}, err
	}
	if p.client == nil {
		return turnBundle{}, errors.New("TURN credential HTTP client is unavailable")
	}
	issued := p.now()
	ctx, cancel := context.WithTimeout(ctx, cloudflareRequestTimeout)
	defer cancel()
	endpoint := "https://rtc.live.cloudflare.com/v1/turn/keys/" + p.config.KeyID + "/credentials/generate-ice-servers"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(`{"ttl":86400}`))
	if err != nil {
		return turnBundle{}, errors.New("TURN credential request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+p.config.APIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ReReply-TURN/1.0")
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return turnBundle{}, ctx.Err()
		}
		// Transport errors may contain URLs or request details. Never propagate
		// them to the call log, public error envelope, or application logger.
		return turnBundle{}, errors.New("TURN credential service request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return turnBundle{}, errors.New("TURN credential service did not create credentials")
	}
	if strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		return turnBundle{}, errors.New("TURN credential service returned an invalid content type")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, cloudflareResponseLimit+1))
	if err != nil || len(raw) > cloudflareResponseLimit {
		return turnBundle{}, errors.New("TURN credential service returned an invalid response size")
	}
	var payload struct {
		Servers []ICEServer `json:"iceServers"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return turnBundle{}, errors.New("TURN credential service returned invalid JSON")
	}
	var selected *ICEServer
	for _, server := range payload.Servers {
		for _, address := range server.URLs {
			if address != cloudflareTURNURL {
				continue
			}
			if selected != nil || !validTURNCredential(server.Username) || !validTURNCredential(server.Credential) {
				return turnBundle{}, errors.New("TURN credential service returned invalid credentials")
			}
			selected = &ICEServer{URLs: []string{cloudflareTURNURL}, Username: server.Username, Credential: server.Credential}
		}
	}
	if selected == nil {
		return turnBundle{}, errors.New("TURN credential service omitted the required relay")
	}
	if err := ctx.Err(); err != nil {
		return turnBundle{}, err
	}
	return turnBundle{server: *selected, issuedAt: issued, expiresAt: issued.Add(cloudflareCredentialTTL)}, nil
}

func validTURNCredential(value string) bool {
	if len(value) < 1 || len(value) > 4096 {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}

// ResolveICEConfiguration is shared by Pion and the authenticated browser
// endpoint. A fresh bundle is required for every new connection; existing calls
// are not renegotiated when a cached bundle is replaced.
func (m *Manager) ResolveICEConfiguration(ctx context.Context) (ICEConfiguration, error) {
	if err := ctx.Err(); err != nil {
		return ICEConfiguration{}, err
	}
	if m.config == nil {
		return ICEConfiguration{}, errors.New("calling is not configured")
	}
	result := ICEConfiguration{Servers: make([]ICEServer, 0), TransportPolicy: "all"}
	if m.config.RelayOnly {
		result.TransportPolicy = "relay"
	}
	if m.config.CloudflareTURN.Configured() {
		m.iceProviderOnce.Do(func() { m.iceProvider = newCloudflareTURNProvider(m.config.CloudflareTURN, m.httpClient) })
		bundle, err := m.iceProvider.resolve(ctx)
		if err != nil {
			return ICEConfiguration{}, err
		}
		result.Servers = append(result.Servers, bundle.server)
		result.ExpiresAt = &bundle.expiresAt
		return result, nil
	}
	now := time.Now()
	for _, server := range m.config.ICEServers {
		username, credential := server.ResolveCredentials(now)
		result.Servers = append(result.Servers, ICEServer{URLs: append([]string(nil), server.URLs...), Username: username, Credential: credential})
	}
	return result, nil
}

// negotiateWebRTC handles the SDP exchange and sets up WebRTC media.
//
// Per the WhatsApp Business Calling API (user-initiated calls):
//  1. Webhook "connect" delivers the consumer's SDP offer (in session.sdp)
//  2. Business creates a PeerConnection and sets the offer as remote description
//  3. Business creates an SDP answer
//  4. Business sends pre_accept with session: { sdp_type: "answer", sdp: "<SDP>" }
//  5. Business sends accept with the same session object
//  6. WebRTC media flows
func (m *Manager) negotiateWebRTC(session *CallSession, account *models.WhatsAppAccount, sdpOffer string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	waAccount := account.ToWAAccount()

	// Create peer connection with Opus codec
	pc, err := m.createPeerConnection(ctx)
	if err != nil {
		m.log.Error("Failed to create peer connection", "error", err, "call_id", session.ID)
		m.rejectCall(ctx, session, account)
		return
	}

	session.mu.Lock()
	session.PeerConnection = pc
	session.mu.Unlock()

	// Add local audio track for IVR playback / server→caller audio
	audioTrack, err := createOpusTrack(pc, "ivr-audio")
	if err != nil {
		m.log.Error("Failed to create audio track", "error", err)
		m.rejectCall(ctx, session, account)
		return
	}

	session.mu.Lock()
	session.AudioTrack = audioTrack
	session.mu.Unlock()

	// Register handler for incoming audio (caller's voice + DTMF)
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		m.log.Info("Received remote track",
			"call_id", session.ID,
			"codec", track.Codec().MimeType,
			"payload_type", track.PayloadType(),
		)

		// Check if this is a dedicated telephone-event track (DTMF)
		if track.Codec().MimeType == "audio/telephone-event" {
			go m.handleDTMFTrack(session, track)
			return
		}

		// Store the caller's remote track for potential audio bridge use
		session.mu.Lock()
		session.CallerRemoteTrack = track
		bridge := session.Bridge
		agentLocal := session.AgentAudioTrack
		session.mu.Unlock()

		// On incoming calls the caller's media often starts only after the agent
		// answers, so the transfer bridge may already be running without the
		// caller track (one-way audio). Wire the track into the live bridge now
		// so the agent can hear the caller; the bridge becomes the sole reader.
		if bridge != nil && agentLocal != nil {
			bridge.AttachCaller(track, agentLocal)
			return
		}

		// Consume audio and detect inline DTMF (telephone-event packets
		// arrive on the same m-line as audio with a different payload type).
		go m.consumeAudioWithDTMF(session, track)
	})

	// Channel to signal when the WebRTC connection is established
	connected := make(chan struct{})

	// Handle connection state changes
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		m.log.Info("Peer connection state changed",
			"call_id", session.ID,
			"state", state.String(),
		)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-connected:
			default:
				close(connected)
			}
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			m.EndCall(session.ID)
		}
	})

	// Step 1: Set the consumer's SDP offer as remote description
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}); err != nil {
		m.log.Error("Failed to set remote description (consumer offer)", "error", err, "call_id", session.ID)
		m.rejectCall(ctx, session, account)
		return
	}

	// Step 2: Create SDP answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		m.log.Error("Failed to create SDP answer", "error", err, "call_id", session.ID)
		m.rejectCall(ctx, session, account)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		m.log.Error("Failed to set local description (answer)", "error", err, "call_id", session.ID)
		m.rejectCall(ctx, session, account)
		return
	}

	// Wait for ICE gathering to complete
	localDesc, err := waitForICEGathering(pc, 15*time.Second)
	if err != nil {
		m.log.Error("ICE gathering failed", "error", err, "call_id", session.ID)
		m.rejectCall(ctx, session, account)
		return
	}

	sdpAnswer := localDesc.SDP

	// Step 3: Pre-accept with our SDP answer
	if err := m.preAcceptActiveCall(ctx, session, account, sdpAnswer); err != nil {
		m.log.Error("Failed to pre-accept call", "error", err, "call_id", session.ID)
		m.rejectCall(ctx, session, account)
		return
	}

	// Step 4: Accept with the same SDP answer
	if err := m.acceptActiveCall(ctx, session, account, sdpAnswer); err != nil {
		m.log.Error("Failed to accept call via API", "error", err, "call_id", session.ID)
		return
	}

	session.mu.Lock()
	session.Status = models.CallStatusAnswered
	session.mu.Unlock()

	m.log.Info("Call accepted with WebRTC, waiting for media connection", "call_id", session.ID)

	// Wait for the WebRTC media connection to be established before starting IVR.
	// ICE connectivity checks run after the SDP exchange; we must wait for them
	// to complete before audio can flow.
	select {
	case <-connected:
		m.log.Info("WebRTC media connected", "call_id", session.ID)
	case <-time.After(15 * time.Second):
		m.log.Error("WebRTC media connection timed out", "call_id", session.ID)
		m.terminateCall(session, waAccount)
		return
	}

	// Brief delay to let the media path stabilize before sending audio
	time.Sleep(500 * time.Millisecond)

	// Sticky-routed call: skip IVR entirely and ring the originating agent
	// directly via the existing transfer flow. initiateTransfer's
	// "ring-this-specific-agent-first" branch handles StickyAgentID; an
	// empty team target keeps the no-team broadcast as the eventual
	// fallback if the sticky agent doesn't answer.
	if session.StickyAgentID != nil {
		go m.initiateTransfer(session, session.AccountName, "", nil)
		return
	}

	// Start IVR flow if configured
	if session.IVRFlow != nil {
		go m.runIVRFlow(session, waAccount)
	}
}

// waitForICEGathering waits for ICE gathering to complete on a PeerConnection
// and returns the local description, or an error on timeout.
func waitForICEGathering(pc *webrtc.PeerConnection, timeout time.Duration) (*webrtc.SessionDescription, error) {
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherComplete:
	case <-time.After(timeout):
		return nil, fmt.Errorf("ICE gathering timed out")
	}
	localDesc := pc.LocalDescription()
	if localDesc == nil {
		return nil, fmt.Errorf("no local description available")
	}
	return localDesc, nil
}

// createOpusTrack creates a new Opus audio track and adds it to the PeerConnection.
func createOpusTrack(pc *webrtc.PeerConnection, streamID string) (*webrtc.TrackLocalStaticRTP, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio",
		streamID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create opus track: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		return nil, fmt.Errorf("failed to add opus track: %w", err)
	}
	return track, nil
}

// createPeerConnection creates a new WebRTC peer connection with Opus codec support
func (m *Manager) createPeerConnection(ctx context.Context) (*webrtc.PeerConnection, error) {
	resolved, err := m.ResolveICEConfiguration(ctx)
	if err != nil {
		return nil, err
	}
	iceServers := make([]webrtc.ICEServer, 0, len(resolved.Servers))
	for _, s := range resolved.Servers {
		ice := webrtc.ICEServer{URLs: s.URLs}
		if s.Username != "" {
			ice.Username = s.Username
			ice.Credential = s.Credential
			ice.CredentialType = webrtc.ICECredentialTypePassword
		}
		iceServers = append(iceServers, ice)
	}

	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	// Force all media through TURN relay when direct UDP is not available.
	if m.config.RelayOnly {
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}

	mediaEngine := &webrtc.MediaEngine{}

	// Register Opus codec
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("failed to register Opus codec: %w", err)
	}

	// Register telephone-event codec for DTMF (RFC 4733)
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  "audio/telephone-event",
			ClockRate: 8000,
		},
		PayloadType: 101,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("failed to register telephone-event codec: %w", err)
	}

	// Configure UDP port range and build API
	settingEngine := webrtc.SettingEngine{}
	portMin := m.config.UDPPortMin
	portMax := m.config.UDPPortMax
	if portMin == 0 {
		portMin = 10000
	}
	if portMax == 0 {
		portMax = 10100
	}
	if err := settingEngine.SetEphemeralUDPPortRange(portMin, portMax); err != nil {
		return nil, fmt.Errorf("failed to set UDP port range: %w", err)
	}

	// On cloud/AWS, map private IP to public IP so ICE candidates
	// advertise the reachable address instead of the internal one.
	// Skip when relay_only — host candidates are not used in relay mode.
	if m.config.PublicIP != "" && !m.config.RelayOnly {
		if err := settingEngine.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        []string{m.config.PublicIP},
			AsCandidateType: webrtc.ICECandidateTypeHost,
		}); err != nil {
			return nil, fmt.Errorf("failed to set ICE address rewrite rules: %w", err)
		}
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
	)

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	// Debug: log ICE candidates and connection state to diagnose TURN issues.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			m.log.Info("ICE gathering complete (no more candidates)")
			return
		}
		m.log.Info("ICE candidate gathered",
			"type", c.Typ.String(),
			"address", c.Address,
			"port", c.Port,
			"protocol", c.Protocol.String(),
			"related", c.RelatedAddress,
		)
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		m.log.Info("ICE connection state changed", "state", state.String())
	})

	return pc, nil
}

// consumeAudioTrack reads and discards RTP packets to keep the stream active.
// It exits when the bridge takes over (BridgeStarted channel is closed) or on error.
func (m *Manager) consumeAudioTrack(session *CallSession, track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-session.BridgeStarted:
			// Bridge is taking over reading from this track
			return
		default:
		}

		_, _, err := track.Read(buf)
		if err != nil {
			return
		}
	}
}

// consumeAudioWithDTMF reads RTP packets from the audio track, detecting
// inline telephone-event (DTMF) packets that share the same m-line.
// WhatsApp sends both Opus audio and telephone-event on a single track.
// In pion v4, a new OnTrack may fire for telephone-event, but we also
// handle the case where DTMF arrives on the same track.
func (m *Manager) consumeAudioWithDTMF(session *CallSession, track *webrtc.TrackRemote) {
	audioPT := track.PayloadType()
	var lastDTMFEvent byte = 0xFF
	var lastEndBit bool
	packetCount := 0

	m.log.Info("Consuming audio with inline DTMF detection",
		"call_id", session.ID,
		"audio_pt", audioPT,
	)

	for {
		select {
		case <-session.BridgeStarted:
			return
		default:
		}

		pkt, _, err := track.ReadRTP()
		if err != nil {
			m.log.Debug("Audio track read ended", "call_id", session.ID, "error", err)
			return
		}

		packetCount++

		// Log every 500th packet and any non-audio packet for debugging
		if pkt.PayloadType != uint8(audioPT) {
			m.log.Info("Non-audio RTP packet received",
				"call_id", session.ID,
				"payload_type", pkt.PayloadType,
				"payload_len", len(pkt.Payload),
				"audio_pt", audioPT,
			)

			// Telephone-event DTMF payload is 4 bytes
			if len(pkt.Payload) >= 4 {
				eventID := pkt.Payload[0]
				endBit := (pkt.Payload[1] & 0x80) != 0

				if digit, ok := decodeDTMFEvent(eventID, endBit, &lastDTMFEvent, &lastEndBit); ok {
					m.log.Info("DTMF digit detected (inline)",
						"call_id", session.ID,
						"digit", string(digit),
						"event_id", eventID,
					)
					sendDTMFDigit(session, digit, m.log)
				}
			}
		} else if packetCount == 1 {
			m.log.Debug("First audio packet received",
				"call_id", session.ID,
				"payload_type", pkt.PayloadType,
			)
		}
	}
}

// rejectCall sends a reject action via the WhatsApp API
func (m *Manager) rejectCall(ctx context.Context, session *CallSession, account *models.WhatsAppAccount) {
	if err := m.rejectActiveCall(ctx, session, account); err != nil {
		m.log.Error("Failed to reject call", "error", err, "call_id", session.ID)
	}
}
