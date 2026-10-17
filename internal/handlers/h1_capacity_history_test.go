package handlers

// Candidate-only source proposal. Provider bodies are synthetic and captured
// locally. No transport delegates unknown hosts to the network.
import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
)

type h1CapacityBodyTransport struct {
	body  map[string]any
	calls int
}

func (s *h1CapacityBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.Scheme != "https" {
		return nil, errors.New("unexpected synthetic method or scheme")
	}
	response := `{"choices":[{"message":{"content":"Synthetic provider result"}}]}`
	switch r.URL.Host {
	case "api.openai.com":
		if r.URL.Path != "/v1/chat/completions" {
			return nil, errors.New("unexpected OpenAI path")
		}
	case "dashscope-intl.aliyuncs.com":
		if r.URL.Path != "/compatible-mode/v1/chat/completions" {
			return nil, errors.New("unexpected Qwen path")
		}
	case "api.anthropic.com":
		if r.URL.Path != "/v1/messages" {
			return nil, errors.New("unexpected Anthropic path")
		}
		response = `{"content":[{"type":"text","text":"Synthetic provider result"}]}`
	case "generativelanguage.googleapis.com":
		if r.URL.Path != "/v1beta/models/h1-history-synthetic:generateContent" {
			return nil, errors.New("unexpected Google path")
		}
		response = `{"candidates":[{"content":{"parts":[{"text":"Synthetic provider result"}]}}]}`
	default:
		return nil, errors.New("unexpected external host in synthetic request")
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&s.body); err != nil {
		return nil, err
	}
	s.calls++
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
}

func h1CapacityHistoryRows(session uuid.UUID) ([]models.ChatbotSessionMessage, models.ChatbotSessionMessage) {
	now := time.Date(2026, time.October, 17, 1, 2, 3, 0, time.UTC)
	row := func(text string, direction models.Direction, when time.Time) models.ChatbotSessionMessage {
		return models.ChatbotSessionMessage{
			BaseModel: models.BaseModel{ID: uuid.New(), CreatedAt: when}, SessionID: session, Direction: direction, Message: text,
		}
	}
	return []models.ChatbotSessionMessage{
		row("Synthetic older user", models.DirectionIncoming, now.Add(-2*time.Minute)),
		row("Synthetic prior answer", models.DirectionOutgoing, now.Add(-time.Minute)),
		row("Synthetic future history", models.DirectionIncoming, now.Add(time.Minute)),
	}, row("Synthetic current user", models.DirectionIncoming, now)
}

func TestH1CapacityHistory_ProjectionUsesExistingLimitAndTimestampSemantics(t *testing.T) {
	history, incoming := h1CapacityHistoryRows(uuid.New())
	cases := []struct {
		name  string
		limit int
		want  []string
	}{
		{"zero", 0, nil},
		{"latest-future-row", 1, []string{"Synthetic future history"}},
		{"current-plus-future", 2, []string{"Synthetic current user", "Synthetic future history"}},
		{"bounded-history", 3, []string{"Synthetic prior answer", "Synthetic current user", "Synthetic future history"}},
		{"unbounded-negative", -1, []string{"Synthetic older user", "Synthetic prior answer", "Synthetic current user", "Synthetic future history"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projected := projectIncomingSessionHistory(history, incoming, tc.limit)
			var got []string
			for _, row := range projected {
				got = append(got, row.Message)
				require.Equal(t, incoming.SessionID, row.SessionID)
			}
			require.Equal(t, tc.want, got)
			for _, row := range projected {
				if row.ID == incoming.ID {
					require.Equal(t, incoming, row, "virtual current row preserves exact identity and timestamp")
				}
			}
		})
	}
	// PostgreSQL has no secondary ordering for tied created_at. Preserve the
	// correct bounded set; this test deliberately does not invent a tie order.
	tied := history[0]
	tied.CreatedAt = incoming.CreatedAt
	got := projectIncomingSessionHistory([]models.ChatbotSessionMessage{tied}, incoming, 2)
	require.Len(t, got, 2)
	require.ElementsMatch(t, []uuid.UUID{tied.ID, incoming.ID}, []uuid.UUID{got[0].ID, got[1].ID})
	require.Equal(t, "Synthetic older user", history[0].Message, "projection does not mutate its source slice")
}

