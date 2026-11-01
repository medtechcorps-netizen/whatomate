package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/callingaudio"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/tts"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

type callingAudioTestObjects struct {
	objects map[string][]byte
	putErr  error
	getErr  error
	puts    int
}

func (s *callingAudioTestObjects) Put(_ context.Context, key string, data []byte, contentType string) error {
	s.puts++
	if s.putErr != nil {
		return s.putErr
	}
	if contentType != "audio/ogg" {
		return fmt.Errorf("unexpected audio content type %q", contentType)
	}
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = append([]byte(nil), data...)
	return nil
}

func (s *callingAudioTestObjects) Get(_ context.Context, key string) ([]byte, string, error) {
	if s.getErr != nil {
		return nil, "", s.getErr
	}
	data, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return append([]byte(nil), data...), "audio/ogg", nil
}

func (s *callingAudioTestObjects) Delete(context.Context, string) error { return nil }
func (s *callingAudioTestObjects) ListPrefix(context.Context, string) ([]storage.ObjectInfo, error) {
	return nil, nil
}

// A short mono PCM WAV exercises the real ffmpeg conversion without fixtures,
// provider access or a speech synthesis installation.
func callingAudioTestWAV(t *testing.T, frequency float64) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for calling audio transcoding tests")
	}
	const sampleRate = 8000
	const samples = 800
	var out bytes.Buffer
	out.WriteString("RIFF")
	require.NoError(t, binary.Write(&out, binary.LittleEndian, uint32(36+samples*2)))
	out.WriteString("WAVEfmt ")
	for _, value := range []any{uint32(16), uint16(1), uint16(1), uint32(sampleRate), uint32(sampleRate * 2), uint16(2), uint16(16)} {
		require.NoError(t, binary.Write(&out, binary.LittleEndian, value))
	}
	out.WriteString("data")
	require.NoError(t, binary.Write(&out, binary.LittleEndian, uint32(samples*2)))
	for i := 0; i < samples; i++ {
		sample := int16(10000 * math.Sin(2*math.Pi*frequency*float64(i)/sampleRate))
		require.NoError(t, binary.Write(&out, binary.LittleEndian, sample))
	}
	return out.Bytes()
}

func TestCallingAudioTranscodePersistsBeforeReturningAndSurvivesColdCache(t *testing.T) {
	objects := &callingAudioTestObjects{}
	orgID := uuid.New()
	app := &App{AudioStore: callingaudio.New(t.TempDir(), objects)}
	filename, err := app.transcodeAndStoreCallingAudio(context.Background(), orgID, callingAudioTestWAV(t, 440))
	require.NoError(t, err)
	require.NoError(t, callingaudio.ValidateFilename(filename))
	assert.Equal(t, filename, filepath.Base(filename), "the graph/API must keep a basename")
	key := "organizations/" + orgID.String() + "/calling/audio/" + filename
	require.Contains(t, objects.objects, key)
	assert.Contains(t, string(objects.objects[key]), "OpusHead")

	coldReplica := callingaudio.New(t.TempDir(), objects)
	restored, err := coldReplica.Read(context.Background(), orgID, filename)
	require.NoError(t, err)
	assert.Equal(t, objects.objects[key], restored)
	_, err = coldReplica.Read(context.Background(), uuid.New(), filename)
	assert.ErrorIs(t, err, callingaudio.ErrNotFound)

	objects.putErr = errors.New("durable write unavailable")
	failedFilename, err := app.transcodeAndStoreCallingAudio(context.Background(), orgID, callingAudioTestWAV(t, 660))
	assert.ErrorIs(t, err, objects.putErr)
	assert.Empty(t, failedFilename, "a failed durable write must not return an asset reference")
	assert.Len(t, objects.objects, 1)
}

func TestCallingAudioLocalModeAndMissingDurableConfiguration(t *testing.T) {
	orgID := uuid.New()
	cfg := &config.Config{Calling: config.CallingConfig{AudioDir: t.TempDir()}}
	app := &App{Config: cfg}
	filename, err := app.transcodeAndStoreCallingAudio(context.Background(), orgID, callingAudioTestWAV(t, 440))
	require.NoError(t, err)
	restarted := &App{Config: cfg}
	store, err := restarted.callingAudioStore()
	require.NoError(t, err)
	_, err = store.Read(context.Background(), orgID, filename)
	require.NoError(t, err)

	cfg.Storage.Type = "s3"
	_, err = restarted.callingAudioStore()
	require.ErrorContains(t, err, "object storage is not initialized")
}

