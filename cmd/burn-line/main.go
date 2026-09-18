// Command burn-line calls the configured county burn-information lines, extracts
// today's burn-day status from each, and PUSHES the readings to The Grid's
// shared push-ingest endpoint as the `burn.line` stream.
//
// It runs on a schedule from CI (.github/workflows/burn-line.yml), not inside
// the server: placing phone calls costs money and must not be re-triggered by a
// container restart, so the dialing lives somewhere with exactly-once-per-day
// semantics. The server only ever receives the results.
//
// # prefab.yaml is the single source of truth
//
// Which lines exist, which are dialed, and which counties each speaks for all
// come from `grid.burn.lines` — the same config the server reads. The workflow
// does NOT enumerate numbers, so enabling a county is one flag in one file
// rather than a config edit plus a matching CI edit that can silently drift
// (config-on/CI-off degrades the source with no reading; CI-on/config-off gets
// 400s from the endpoint).
//
// Usage:
//
//	burn-line -endpoint https://data.sierragridteam.org/api/v1/ingest/burn.line
//	burn-line -line calaveras-apcd      # just one line
//	burn-line -dry-run                  # call and extract, print, do not push
//
// Credentials come from the environment:
//
//	TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_FROM_NUMBER, TWILIO_TWIML_URL
//	OPENAI_API_KEY
//	GRID_INGEST_TOKEN   a grid.ingest reporter token granted the "burn.line"
//	                    stream. Mint with:
//	                      make ingest-token REPORTER=burn-line STREAMS=burn.line
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/dpup/sierra-data/internal/clients/burnline"
	"github.com/dpup/sierra-data/internal/clients/twilio"
	"github.com/dpup/sierra-data/internal/config"
)

func main() {
	var (
		endpoint = flag.String("endpoint", "", "Grid burn.line ingest endpoint, e.g. https://host/api/v1/ingest/burn.line (required unless -dry-run)")
		only     = flag.String("line", "", "dial only this line id (default: every enabled line)")
		model    = flag.String("model", openai.GPT4o, "chat model for extraction")
		hangUp   = flag.Duration("hangup-after", 90*time.Second, "how long to stay on each line")
		timeout  = flag.Duration("timeout", 10*time.Minute, "deadline per line")
		dryRun   = flag.Bool("dry-run", false, "call and extract, print the readings, do not push")
	)
	flag.Parse()

	if *endpoint == "" && !*dryRun {
		fatal("-endpoint is required (or pass -dry-run)")
	}

	cfg := config.LoadConfig()
	lines := cfg.Grid.Burn.EnabledLines()
	if *only != "" {
		l, ok := cfg.Grid.Burn.Line(*only)
		if !ok {
			fatal("no line %q in grid.burn.lines", *only)
		}
		if !l.Dialed() {
			fatal("line %q is configured but not enabled (set enabled: true to dial it)", *only)
		}
		lines = []config.BurnLine{l}
	}
	if len(lines) == 0 {
		fatal("no enabled burn lines in grid.burn.lines")
	}

	sid := mustEnv("TWILIO_ACCOUNT_SID")
	authToken := mustEnv("TWILIO_AUTH_TOKEN")
	from := mustEnv("TWILIO_FROM_NUMBER")
	twiml := mustEnv("TWILIO_TWIML_URL")
	openaiKey := mustEnv("OPENAI_API_KEY")
	var ingestToken string
	if !*dryRun {
		ingestToken = mustEnv("GRID_INGEST_TOKEN")
	}

	tw := twilio.NewClient(sid, authToken)
	ai := openai.NewClient(openaiKey)

	// Lines are independent: one being unreachable must not stop the others.
	// Every failure is reported and the exit code reflects whether ANY failed,
	// so a single bad line is visible in CI without losing the good ones.
	// Read every line first, then push ONE report. Reading is independent per
	// line — one unreachable recording must not cost the others — but the push
	// is batched, because the endpoint rate-limits per reporter against the last
	// ACCEPTED report, so a push per line would 429 everything after the first.
	var failed []string
	var readings []pushReading
	for _, l := range lines {
		fmt.Printf("\n=== %s (%s) — %s\n", l.ID, l.Phone, strings.Join(l.Counties, ", "))
		r, err := read(l, tw, ai, runOpts{
			from: from, twiml: twiml, model: *model,
			hangUp: *hangUp, timeout: *timeout,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
			failed = append(failed, l.ID)
			continue
		}
		readings = append(readings, pushReading{
			LineID:               l.ID,
			Status:               string(r.Status),
			Message:              r.Message,
			CleanedTranscription: r.Transcript,
			Confidence:           r.Confidence,
			ObservedAt:           r.ObservedAt.Format(time.RFC3339),
		})
	}

	if len(readings) > 0 && !*dryRun {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		if err := push(ctx, *endpoint, ingestToken, readings); err != nil {
			cancel()
			fatal("push failed (%d reading(s) not delivered): %v", len(readings), err)
		}
		cancel()
		fmt.Printf("\nPushed %d reading(s) to %s\n", len(readings), *endpoint)
	} else if *dryRun {
		fmt.Printf("\n-dry-run: %d reading(s) not pushed\n", len(readings))
	}

	if len(failed) > 0 {
		// A failed line contributes NOTHING to the report. Its previous reading
		// then ages past the server's freshness gate and the facet reads UNKNOWN —
		// the honest outcome, and why this exits non-zero rather than guessing.
		fatal("%d of %d line(s) failed: %s", len(failed), len(lines), strings.Join(failed, ", "))
	}
	fmt.Printf("\nAll %d line(s) read successfully.\n", len(lines))
}

// runOpts bundles the per-run settings shared by every line.
type runOpts struct {
	from, twiml, model string
	hangUp, timeout    time.Duration
}

// read dials one line and returns its reading.
func read(l config.BurnLine, tw *twilio.Client, ai *openai.Client, o runOpts) (*burnline.Reading, error) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	reader := burnline.NewReader(tw, ai, burnline.Config{
		Phone: l.Phone, From: o.from, TwiMLURL: o.twiml,
		County: displayName(l), HangUpAfter: o.hangUp, Model: o.model,
	})

	reading, err := reader.Read(ctx)
	if err != nil {
		return nil, err
	}

	fmt.Printf("  status:     %s\n", reading.Status)
	fmt.Printf("  confidence: %d\n", reading.Confidence)
	fmt.Printf("  message:    %s\n", reading.Message)
	fmt.Printf("  observedAt: %s\n", reading.ObservedAt.Format(time.RFC3339))
	fmt.Printf("  transcript: %s\n", truncate(reading.Transcript, 240))
	return reading, nil
}

