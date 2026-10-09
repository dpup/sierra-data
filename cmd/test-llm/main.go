// Command test-llm probes an OpenAI-compatible chat/audio provider with the
// exact request shapes the service sends, and writes a Markdown report.
//
// It exists for one question: can a provider other than OpenAI (today,
// LunaRoute at https://gw.lunaroute.com/v1) take over the three AI surfaces
// unchanged? The checks are the ones docs/design/lunaroute-compatibility.md §6
// lists — strict json_schema with the production schemas, which token-budget
// field is honoured, whether reasoning can be switched off, latency, audio
// transcription, and the 429 behaviour at the concurrency limit. Each check is
// reported, never asserted: this is a diagnostic, like test-pge, not a test.
//
// Usage:
//
//	OPENAI_API_KEY=lr_... OPENAI_BASE_URL=https://gw.lunaroute.com/v1 \
//	  test-llm -models glm-5.3,deepseek-4.1-flash -audio recording.mp3
//	OPENAI_API_KEY=sk-... test-llm -models gpt-5-mini -transcribe-model whisper-1
//
// .github/workflows/llm-probe.yml runs it on demand from CI, where the keys
// live, against both providers so the report carries a baseline.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/dpup/sierra-data/internal/clients/burnline"
	"github.com/dpup/sierra-data/internal/ingest"
	"github.com/dpup/sierra-data/internal/lib/alerts"
)

func main() {
	var (
		baseURL     = flag.String("base-url", os.Getenv("OPENAI_BASE_URL"), "API base URL (default $OPENAI_BASE_URL; empty = api.openai.com)")
		models      = flag.String("models", "glm-5.3,deepseek-4.1-flash,glm-5.3-flash", "comma-separated chat model ids to probe")
		transcribe  = flag.String("transcribe-model", "whisper-large-v3", "transcription model id (whisper-1 on OpenAI)")
		audio       = flag.String("audio", "", "audio file to transcribe (mp3/wav); skipped when empty")
		runs        = flag.Int("runs", 3, "repetitions of the incident request for the latency figures")
		concurrency = flag.Int("concurrency", 0, "fire this many parallel requests to observe the concurrency limit (0 = skip)")
		timeout     = flag.Duration("timeout", 180*time.Second, "per-request timeout")
		effort      = flag.String("reasoning-effort", "", "send reasoning_effort on every production-shape request (what prefab.yaml would configure; empty = omit)")
		noThinking  = flag.Bool("no-thinking", false, "send chat_template_kwargs.enable_thinking=false on every production-shape request")
		out         = flag.String("out", "", "also write the Markdown report to this file")
	)
	flag.Parse()

	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		fmt.Fprintln(os.Stderr, "test-llm: OPENAI_API_KEY is required")
		os.Exit(2)
	}
	cfg := openai.DefaultConfig(key)
	if *baseURL != "" {
		cfg.BaseURL = strings.TrimRight(*baseURL, "/")
	}
	cfg.HTTPClient = &http.Client{Timeout: *timeout}
	client := openai.NewClientWithConfig(cfg)

	p := &probe{
		client:     client,
		baseURL:    cfg.BaseURL,
		key:        key,
		http:       &http.Client{Timeout: *timeout},
		timeout:    *timeout,
		effort:     *effort,
		noThinking: *noThinking,
	}

	var ids []string
	for _, m := range strings.Split(*models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			ids = append(ids, m)
		}
	}

	r := &report{}
	r.h1("LLM provider probe")
	r.kv("Base URL", cfg.BaseURL)
	r.kv("Chat models", strings.Join(ids, ", "))
	r.kv("Transcription model", *transcribe)
	r.kv("Reasoning tuning on production requests", tuningLabel(*effort, *noThinking))
	r.kv("Run at", time.Now().UTC().Format(time.RFC3339))

	p.catalog(r, ids)
	for _, id := range ids {
		p.chatModel(r, id, *runs)
	}
	if *audio != "" {
		p.audio(r, *transcribe, *audio)
	} else {
		r.h2("Audio transcription")
		r.p("Skipped: no `-audio` file given.")
	}
	if *concurrency > 0 && len(ids) > 0 {
		p.concurrency(r, ids[0], *concurrency)
	}

	fmt.Print(r.String())
	if *out != "" {
		if err := os.WriteFile(*out, []byte(r.String()), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "test-llm: write %s: %v\n", *out, err)
			os.Exit(1)
		}
	}
}