func cachedCallingAudioTTS(t *testing.T) (*tts.PiperTTS, string, []byte) {
	t.Helper()
	text := "A cached IVR greeting"
	hash := sha256.Sum256([]byte(text))
	filename := fmt.Sprintf("tts_%x.ogg", hash[:8])
	data := []byte("OggS-synthetic-piper-cache")
	piper := &tts.PiperTTS{AudioDir: t.TempDir(), BinaryPath: "must-not-run-piper"}
	require.NoError(t, os.WriteFile(filepath.Join(piper.AudioDir, filename), data, 0600))
	return piper, text, data
}

func callingAudioGreetingMenu(text string) models.JSONB {
	return models.JSONB{
		"version": 2, "entry_node": "greeting", "edges": []any{},
		"nodes": []any{map[string]any{
			"id": "greeting", "type": "hangup",
			"config": map[string]any{"greeting_text": text, "audio_file": "previous.ogg"},
		}},
	}
}

func callingAudioMenuFilename(menu models.JSONB) string {
	return menu["nodes"].([]any)[0].(map[string]any)["config"].(map[string]any)["audio_file"].(string)
}

func TestCallingAudioTTSCacheHitIsPersistedPerTenantAndFailsClosed(t *testing.T) {
	piper, text, data := cachedCallingAudioTTS(t)
	objects := &callingAudioTestObjects{}
	app := &App{TTS: piper, AudioStore: callingaudio.New(t.TempDir(), objects)}
	orgA, orgB := uuid.New(), uuid.New()
	for _, orgID := range []uuid.UUID{orgA, orgB} {
		menu := callingAudioGreetingMenu(text)
		require.NoError(t, app.generateIVRAudio(context.Background(), orgID, menu))
		filename := callingAudioMenuFilename(menu)
		assert.Equal(t, fmt.Sprintf("%x.ogg", sha256.Sum256(data)), filename)
		assert.Equal(t, data, objects.objects["organizations/"+orgID.String()+"/calling/audio/"+filename])
	}
	assert.Equal(t, 2, objects.puts)

	objects.putErr = errors.New("object store unavailable")
	menu := callingAudioGreetingMenu(text)
	require.ErrorIs(t, app.generateIVRAudio(context.Background(), orgA, menu), objects.putErr)
	assert.Equal(t, "previous.ogg", callingAudioMenuFilename(menu), "do not return a new graph reference on persistence failure")
}