// pushBody mirrors the burn.line stream contract. Request bodies on
// /api/v1/ingest are snake_case (responses are camelCase).
type pushBody struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Readings      []pushReading `json:"readings"`
}

type pushReading struct {
	LineID               string `json:"line_id"`
	Status               string `json:"status"`
	Message              string `json:"message"`
	CleanedTranscription string `json:"cleaned_transcription"`
	Confidence           int32  `json:"confidence"`
	ObservedAt           string `json:"observed_at"`
}

// push sends every reading in ONE report.
//
// Batched deliberately: the endpoint rate-limits per reporter against the last
// ACCEPTED report (60s for this one), so pushing each line separately would 429
// every line after the first. One report is also atomic from the operator's
// point of view — the 202 body reports accepted counts and per-reading warnings.
func push(ctx context.Context, endpoint, token string, readings []pushReading) error {
	body, err := json.Marshal(pushBody{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Readings:      readings,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	// Warnings mean some readings were skipped; surface them, they are the only
	// signal that a line silently did not land.
	fmt.Printf("  server: %s\n", strings.TrimSpace(buf.String()))
	return nil
}

// displayName is what the transcription and extraction prompts are primed with.
// The line's own name is best ("Calaveras County APCD burn line"); fall back to
// the first county it speaks for.
func displayName(l config.BurnLine) string {
	if l.Name != "" {
		return l.Name
	}
	if len(l.Counties) > 0 {
		return countyDisplayName(l.Counties[0])
	}
	return l.ID
}

// countyDisplayName turns "calaveras-county" into "Calaveras County".
func countyDisplayName(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		fatal("%s is required in the environment", key)
	}
	return v
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "burn-line: "+format+"\n", args...)
	os.Exit(1)
}
