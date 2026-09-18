// Command burn-line calls the configured county burn-information lines, extracts
// today's burn-day status from each, and PUSHES the readings to The Grid's
// /ingest/burn-line endpoint.
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
//	burn-line -endpoint https://data.sierragridteam.org/ingest/burn-line
//	burn-line -line calaveras-apcd      # just one line
//	burn-line -dry-run                  # call and extract, print, do not push
//
// Credentials come from the environment:
//
//	TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_FROM_NUMBER, TWILIO_TWIML_URL
//	OPENAI_API_KEY
//	GRID_INGEST_TOKEN   (matches the server's grid.burn.ingestToken)
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
		endpoint = flag.String("endpoint", "", "Grid ingest endpoint (required unless -dry-run)")
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
	var failed []string
	for _, l := range lines {
		fmt.Printf("\n=== %s (%s) — %s\n", l.ID, l.Phone, strings.Join(l.Counties, ", "))
		opts := runOpts{
			from: from, twiml: twiml, model: *model,
			hangUp: *hangUp, timeout: *timeout,
			endpoint: *endpoint, ingestToken: ingestToken, dryRun: *dryRun,
		}
		if err := readAndPush(l, tw, ai, opts); err != nil {
			fmt.Fprintf(os.Stderr, "  FAILED: %v\n", err)
			failed = append(failed, l.ID)
			continue
		}
	}

	if len(failed) > 0 {
		// A failed line pushes NOTHING. Its previous reading then ages past the
		// server's freshness gate and the facet reads UNKNOWN — the honest
		// outcome, and why this exits non-zero rather than publishing a guess.
		fatal("%d of %d line(s) failed: %s", len(failed), len(lines), strings.Join(failed, ", "))
	}
	fmt.Printf("\nAll %d line(s) read successfully.\n", len(lines))
}

// runOpts bundles the per-run settings shared by every line.
type runOpts struct {
	from, twiml, model    string
	hangUp, timeout       time.Duration
	endpoint, ingestToken string
	dryRun                bool
}

func readAndPush(l config.BurnLine, tw *twilio.Client, ai *openai.Client, o runOpts) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	reader := burnline.NewReader(tw, ai, burnline.Config{
		Phone: l.Phone, From: o.from, TwiMLURL: o.twiml,
		County: displayName(l), HangUpAfter: o.hangUp, Model: o.model,
	})

	reading, err := reader.Read(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("  status:     %s\n", reading.Status)
	fmt.Printf("  confidence: %d\n", reading.Confidence)
	fmt.Printf("  message:    %s\n", reading.Message)
	fmt.Printf("  observedAt: %s\n", reading.ObservedAt.Format(time.RFC3339))
	fmt.Printf("  transcript: %s\n", truncate(reading.Transcript, 240))

	if o.dryRun {
		fmt.Println("  (-dry-run: not pushing)")
		return nil
	}
	if err := push(ctx, o.endpoint, o.ingestToken, l.ID, reading); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	fmt.Printf("  pushed to %s\n", o.endpoint)
	return nil
}

// pushBody mirrors the ingest endpoint's wire shape.
type pushBody struct {
	LineID               string `json:"lineId"`
	Status               string `json:"status"`
	Message              string `json:"message"`
	CleanedTranscription string `json:"cleanedTranscription"`
	Confidence           int32  `json:"confidence"`
	ObservedAt           string `json:"observedAt"`
}

func push(ctx context.Context, endpoint, token, lineID string, r *burnline.Reading) error {
	body, err := json.Marshal(pushBody{
		LineID:               lineID,
		Status:               string(r.Status),
		Message:              r.Message,
		CleanedTranscription: r.Transcript,
		Confidence:           r.Confidence,
		ObservedAt:           r.ObservedAt.Format(time.RFC3339),
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
	if resp.StatusCode != http.StatusAccepted {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
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