func TestH1CapacityHistory_AllProvidersKeepSelectedSnapshotAndCurrentMessage(t *testing.T) {
	providers := []models.AIProvider{models.AIProviderOpenAI, models.AIProviderAnthropic, models.AIProviderGoogle, models.AIProviderQwen}
	cases := []struct {
		name    string
		include bool
		limit   int
		want    []string
		roles   []string
	}{
		{"disabled", false, 3, []string{"Synthetic current user"}, []string{"user"}},
		{"zero", true, 0, []string{"Synthetic current user"}, []string{"user"}},
		{"one", true, 1, []string{"Synthetic future history", "Synthetic current user"}, []string{"user", "user"}},
		{"three", true, 3, []string{"Synthetic prior answer", "Synthetic current user", "Synthetic future history", "Synthetic current user"}, []string{"assistant", "user", "user", "user"}},
	}
	for _, provider := range providers {
		for _, tc := range cases {
			t.Run(string(provider)+"/"+tc.name, func(t *testing.T) {
				session := &models.ChatbotSession{BaseModel: models.BaseModel{ID: uuid.New()}, SessionData: models.JSONB{}}
				history, incoming := h1CapacityHistoryRows(session.ID)
				projected := projectIncomingSessionHistory(history, incoming, tc.limit)
				transport := &h1CapacityBodyTransport{}
				app := &App{Log: testutil.NopLogger(), HTTPClient: &http.Client{Transport: transport, Timeout: time.Second},
					legacyAIInput: &legacyAIInput{sessionID: session.ID, history: projected}}
				settings := &models.ChatbotSettings{OrganizationID: uuid.New(), AI: models.AIConfig{Enabled: true, Provider: provider,
					APIKey: "synthetic-not-a-credential", Model: "h1-history-synthetic", BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", IncludeHistory: tc.include, HistoryLimit: tc.limit, MaxTokens: 32}}
				// Mutating a returned history slice must not mutate the invocation input.
				returned := app.getSessionHistory(session.ID, tc.limit)
				if len(returned) > 0 {
					returned[0].Message = "Synthetic caller mutation"
				}
				answer, err := app.generateAIResponse(settings, session, incoming.Message)
				require.NoError(t, err)
				require.Equal(t, "Synthetic provider result", answer)
				require.Equal(t, 1, transport.calls)
				var contents, roles []string
				if provider == models.AIProviderGoogle {
					entries, ok := transport.body["contents"].([]any)
					require.True(t, ok)
					for _, entry := range entries {
						message, ok := entry.(map[string]any)
						require.True(t, ok)
						role, ok := message["role"].(string)
						require.True(t, ok)
						if role == "model" {
							role = "assistant"
						}
						roles = append(roles, role)
						parts, ok := message["parts"].([]any)
						require.True(t, ok)
						require.Len(t, parts, 1)
						part, ok := parts[0].(map[string]any)
						require.True(t, ok)
						text, ok := part["text"].(string)
						require.True(t, ok)
						contents = append(contents, text)
					}
				} else {
					entries, ok := transport.body["messages"].([]any)
					require.True(t, ok)
					for _, entry := range entries {
						message, ok := entry.(map[string]any)
						require.True(t, ok)
						role, ok := message["role"].(string)
						require.True(t, ok)
						text, ok := message["content"].(string)
						require.True(t, ok)
						roles = append(roles, role)
						contents = append(contents, text)
					}
				}
				require.Equal(t, tc.want, contents)
				require.Equal(t, tc.roles, roles)
			})
		}
	}
}