func callingAudioUploadRequest(t *testing.T, orgID, userID uuid.UUID, frequency float64) *fastglue.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="test.wav"`},
		"Content-Type":        {"audio/wav"},
	})
	require.NoError(t, err)
	_, err = part.Write(callingAudioTestWAV(t, frequency))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	req := callingAudioRequest()
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
	req.RequestCtx.Request.SetBody(body.Bytes())
	testutil.SetAuthContext(req, orgID, userID)
	t.Cleanup(func() { req.RequestCtx.Request.RemoveMultipartFormFiles() })
	return req
}

// Init supplies fasthttp's server context, required when a handler passes the
// request through context.Context to storage (not just its HTTP fields).
func callingAudioRequest() *fastglue.Request {
	ctx := &fasthttp.RequestCtx{}
	ctx.Init(&fasthttp.Request{}, nil, nil)
	return &fastglue.Request{RequestCtx: ctx}
}

func TestCallingAudioHandlersUploadPreviewAndOrganizationReplacement(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	otherOrg := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	objects := &callingAudioTestObjects{}
	app := &App{DB: db, Log: testutil.NopLogger(), AudioStore: callingaudio.New(t.TempDir(), objects)}

	upload := callingAudioUploadRequest(t, org.ID, user.ID, 440)
	require.NoError(t, app.UploadIVRAudio(upload))
	require.Equal(t, fasthttp.StatusOK, upload.RequestCtx.Response.StatusCode(), string(upload.RequestCtx.Response.Body()))
	var uploaded struct {
		Filename string `json:"filename"`
	}
	testutil.ParseEnvelopeResponse(t, upload, &uploaded)
	require.NotEmpty(t, uploaded.Filename)

	// A replacement instance has none of the original replica's local files.
	app.AudioStore = callingaudio.New(t.TempDir(), objects)
	for _, targetOrg := range []uuid.UUID{org.ID, otherOrg.ID} {
		preview := callingAudioRequest()
		testutil.SetAuthContext(preview, targetOrg, user.ID)
		testutil.SetPathParam(preview, "filename", uploaded.Filename)
		require.NoError(t, app.ServeIVRAudio(preview))
		if targetOrg == org.ID {
			assert.Equal(t, fasthttp.StatusOK, preview.RequestCtx.Response.StatusCode())
			assert.Contains(t, string(preview.RequestCtx.Response.Body()), "OpusHead")
			assert.Equal(t, "private, no-store", string(preview.RequestCtx.Response.Header.Peek("Cache-Control")))
		} else {
			assert.Equal(t, fasthttp.StatusNotFound, preview.RequestCtx.Response.StatusCode())
		}
	}
	invalid := callingAudioRequest()
	testutil.SetAuthContext(invalid, org.ID, user.ID)
	testutil.SetPathParam(invalid, "filename", "../"+uploaded.Filename)
	require.NoError(t, app.ServeIVRAudio(invalid))
	assert.Equal(t, fasthttp.StatusBadRequest, invalid.RequestCtx.Response.StatusCode())

	objects.getErr = errors.New("object store temporarily unavailable")
	app.AudioStore = callingaudio.New(t.TempDir(), objects)
	unavailable := callingAudioRequest()
	testutil.SetAuthContext(unavailable, org.ID, user.ID)
	testutil.SetPathParam(unavailable, "filename", uploaded.Filename)
	require.NoError(t, app.ServeIVRAudio(unavailable))
	assert.Equal(t, fasthttp.StatusInternalServerError, unavailable.RequestCtx.Response.StatusCode(), "provider failures are not missing files")
	objects.getErr = nil

	for _, audioType := range []string{"hold_music", "ringback"} {
		var oldFilename string
		for _, frequency := range []float64{440, 660} {
			req := callingAudioUploadRequest(t, org.ID, user.ID, frequency)
			testutil.SetQueryParam(req, "type", audioType)
			require.NoError(t, app.UploadOrgAudio(req))
			require.Equal(t, fasthttp.StatusOK, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
			testutil.ParseEnvelopeResponse(t, req, &uploaded)
			assert.NotEqual(t, oldFilename, uploaded.Filename)
			if oldFilename != "" {
				_, err := app.AudioStore.Read(context.Background(), org.ID, oldFilename)
				require.NoError(t, err, "replacing organization audio must preserve the previous asset")
			}
			oldFilename = uploaded.Filename
		}
		require.NoError(t, db.First(org, org.ID).Error)
		assert.Equal(t, oldFilename, org.Settings[audioType+"_file"])
	}

	objects.putErr = errors.New("durable store rejected write")
	failed := callingAudioUploadRequest(t, org.ID, user.ID, 880)
	testutil.SetQueryParam(failed, "type", "hold_music")
	before := org.Settings["hold_music_file"]
	require.NoError(t, app.UploadOrgAudio(failed))
	assert.Equal(t, fasthttp.StatusInternalServerError, failed.RequestCtx.Response.StatusCode())
	require.NoError(t, db.First(org, org.ID).Error)
	assert.Equal(t, before, org.Settings["hold_music_file"])

	failed = callingAudioUploadRequest(t, org.ID, user.ID, 880)
	require.NoError(t, app.UploadIVRAudio(failed))
	assert.Equal(t, fasthttp.StatusInternalServerError, failed.RequestCtx.Response.StatusCode())
}

func TestCallingAudioTTSFailurePreservesSavedFlowAndRouting(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	piper, text, _ := cachedCallingAudioTTS(t)
	objects := &callingAudioTestObjects{putErr: errors.New("durable store unavailable")}
	app := &App{DB: db, Log: testutil.NopLogger(), TTS: piper, AudioStore: callingaudio.New(t.TempDir(), objects)}
	existing := models.IVRFlow{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		WhatsAppAccount: "calling-audio-test", Name: "Original", IsActive: true, IsCallStart: true,
		Menu: callingAudioGreetingMenu(""),
	}
	require.NoError(t, db.Create(&existing).Error)
	for _, update := range []bool{false, true} {
		body, err := json.Marshal(IVRFlowRequest{
			WhatsAppAccount: existing.WhatsAppAccount, Name: "Changed", IsActive: true, IsCallStart: true,
			Menu: callingAudioGreetingMenu(text),
		})
		require.NoError(t, err)
		req := callingAudioRequest()
		req.RequestCtx.Request.Header.SetContentType("application/json")
		req.RequestCtx.Request.Header.SetMethod("POST")
		req.RequestCtx.Request.SetBody(body)
		testutil.SetAuthContext(req, org.ID, user.ID)
		if update {
			testutil.SetPathParam(req, "id", existing.ID.String())
			require.NoError(t, app.UpdateIVRFlow(req))
		} else {
			require.NoError(t, app.CreateIVRFlow(req))
		}
		require.Equal(t, fasthttp.StatusBadRequest, req.RequestCtx.Response.StatusCode(), string(req.RequestCtx.Response.Body()))
		var flows []models.IVRFlow
		require.NoError(t, db.Where("organization_id = ?", org.ID).Find(&flows).Error)
		require.Len(t, flows, 1, "failed TTS must not create a flow")
		assert.Equal(t, "Original", flows[0].Name)
		assert.True(t, flows[0].IsCallStart, "failed TTS must not unset existing call routing")
		assert.Equal(t, "previous.ogg", callingAudioMenuFilename(flows[0].Menu))
	}
	assert.Equal(t, 2, objects.puts, "both create and update must try durable storage on a Piper cache hit")
}
