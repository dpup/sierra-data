// Package burnline turns a county's recorded burn-information phone line into a
// structured burn-day reading: place a recorded call, transcribe it, and extract
// today's status.
//
// # This is a reading of phone audio, not an API response
//
// Everything downstream must treat it accordingly. The pipeline is
// Twilio call -> Whisper transcription -> LLM extraction, so the result carries
// a Confidence and the cleaned Transcript, and the caller publishes the phone
// number alongside it so a reader can always reach the actual authority.
//
// Two behaviours are deliberate and differ from the TypeScript pipeline this
// replaces (dpup/burnday):
//
//   - **An extraction failure yields an ERROR, never a fabricated status.** The
//     original defaulted to "red" on any failure, reasoning that no-burn is the
//     safe direction. It is the safe direction, but it is still an assertion we
//     never read, and this service's posture everywhere else is that an error
//     must not become a confident answer. Failing means no push happens, the
//     reading ages out, and the facet reads UNKNOWN — which a consumer renders
//     as "call the line": honest and equally safe.
//   - **The caller hangs up.** The line is a looping recorded message that never
//     ends the call, so an un-hung-up call bills until Twilio's own timeout.
package burnline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/dpup/sierra-data/internal/clients/twilio"
)

// Status is the extracted burn-day answer, in the vocabulary the ingest push
// endpoint accepts.
type Status string

const (
	StatusUnknown Status = ""       // unreadable — never published as an answer
	StatusBurn    Status = "green"  // permissive burn day
	StatusNoBurn  Status = "red"    // no-burn day
	StatusLimited Status = "orange" // burn day WITH elevation restrictions
)

// Reading is one call's result.
type Reading struct {
	Status     Status
	Message    string    // brief plain-language summary for display
	Transcript string    // cleaned transcript, loops removed
	Confidence int32     // 0-100, the extraction model's own confidence
	ObservedAt time.Time // when the line was called
}

// Config parameterizes one county's line.
type Config struct {
	// Phone is the recorded line to dial, E.164.
	Phone string
	// From is the Twilio number to call from.
	From string
	// TwiMLURL tells Twilio what to do once answered. The burn line is
	// listen-only, so this is a bin that simply pauses while recording.
	TwiMLURL string
	// County names the county in the prompts — Whisper is markedly better on
	// proper nouns it has been primed with.
	County string
	// HangUpAfter bounds how long we stay on the line. The message loops, so
	// this only needs to cover one full pass.
	HangUpAfter time.Duration
	// Model is the chat model used for extraction.
	Model string
}

// Reader places calls and extracts readings.
type Reader struct {
	tw     *twilio.Client
	ai     *openai.Client
	cfg    Config
	now    func() time.Time
	sleep  func(time.Duration)
	poll   time.Duration // how often to re-check call status
	settle time.Duration // how long to wait for the recording to materialize
}

// NewReader builds a Reader.
func NewReader(tw *twilio.Client, ai *openai.Client, cfg Config) *Reader {
	if cfg.HangUpAfter <= 0 {
		cfg.HangUpAfter = 90 * time.Second
	}
	if cfg.Model == "" {
		cfg.Model = openai.GPT4o
	}
	return &Reader{
		tw: tw, ai: ai, cfg: cfg,
		now: time.Now, sleep: time.Sleep,
		poll: 5 * time.Second, settle: 10 * time.Second,
	}
}

// Read places the call and returns the extracted reading.
func (r *Reader) Read(ctx context.Context) (*Reading, error) {
	observed := r.now().UTC()

	audio, err := r.record(ctx)
	if err != nil {
		return nil, err
	}
	transcript, err := r.transcribe(ctx, audio)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(transcript) == "" {
		return nil, fmt.Errorf("burnline: transcription was empty")
	}
	reading, err := r.extract(ctx, transcript)
	if err != nil {
		return nil, err
	}
	reading.ObservedAt = observed
	return reading, nil
}

// record places the call, hangs up after HangUpAfter, and downloads the audio.
func (r *Reader) record(ctx context.Context) ([]byte, error) {
	call, err := r.tw.CreateCall(ctx, r.cfg.Phone, r.cfg.From, r.cfg.TwiMLURL, 2*time.Minute)
	if err != nil {
		return nil, err
	}

	// Stay on the line long enough for one full pass of the looping message,
	// then hang up. Bounded so a wedged call cannot run (and bill) forever.
	started := r.now()
	deadline := r.cfg.HangUpAfter + 5*time.Minute
	for {
		r.sleep(r.poll)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur, err := r.tw.GetCall(ctx, call.SID)
		if err != nil {
			return nil, err
		}
		if cur.Status == "completed" {
			break
		}
		if cur.Status == "failed" || cur.Status == "busy" ||
			cur.Status == "no-answer" || cur.Status == "canceled" {
			return nil, fmt.Errorf("burnline: call %s ended as %q", call.SID, cur.Status)
		}
		if cur.Status == "in-progress" && r.now().Sub(started) >= r.cfg.HangUpAfter {
			if err := r.tw.EndCall(ctx, call.SID); err != nil {
				return nil, err
			}
			break
		}
		if r.now().Sub(started) > deadline {
			return nil, fmt.Errorf("burnline: call %s never progressed (last status %q)", call.SID, cur.Status)
		}
	}

	// Twilio finalizes a recording asynchronously after the call ends.
	r.sleep(r.settle)
	recs, err := r.tw.Recordings(ctx, call.SID)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("burnline: call %s produced no recording", call.SID)
	}
	return r.tw.DownloadRecording(ctx, recs[0].SID)
}