type probe struct {
	client     *openai.Client
	baseURL    string
	key        string
	http       *http.Client
	timeout    time.Duration
	effort     string // -reasoning-effort
	noThinking bool   // -no-thinking
}

// tuned applies the configured reasoning knobs to a production-shape request.
// Production sets reasoning_effort from config (today: "low" on the gpt-5
// family), so the probe's "as in production" checks carry the same setting.
func (p *probe) tuned(req openai.ChatCompletionRequest) openai.ChatCompletionRequest {
	if p.effort != "" {
		req.ReasoningEffort = p.effort
	}
	if p.noThinking {
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	return req
}

func tuningLabel(effort string, noThinking bool) string {
	var parts []string
	if effort != "" {
		parts = append(parts, "reasoning_effort="+effort)
	}
	if noThinking {
		parts = append(parts, "chat_template_kwargs.enable_thinking=false")
	}
	if len(parts) == 0 {
		return "none (provider default)"
	}
	return strings.Join(parts, ", ")
}

func (p *probe) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), p.timeout)
}

// ---------------------------------------------------------------------------
// Catalog

func (p *probe) catalog(r *report, want []string) {
	r.h2("Catalog")
	ctx, cancel := p.ctx()
	defer cancel()
	list, err := p.client.ListModels(ctx)
	if err != nil {
		r.p("`GET /models` failed: " + describe(err))
	} else {
		have := map[string]bool{}
		for _, m := range list.Models {
			have[m.ID] = true
		}
		r.p(fmt.Sprintf("`GET /models` listed %d models.", len(list.Models)))
		for _, id := range want {
			r.li(fmt.Sprintf("`%s`: %s", id, yesno(have[id], "listed", "NOT listed")))
		}
	}

	// LunaRoute documents a separate listing for transcription models; OpenAI
	// has none (404 there is expected).
	status, body, err := p.rawGet("/audio/transcriptions/models")
	switch {
	case err != nil:
		r.p("`GET /audio/transcriptions/models`: " + err.Error())
	case status == http.StatusOK:
		r.p("`GET /audio/transcriptions/models` → 200:")
		r.code(truncate(body, 1500))
	default:
		r.p(fmt.Sprintf("`GET /audio/transcriptions/models` → %d: %s", status, truncate(body, 300)))
	}
}

// ---------------------------------------------------------------------------
// Chat checks, per model

// incidentFixture is a CHP-style incident on Hwy 4 near Arnold, in the shape
// services/incidents.go hands the enhancer. The place list offers Arnold and
// the corridor; everything in forbiddenPlaces is plausible nearby geography
// the model must NOT introduce (the grounding rule the prompt was tuned for).
var incidentFixture = alerts.RawAlert{
	ID:    "250911GG0206",
	Title: "Traffic Hazard",
	Description: "Sep 11 2025 10:54AM 1125-Traffic Hazard SR4 E / Blagen Rd " +
		"Sep 11 2025 10:55AM [4] LRG TREE BRANCH IN #2 LN " +
		"Sep 11 2025 10:58AM [6] 1039 CT " +
		"Sep 11 2025 11:02AM [8] CALTRANS ENRT Last updated: 09/11/2025 11:05am",
	Location:   "SR4 E / Blagen Rd (38.2545, -120.3510)",
	StyleUrl:   "#incidentIcon",
	Timestamp:  time.Date(2025, 9, 11, 18, 5, 0, 0, time.UTC),
	PlaceNames: []string{"Arnold", "Hwy 4 Murphys–Arnold corridor"},
}

var forbiddenPlaces = []string{"Merced", "Stockton", "Sacramento", "Modesto", "Sonora",
	"Angels Camp", "Dorrington", "Avery", "Hathaway Pines", "Camp Connell", "Bear Valley"}

