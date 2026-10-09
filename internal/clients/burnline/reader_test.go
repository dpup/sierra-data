package burnline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/clients/twilio"
)

func TestParseStatus(t *testing.T) {
	assert.Equal(t, StatusBurn, parseStatus("green"))
	assert.Equal(t, StatusNoBurn, parseStatus("red"))
	assert.Equal(t, StatusLimited, parseStatus("orange"))
	assert.Equal(t, StatusLimited, parseStatus(" ORANGE "))
	// Never a permissive default.
	assert.Equal(t, StatusUnknown, parseStatus("chartreuse"))
	assert.Equal(t, StatusUnknown, parseStatus(""))
}

// twilioStub serves the four Twilio calls the reader makes.
func twilioStub(t *testing.T, callStatus string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/Calls.json"):
			_, _ = w.Write([]byte(`{"sid":"CA1","status":"queued"}`))
		case strings.Contains(r.URL.Path, "/Recordings/"):
			_, _ = w.Write([]byte("fake-mp3"))
		case strings.Contains(r.URL.Path, "/Recordings.json"):
			_, _ = w.Write([]byte(`{"recordings":[{"sid":"RE1","duration":"40"}]}`))
		case strings.Contains(r.URL.Path, "/Calls/CA1.json"):
			_, _ = w.Write([]byte(`{"sid":"CA1","status":"` + callStatus + `"}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

// openaiStub serves the transcription and the extraction.
func openaiStub(t *testing.T, transcript string, extraction any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/audio/transcriptions"):
			_ = json.NewEncoder(w).Encode(map[string]any{"text": transcript})
		case strings.Contains(r.URL.Path, "/chat/completions"):
			content, _ := json.Marshal(extraction)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "content": string(content)},
				}},
			})
		default:
			w.WriteHeader(404)
		}
	}))
}

func newTestReader(t *testing.T, tw, ai *httptest.Server) *Reader {
	t.Helper()
	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL = ai.URL
	r := NewReader(
		twilio.NewClientWithHTTPDoer(tw.URL, "AC1", "tok", tw.Client()),
		openai.NewClientWithConfig(cfg),
		Config{Phone: "+12097546600", From: "+15550000000", TwiMLURL: "https://twiml.test",
			County: "Calaveras County", HangUpAfter: time.Millisecond},
	)
	// Keep the call state machine's timing out of the test.
	r.sleep = func(time.Duration) {}
	r.poll = 0
	r.settle = 0
	return r
}

func TestRead_HappyPath(t *testing.T) {
	tw := twilioStub(t, "completed")
	defer tw.Close()
	ai := openaiStub(t, "Today, September 10th, is not a burn day in Calaveras County.", map[string]any{
		"status":               "red",
		"message":              "Burning is prohibited today in Calaveras County.",
		"cleanedTranscription": "Today, September 10th, is not a burn day in Calaveras County.",
		"confidence":           95,
	})
	defer ai.Close()

	got, err := newTestReader(t, tw, ai).Read(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusNoBurn, got.Status)
	assert.Equal(t, int32(95), got.Confidence)
	assert.Contains(t, got.Transcript, "not a burn day")
	assert.False(t, got.ObservedAt.IsZero(), "a reading must carry when it was taken")
}

// "orange" is an elevation-restricted burn day and must survive as its own
// status rather than collapsing into green.
func TestRead_ElevationRestrictedIsOrange(t *testing.T) {
	tw := twilioStub(t, "completed")
	defer tw.Close()
	ai := openaiStub(t, "permissive burn days at 3500 feet elevation or more", map[string]any{
		"status": "orange", "message": "Burning allowed above 3500 feet.",
		"cleanedTranscription": "permissive burn days at 3500 feet elevation or more",
		"confidence":           88,
	})
	defer ai.Close()

	got, err := newTestReader(t, tw, ai).Read(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusLimited, got.Status)
}

// THE behaviour that differs from the pipeline this replaces: an unusable
// extraction must ERROR, not fall back to a fabricated "red" nobody read.
func TestRead_UnusableStatusIsAnError(t *testing.T) {
	tw := twilioStub(t, "completed")
	defer tw.Close()
	ai := openaiStub(t, "some transcript", map[string]any{
		"status": "chartreuse", "message": "?", "cleanedTranscription": "x", "confidence": 10,
	})
	defer ai.Close()

	_, err := newTestReader(t, tw, ai).Read(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unusable status")
}

// An empty transcription must not reach the extractor — there is nothing to
// classify, and a model handed "" will still confidently return something.
func TestRead_EmptyTranscriptionIsAnError(t *testing.T) {
	tw := twilioStub(t, "completed")
	defer tw.Close()
	ai := openaiStub(t, "   ", map[string]any{"status": "red"})
	defer ai.Close()

	_, err := newTestReader(t, tw, ai).Read(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// A call that never connects must fail rather than hang or produce a guess.
func TestRead_FailedCallIsAnError(t *testing.T) {
	tw := twilioStub(t, "no-answer")
	defer tw.Close()
	ai := openaiStub(t, "unused", map[string]any{"status": "red"})
	defer ai.Close()

	_, err := newTestReader(t, tw, ai).Read(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-answer")
}

// Confidence outside 0-100 would be rejected by the ingest endpoint; clamp so a
// model slip does not discard an otherwise good reading.
func TestRead_ConfidenceIsClamped(t *testing.T) {
	tw := twilioStub(t, "completed")
	defer tw.Close()
	ai := openaiStub(t, "today is a burn day", map[string]any{
		"status": "green", "message": "Burning allowed.",
		"cleanedTranscription": "today is a burn day", "confidence": 140,
	})
	defer ai.Close()

	got, err := newTestReader(t, tw, ai).Read(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(100), got.Confidence)
}