// transcribe runs Whisper over the recording. The prompt primes it with the
// domain's proper nouns and jargon, which measurably improves "Calaveras",
// "Cal Fire" and the elevation phrasing.
func (r *Reader) transcribe(ctx context.Context, audio []byte) (string, error) {
	prompt := fmt.Sprintf("This is a recording of the %s burn information line. "+
		"The automated message covers burn day restrictions, fire danger, elevation "+
		"requirements, and permit information. Key terms: %s, Cal Fire, burn day, "+
		"permissive burn day, fire danger, elevation restrictions (3500 feet), "+
		"landscape debris, permits, burn barrel.", r.cfg.County, r.cfg.County)

	resp, err := r.ai.CreateTranscription(ctx, openai.AudioRequest{
		Model:       openai.Whisper1,
		FilePath:    "recording.mp3", // names the Reader's content; no file is opened
		Reader:      bytes.NewReader(audio),
		Prompt:      prompt,
		Temperature: 0.1,
		Language:    "en",
	})
	if err != nil {
		return "", fmt.Errorf("burnline: transcribe: %w", err)
	}
	return resp.Text, nil
}

// extractSchema is the structured-output contract for the extraction step.
var extractSchema = openai.ChatCompletionResponseFormatJSONSchema{
	Name:   "burn_status_analysis",
	Strict: true,
	Schema: json.RawMessage(`{
      "type": "object",
      "properties": {
        "status": {"type": "string", "enum": ["green", "red", "orange"]},
        "message": {"type": "string"},
        "cleanedTranscription": {"type": "string"},
        "confidence": {"type": "number"}
      },
      "required": ["status", "message", "cleanedTranscription", "confidence"],
      "additionalProperties": false
    }`),
}

type extraction struct {
	Status               string  `json:"status"`
	Message              string  `json:"message"`
	CleanedTranscription string  `json:"cleanedTranscription"`
	Confidence           float64 `json:"confidence"`
}

// extract reads today's status out of the transcript.
func (r *Reader) extract(ctx context.Context, transcript string) (*Reading, error) {
	today := r.now().Format("Monday, January 2, 2006")

	prompt := fmt.Sprintf(`You are analyzing a transcription of the %s burn information line.

Today's date: %s

The recording LOOPS, so the transcription may repeat the same message more than
once. Keep only the first complete pass.

Classify TODAY's burn status as exactly one of:
  - "green": a burn day with no elevation restriction ("today is a burn day",
    "burning is allowed everywhere").
  - "orange": a burn day that applies only ABOVE a stated elevation
    ("permissive burn days at 3500 feet elevation or more").
  - "red": not a burn day, burning prohibited or suspended, OR the dates the
    message names do not include today.

The elevation distinction is the one people get wrong: an unqualified burn day
is green, an elevation-qualified one is orange. If the message names specific
dates, check them against today's date above rather than assuming the message is
current.

Also return a 0-100 confidence, and a one-sentence summary suitable for display.

Transcription:
%q`, r.cfg.County, today, transcript)

	resp, err := r.ai.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       r.cfg.Model,
		Temperature: 0.1,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &extractSchema,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("burnline: extract: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("burnline: extract returned no choices")
	}

	var out extraction
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &out); err != nil {
		return nil, fmt.Errorf("burnline: decode extraction: %w", err)
	}

	status := parseStatus(out.Status)
	if status == StatusUnknown {
		// Unlike the pipeline this replaces, an unreadable answer is NOT
		// downgraded to a fabricated "red" — see the package doc.
		return nil, fmt.Errorf("burnline: model returned unusable status %q", out.Status)
	}
	confidence := out.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 100 {
		confidence = 100
	}
	cleaned := strings.TrimSpace(out.CleanedTranscription)
	if cleaned == "" {
		cleaned = strings.TrimSpace(transcript)
	}
	return &Reading{
		Status:     status,
		Message:    strings.TrimSpace(out.Message),
		Transcript: cleaned,
		Confidence: int32(confidence),
	}, nil
}

// parseStatus maps the model's vocabulary onto Status. Anything unrecognized is
// UNKNOWN, never a permissive default.
func parseStatus(s string) Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "green":
		return StatusBurn
	case "red":
		return StatusNoBurn
	case "orange":
		return StatusLimited
	default:
		return StatusUnknown
	}
}