// nwsFixture is the Fire Weather Watch from TestNWSEnhancerLive.
const nwsHeadline = "Fire Weather Watch — thunderstorms and strong outflow winds"
const nwsDescription = `The National Weather Service in Sacramento has issued a Fire
Weather Watch for thunderstorms and strong outflow winds, which
is in effect from late Wednesday night through Thursday evening.

* Affected Area...Fire Zone 130 Sierra (Tehama-Plumas) Above
3000 ft, Fire Zone 132 Sierra (Yuba-Placer) 3000-5000 ft, Fire
Zone 133 Sierra (Sierra-Placer) Above 5000 ft, Fire Zone 135
Sierra 3000-5000 ft, Fire Zone 136 Sierra (El Dorado-Amador)
Above 5000 ft, Fire Zone 138 Sierra (Cal-Tuo) 3000-5000 ft and
Fire Zone 139 Sierra (Cal-Tuo) Above 5000 ft.

* Thunderstorms...Isolated to scattered coverage of a mix of wet
and dry thunderstorms expected. Lightning strikes may also occur
outside of main precipitation cores.

* Outflow Winds...Gusty and erratic outflow winds could occur near
any thunderstorm development.

* Impacts...Lightning can create new fire starts and may combine
with strong outflow winds to cause a fire to rapidly grow in
size and intensity.`

// burnTranscript is an elevation-restricted burn day, the classification the
// extraction prompt calls out as the one people get wrong (expected: orange).
const burnTranscript = "Thank you for calling the Calaveras County Air Pollution Control District " +
	"burn information line. Today, Thursday, September 11, is a permissive burn day " +
	"at 3500 feet elevation or more. Below 3500 feet burning is not permitted today. " +
	"A burn permit from Cal Fire is required. Thank you for calling the Calaveras " +
	"County Air Pollution Control District burn information line. Today, Thursday, " +
	"September 11, is a permissive burn day at 3500 feet elevation or more."

type result struct {
	err        error
	latency    time.Duration
	finish     string
	content    string
	reasoning  int // len(reasoning_content)
	completion int // usage.completion_tokens
	reasonTok  int // usage.completion_tokens_details.reasoning_tokens
	warning    string
}

func (p *probe) chat(req openai.ChatCompletionRequest) result {
	ctx, cancel := p.ctx()
	defer cancel()
	start := time.Now()
	resp, err := p.client.CreateChatCompletion(ctx, req)
	res := result{err: err, latency: time.Since(start)}
	if err != nil {
		return res
	}
	res.warning = resp.Header().Get("X-LunaRoute-Warning")
	res.completion = resp.Usage.CompletionTokens
	if d := resp.Usage.CompletionTokensDetails; d != nil {
		res.reasonTok = d.ReasoningTokens
	}
	if len(resp.Choices) == 0 {
		res.err = errors.New("no choices in response")
		return res
	}
	c := resp.Choices[0]
	res.finish = string(c.FinishReason)
	res.content = c.Message.Content
	res.reasoning = len(c.Message.ReasoningContent)
	return res
}

// incidentRequest mirrors internal/lib/alerts/enhancer.go exactly (keep in
// sync): system prompt, JSON-marshalled RawAlert in the user prompt, strict
// schema, max_completion_tokens 3000, no temperature.
func incidentRequest(model string) openai.ChatCompletionRequest {
	raw, _ := json.Marshal(incidentFixture)
	user := fmt.Sprintf(`Parse this traffic incident report and return structured JSON:

Raw Alert: %s

Extract structured information following the schema.
Focus on making the details field human-readable by removing technical abbreviations and jargon.
If a style_url is provided, use the StyleUrl definitions ONLY to set road_status and to phrase the description naturally (e.g. mention one-way control or lane restrictions when they actually apply). Never name the KML style or append a meta/classification note such as "(Style: ...)" to details or any other field — describe the situation, not its category.
For the condensed summary, follow the examples provided - do NOT include location, keep it under 120 characters.`, string(raw))
	return openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: alerts.SystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: user},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &alerts.AlertEnhancementSchema,
		},
		MaxCompletionTokens: 3000,
	}
}

// portableIncidentRequest is incidentRequest with additional_info redefined as
// a list of {key, value} pairs. Everything else (prompt, strictness, budget)
// is identical, so a pass here isolates the schema construct.
func portableIncidentRequest(model string) openai.ChatCompletionRequest {
	encoded, err := json.Marshal(alerts.AlertEnhancementSchema.Schema)
	if err != nil {
		panic(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		panic(err)
	}
	props := schema["properties"].(map[string]any)
	props["additional_info"] = map[string]any{
		"type":        "array",
		"description": "Structured facts as key/value pairs (keys: alphanumeric/._/- only)",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":   map[string]any{"type": "string"},
				"value": map[string]any{"type": "string"},
			},
			"required":             []string{"key", "value"},
			"additionalProperties": false,
		},
	}
	schema["required"] = append(schema["required"].([]any), "additional_info")
	raw, _ := json.Marshal(schema)
	req := incidentRequest(model)
	req.ResponseFormat = &openai.ChatCompletionResponseFormat{
		Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
			Name: "alert_enhancement_portable", Strict: true, Schema: json.RawMessage(raw),
		},
	}
	return req
}

// nwsRequest mirrors internal/ingest/enhance_nws.go.
func nwsRequest(model string) openai.ChatCompletionRequest {
	input, _ := json.Marshal(map[string]any{
		"headline": nwsHeadline, "description": nwsDescription,
		"place_names": []string{"Ebbetts Pass Corridor"},
	})
	return openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: ingest.NWSSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: "Summarize this NWS alert:\n" + string(input)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &ingest.NWSSummarySchema,
		},
		MaxCompletionTokens: 1500,
	}
}

// burnRequest mirrors internal/clients/burnline/reader.go extract.
func burnRequest(model string) openai.ChatCompletionRequest {
	return openai.ChatCompletionRequest{
		Model:       model,
		Temperature: 0.1,
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: burnline.ExtractPrompt("Calaveras County APCD burn line", "Thursday, September 11, 2025", burnTranscript),
		}},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &burnline.ExtractSchema,
		},
	}
}

func (p *probe) chatModel(r *report, model string, runs int) {
	r.h2("Model `" + model + "`")

	// 1. The incident schema, verbatim — the complex one (patternProperties,
	//    maxLength, nullable types).
	r.h3("Strict json_schema: incident enhancement")
	inc := p.chat(p.tuned(incidentRequest(model)))
	p.describeResult(r, inc)
	var incOut alerts.StructuredDescription
	if inc.err == nil {
		if err := json.Unmarshal([]byte(inc.content), &incOut); err != nil {
			r.li("**Parse: FAIL** — " + err.Error())
		} else {
			r.li("Parse: ok")
			scoreIncident(r, incOut)
		}
		r.details("raw response", inc.content)
	}

	// 1b. The same request with the portable schema variant (§5 of the design
	//     doc): additional_info as a list of {key, value} pairs instead of an
	//     object with patternProperties and no properties, which the Outlines
	//     grammar engine behind deepseek-4.1-flash rejects ("Unsupported JSON
	//     Schema structure false").
	r.h3("Strict json_schema: incident enhancement, portable additional_info")
	port := p.chat(p.tuned(portableIncidentRequest(model)))
	p.describeResult(r, port)
	if port.err == nil {
		var o struct {
			Details        string `json:"details"`
			AdditionalInfo []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"additional_info"`
		}
		if err := json.Unmarshal([]byte(port.content), &o); err != nil {
			r.li("**Parse: FAIL** — " + err.Error())
		} else {
			r.li(fmt.Sprintf("Parse: ok; %d additional_info pairs; details %d chars", len(o.AdditionalInfo), len(o.Details)))
			if len(o.AdditionalInfo) == 0 {
				r.li("**No additional_info pairs** — the model skipped the metadata under the list form (the prompt describes it as an object)")
			}
		}
		r.details("raw response", port.content)
	}

	// 2. The one-field NWS schema.
	r.h3("Strict json_schema: NWS summary")
	nws := p.chat(p.tuned(nwsRequest(model)))
	p.describeResult(r, nws)
	if nws.err == nil {
		var o struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal([]byte(nws.content), &o); err != nil {
			r.li("**Parse: FAIL** — " + err.Error())
		} else {
			scoreNWS(r, o.Summary)
		}
		r.details("raw response", nws.content)
	}

	// 3. The burn extraction schema (+ temperature).
	r.h3("Strict json_schema: burn-line extraction (expected `orange`)")
	burn := p.chat(p.tuned(burnRequest(model)))
	p.describeResult(r, burn)
	if burn.err == nil {
		var o struct {
			Status               string  `json:"status"`
			Message              string  `json:"message"`
			CleanedTranscription string  `json:"cleanedTranscription"`
			Confidence           float64 `json:"confidence"`
		}
		if err := json.Unmarshal([]byte(burn.content), &o); err != nil {
			r.li("**Parse: FAIL** — " + err.Error())
		} else {
			r.li(fmt.Sprintf("status `%s` (%s), confidence %g, message: %s",
				o.Status, yesno(o.Status == "orange", "correct", "**WRONG**"), o.Confidence, o.Message))
			if o.Confidence > 0 && o.Confidence <= 1 {
				r.li("**Confidence on the wrong scale** (a 0-1 fraction; the prompt asks for 0-100 and reader.go would store it as 0)")
			}
			if degenerate(o.CleanedTranscription) {
				r.li(fmt.Sprintf("**Degenerate cleanedTranscription** %q — reader.go would fall back to the raw transcript", o.CleanedTranscription))
			}
		}
		r.details("raw response", burn.content)
	}

	// 4. Which token-budget field is honoured. A 48-token budget on a prompt
	//    that wants ~300 words must either stop short (finish_reason length,
	//    few completion tokens) or be ignored.
	r.h3("Token budget field")
	long := func() []openai.ChatCompletionMessage {
		return []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser,
			Content: "Write about 300 words on how chain controls work on Sierra passes."}}
	}
	for _, c := range []struct {
		name string
		req  openai.ChatCompletionRequest
	}{
		{"max_completion_tokens=48", openai.ChatCompletionRequest{Model: model, Messages: long(), MaxCompletionTokens: 48}},
		{"max_tokens=48", openai.ChatCompletionRequest{Model: model, Messages: long(), MaxTokens: 48}},
	} {
		res := p.chat(c.req)
		if res.err != nil {
			r.li(fmt.Sprintf("`%s`: request failed — %s", c.name, describe(res.err)))
			continue
		}
		honoured := res.finish == "length" || res.completion <= 64
		r.li(fmt.Sprintf("`%s`: %s (finish_reason=%s, completion_tokens=%d, content=%d chars)",
			c.name, yesno(honoured, "honoured", "**ignored**"), res.finish, res.completion, len(res.content)))
	}

	// 5. Reasoning control. Baseline is check 1; try the two knobs go-openai
	//    can send.
	r.h3("Reasoning control (incident request, no tuning vs each knob)")
	base := p.chat(incidentRequest(model))
	if base.err != nil {
		r.li("provider default: request failed — " + describe(base.err))
	} else {
		r.li(fmt.Sprintf("provider default: reasoning_content=%d chars, reasoning_tokens=%d, completion_tokens=%d, finish_reason=%s, %s",
			base.reasoning, base.reasonTok, base.completion, base.finish, base.latency.Round(100*time.Millisecond)))
	}
	for _, c := range []struct {
		name string
		mod  func(*openai.ChatCompletionRequest)
	}{
		{"reasoning_effort=low", func(q *openai.ChatCompletionRequest) { q.ReasoningEffort = "low" }},
		{"chat_template_kwargs.enable_thinking=false", func(q *openai.ChatCompletionRequest) {
			q.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
		}},
	} {
		req := incidentRequest(model)
		c.mod(&req)
		res := p.chat(req)
		if res.err != nil {
			r.li(fmt.Sprintf("`%s`: **rejected** — %s", c.name, describe(res.err)))
			continue
		}
		var parsed alerts.StructuredDescription
		ok := json.Unmarshal([]byte(res.content), &parsed) == nil && parsed.Details != ""
		r.li(fmt.Sprintf("`%s`: accepted; reasoning_content=%d chars, reasoning_tokens=%d, completion_tokens=%d, %s, output %s",
			c.name, res.reasoning, res.reasonTok, res.completion, res.latency.Round(100*time.Millisecond),
			yesno(ok, "valid", "**unusable**")))
		if ok {
			scoreIncident(r, parsed)
			r.details("raw response ("+c.name+")", res.content)
		}
		// The placeholder failures seen in runs 3 and 4 were on the NWS summary
		// and the burn transcript, never the incident, so each knob is also
		// judged on those two, independently of how the tuned checks above
		// were configured.
		nreq := nwsRequest(model)
		c.mod(&nreq)
		if nres := p.chat(nreq); nres.err != nil {
			r.li(fmt.Sprintf("`%s` NWS: request failed — %s", c.name, describe(nres.err)))
		} else {
			var o struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal([]byte(nres.content), &o) != nil {
				r.li(fmt.Sprintf("`%s` NWS: **unparseable**", c.name))
			} else {
				r.li(fmt.Sprintf("`%s` NWS: %s", c.name, nres.latency.Round(100*time.Millisecond)))
				scoreNWS(r, o.Summary)
			}
		}
		breq := burnRequest(model)
		c.mod(&breq)
		if bres := p.chat(breq); bres.err != nil {
			r.li(fmt.Sprintf("`%s` burn: request failed — %s", c.name, describe(bres.err)))
		} else {
			var o struct {
				Status               string  `json:"status"`
				CleanedTranscription string  `json:"cleanedTranscription"`
				Confidence           float64 `json:"confidence"`
			}
			if json.Unmarshal([]byte(bres.content), &o) != nil {
				r.li(fmt.Sprintf("`%s` burn: **unparseable**", c.name))
			} else {
				r.li(fmt.Sprintf("`%s` burn: status `%s` (%s), confidence %g, transcript %s, %s", c.name, o.Status,
					yesno(o.Status == "orange", "correct", "**WRONG**"), o.Confidence,
					yesno(!degenerate(o.CleanedTranscription), "whole", "**placeholder**"),
					bres.latency.Round(100*time.Millisecond)))
			}
		}
	}

	// 6. Latency over N runs of the production incident request.
	r.h3(fmt.Sprintf("Latency (%d runs, incident request as sent in production)", runs))
	var ds []time.Duration
	failures := 0
	for i := 0; i < runs; i++ {
		res := p.chat(p.tuned(incidentRequest(model)))
		if res.err != nil {
			failures++
			continue
		}
		ds = append(ds, res.latency)
	}
	if len(ds) > 0 {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		r.li(fmt.Sprintf("min %s · median %s · max %s · failures %d/%d",
			ds[0].Round(100*time.Millisecond), ds[len(ds)/2].Round(100*time.Millisecond),
			ds[len(ds)-1].Round(100*time.Millisecond), failures, runs))
	} else {
		r.li(fmt.Sprintf("every run failed (%d/%d)", failures, runs))
	}
}

func (p *probe) describeResult(r *report, res result) {
	if res.err != nil {
		r.li("**Request failed**: " + describe(res.err))
		return
	}
	r.li(fmt.Sprintf("200 in %s; finish_reason=%s; completion_tokens=%d (reasoning %d); reasoning_content=%d chars; content=%d chars",
		res.latency.Round(100*time.Millisecond), res.finish, res.completion, res.reasonTok, res.reasoning, len(res.content)))
	if res.finish == "length" && strings.TrimSpace(res.content) == "" {
		r.li("**Empty content with finish_reason=length** — the budget was spent on reasoning.")
	}
	if res.warning != "" {
		r.li("X-LunaRoute-Warning: " + res.warning)
	}
}

// scoreIncident applies the checks the prompt was tuned against. Invented
// geography is the one that matters most; it is a heuristic here (a fixed list
// of plausible neighbours) and the raw output is printed for hand scoring.
func scoreIncident(r *report, o alerts.StructuredDescription) {
	okEnum := func(v string, allowed ...string) bool {
		for _, a := range allowed {
			if v == a {
				return true
			}
		}
		return false
	}
	r.li(fmt.Sprintf("impact `%s` %s · road_status `%s` %s · chain_status `%s` %s",
		o.Impact, yesno(okEnum(o.Impact, "none", "light", "moderate", "severe"), "ok", "**invalid**"),
		o.RoadStatus, yesno(okEnum(o.RoadStatus, "open", "restricted", "closed"), "ok", "**invalid**"),
		o.ChainStatus, yesno(okEnum(o.ChainStatus, "none", "r1", "r2", "active_unspecified"), "ok", "**invalid**")))
	r.li(fmt.Sprintf("condensed_summary %d chars %s: %s", len(o.CondensedSummary),
		yesno(len(o.CondensedSummary) > 0 && len(o.CondensedSummary) <= 120, "ok", "**out of bounds**"), o.CondensedSummary))
	text := o.Details + " " + o.Location.Description + " " + o.CondensedSummary
	var invented []string
	for _, pl := range forbiddenPlaces {
		if strings.Contains(text, pl) {
			invented = append(invented, pl)
		}
	}
	if len(invented) > 0 {
		r.li("**Invented geography**: " + strings.Join(invented, ", "))
	} else {
		r.li("No forbidden place names (heuristic list) — hand-check the details below")
	}
	if strings.Contains(strings.ToLower(o.Details), "(style") || strings.Contains(strings.ToLower(o.Details), "courtesy of") {
		r.li("**Decoration present** (style note / attribution) — enhancer.go strips it, but the prompt asked for none")
	}
	r.li(fmt.Sprintf("time_reported=%q last_update=%q (expect 2025-09-11T10:54:00-07:00 / 11:05)", o.TimeReported, o.LastUpdate))
}

func scoreNWS(r *report, s string) {
	if degenerate(s) {
		r.li(fmt.Sprintf("**Degenerate summary** %q — a placeholder, not a summary (seen from glm-5.3 with thinking fully disabled)", s))
		return
	}
	sentences := strings.Count(s, ". ") + 1
	r.li(fmt.Sprintf("summary %d chars, ~%d sentences %s", len(s), sentences,
		yesno(len(s) <= 320 && sentences <= 2, "ok", "**over the 2-sentence / 320-char cap**")))
	if strings.Contains(s, "Zone 1") || strings.Contains(s, "Fire Zone") {
		r.li("**Zone identifiers leaked** into the summary")
	}
	if strings.Contains(s, "Ebbetts") {
		r.li("Names the supplied place (good)")
	}
	r.li("summary: " + s)
}

// ---------------------------------------------------------------------------
// Audio

func (p *probe) audio(r *report, model, path string) {
	r.h2("Audio transcription")
	data, err := os.ReadFile(path)
	if err != nil {
		r.p("read " + path + ": " + err.Error())
		return
	}
	ctx, cancel := p.ctx()
	defer cancel()
	start := time.Now()
	// Same prompt shape and params as burnline.Reader.transcribe.
	resp, err := p.client.CreateTranscription(ctx, openai.AudioRequest{
		Model:    model,
		FilePath: path,
		Reader:   bytes.NewReader(data),
		Prompt: "This is a recording of the Calaveras County burn information line. " +
			"Key terms: Calaveras County, Cal Fire, burn day, permissive burn day, " +
			"elevation restrictions (3500 feet), permits, burn barrel.",
		Temperature: 0.1,
		Language:    "en",
	})
	if err != nil {
		r.p(fmt.Sprintf("`POST /audio/transcriptions` (model `%s`, %d bytes) failed: %s", model, len(data), describe(err)))
		return
	}
	r.p(fmt.Sprintf("`POST /audio/transcriptions` (model `%s`, %d bytes) → 200 in %s", model, len(data), time.Since(start).Round(100*time.Millisecond)))
	r.code(resp.Text)
}

// ---------------------------------------------------------------------------
// Concurrency: raw HTTP so the 429 headers are visible (go-openai's error
// types drop them).

func (p *probe) concurrency(r *report, model string, n int) {
	r.h2(fmt.Sprintf("Concurrency: %d parallel requests to `%s`", n, model))
	// No token-budget field on purpose: OpenAI's gpt-5 family rejects
	// max_tokens and other gateways may not know max_completion_tokens, and
	// a 400 would hide the 429 this check exists to observe. The prompt is
	// short enough to bound the spend by itself.
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Count slowly from one to forty, one number per line."}},
	})
	type outcome struct {
		status     int
		retryAfter string
		code       string
		latency    time.Duration
		err        error
	}
	outs := make([]outcome, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			req, _ := http.NewRequest(http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+p.key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := p.http.Do(req)
			o := outcome{latency: time.Since(start), err: err}
			if err == nil {
				o.status = resp.StatusCode
				o.retryAfter = resp.Header.Get("Retry-After")
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				var env struct {
					Error struct {
						Code any `json:"code"`
					} `json:"error"`
				}
				if json.Unmarshal(b, &env) == nil && env.Error.Code != nil {
					o.code = fmt.Sprint(env.Error.Code)
				}
				o.latency = time.Since(start)
			}
			outs[i] = o
		}(i)
	}
	wg.Wait()
	for i, o := range outs {
		if o.err != nil {
			r.li(fmt.Sprintf("#%d: transport error after %s: %v", i+1, o.latency.Round(100*time.Millisecond), o.err))
			continue
		}
		line := fmt.Sprintf("#%d: HTTP %d in %s", i+1, o.status, o.latency.Round(100*time.Millisecond))
		if o.code != "" {
			line += " code=" + o.code
		}
		if o.retryAfter != "" {
			line += " Retry-After=" + o.retryAfter
		}
		r.li(line)
	}
}

// ---------------------------------------------------------------------------
// Helpers

func (p *probe) rawGet(path string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(b), nil
}

// describe renders a go-openai error with its HTTP status and provider code.
func describe(err error) string {
	var api *openai.APIError
	if errors.As(err, &api) {
		return fmt.Sprintf("HTTP %d code=%v: %s", api.HTTPStatusCode, api.Code, api.Message)
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return fmt.Sprintf("HTTP %d: %v", reqErr.HTTPStatusCode, reqErr.Err)
	}
	return err.Error()
}

// degenerate reports a placeholder answer: empty, "...", or too short to be
// the field it stands in for. Length checks alone pass these.
func degenerate(s string) bool {
	t := strings.Trim(strings.TrimSpace(s), ".…-– ")
	return len(t) < 20
}

func yesno(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// report accumulates GitHub-flavoured Markdown (it is pasted into the
// workflow's step summary, which renders <details>).
type report struct{ b strings.Builder }

func (r *report) String() string { return r.b.String() }
func (r *report) h1(s string)    { fmt.Fprintf(&r.b, "# %s\n\n", s) }
func (r *report) h2(s string)    { fmt.Fprintf(&r.b, "\n## %s\n\n", s) }
func (r *report) h3(s string)    { fmt.Fprintf(&r.b, "\n### %s\n\n", s) }
func (r *report) p(s string)     { fmt.Fprintf(&r.b, "%s\n\n", s) }
func (r *report) li(s string)    { fmt.Fprintf(&r.b, "- %s\n", s) }
func (r *report) kv(k, v string) { fmt.Fprintf(&r.b, "- **%s**: %s\n", k, v) }
func (r *report) code(s string)  { fmt.Fprintf(&r.b, "```\n%s\n```\n\n", s) }
func (r *report) details(sum, s string) {
	fmt.Fprintf(&r.b, "\n<details><summary>%s</summary>\n\n```json\n%s\n```\n\n</details>\n\n", sum, s)
}
