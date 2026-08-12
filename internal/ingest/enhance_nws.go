package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/dpup/sierra-data/internal/config"
)

// NWSEnhancement is the model output plus the I/O captured for transparency
// (what was sent / what came back), mirroring the incident enhancer.
type NWSEnhancement struct {
	Summary  string
	Request  string // the incident-specific user prompt sent to the model
	Response string // the raw structured JSON returned
}

// NWSEnhancer condenses an NWS alert into a short plain-language summary,
// localized against the supplied place names (spec §3.1: the place directory
// is the one permitted external grounding). Implementations must be safe for
// concurrent use; a nil NWSEnhancer disables enhancement entirely.
//
// `headline` is the composed card line (nws.Alert.ShortHeadline) — the product
// name and its reason — not CAP's issuance boilerplate. The model is told it
// is displayed above the summary and must not repeat it.
type NWSEnhancer interface {
	Enhance(ctx context.Context, headline, description string, placeNames []string) (NWSEnhancement, error)
}

// nwsSystemPrompt enforces the spec §3.1 enhancement policy: the model
// translates, never asserts. Quotable rules, in policy order.
//
// Rules 4-7 exist because the first version produced 865-character summaries
// on a card that has room for two lines. It was asked to "unwrap the
// WHAT/WHERE/WHEN/IMPACTS blocks into prose" with no ceiling, so it did:
// roughly 350 characters of one Fire Weather Watch summary was a transcription
// of seven forecast zones with their elevation bands — "Fire Zone 130 Sierra
// (Tehama-Plumas) Above 3000 ft; Fire Zone 132 …" — none of which are in this
// service's area, followed by the issuance and expiry stamps the card already
// displays. The reader of a regional summary is answering one question: does
// this affect me, here, today. Zone rosters and timestamps do not help them
// answer it, and they crowded out the part that did.
const nwsSystemPrompt = `You condense National Weather Service alerts for a local hazard dashboard.

Policy — enhancement translates, never asserts:
1. Your summary may contain no place, number, or instruction not present in the alert text or the supplied place-name list.
2. The place-name list is the only permitted external knowledge: use it to translate forecast-zone references into named local places. Never introduce a place that is in neither the alert nor the list.
3. Never paraphrase directives. If the alert instructs people to do something, say the alert includes instructions and that readers should consult the original; do not restate, reword, or summarize the instruction itself.
4. Write at most 2 sentences and at most 320 characters, unwrapping the hard-wrapped ALL-CAPS "* WHAT/WHERE/WHEN/IMPACTS" blocks into prose. Lead with the hazard itself and what it means on the ground — the reader is deciding whether this affects them where they are, today.
5. Never reproduce forecast-zone or fire-zone identifiers, zone numbers, or their elevation bands. A supplied place name lies inside the affected area — that is where the reader is, and it is why the name was supplied — so name it rather than describing the region generically. Only where no place name is supplied may you fall back to the alert's own general terms for the area.
6. Do not restate the issuing office, the time the alert was issued, or when it expires — all three are displayed alongside your summary. Give timing only in the alert's own relative words ("late Wednesday night through Thursday evening"), and only if it fits.
7. The headline, which names the product, is displayed directly above your summary. Do not open with the product's name or restate the headline — start with the hazard itself, and add what the headline does not say.
8. Add no advice, speculation, or urgency the alert does not itself state.`

// nwsSummarySchema is the structured-output contract: exactly one required
// string field, so a well-formed response can never be missing the summary.
var nwsSummarySchema = openai.ChatCompletionResponseFormatJSONSchema{
	Name:   "nws_alert_summary",
	Strict: true,
	Schema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {
				"type": "string",
				"description": "At most 2 sentences and at most 320 characters, condensing the alert per the policy. No zone identifiers, no timestamps, no repetition of the headline."
			}
		},
		"required": ["summary"],
		"additionalProperties": false
	}`),
}

// openaiNWSEnhancer is the production NWSEnhancer over OpenAI structured
// outputs (same client + gpt-5-family handling as internal/lib/alerts).
type openaiNWSEnhancer struct {
	client  *openai.Client
	model   string
	timeout time.Duration // 0 => rely on the caller's ctx
}

// NewNWSEnhancer builds the OpenAI-backed enhancer from the shared OpenAI
// client config. Returns nil (enhancement disabled) when no API key is set —
// callers treat a nil NWSEnhancer as "serve raw alerts".
func NewNWSEnhancer(cfg config.OpenAIClient) NWSEnhancer {
	if cfg.APIKey == "" {
		return nil
	}
	return &openaiNWSEnhancer{
		client:  openai.NewClient(cfg.APIKey),
		model:   cfg.Model,
		timeout: cfg.Timeout,
	}
}

// Enhance implements NWSEnhancer.
func (e *openaiNWSEnhancer) Enhance(ctx context.Context, headline, description string, placeNames []string) (NWSEnhancement, error) {
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	// The user prompt is data-only; all policy lives in the system prompt.
	// JSON-encode the untrusted alert text so it cannot masquerade as
	// instructions or break the prompt structure.
	input, err := json.Marshal(map[string]any{
		"headline":    headline,
		"description": description,
		"place_names": placeNames,
	})
	if err != nil {
		return NWSEnhancement{}, fmt.Errorf("nws enhancer: encoding input: %w", err)
	}
	userPrompt := "Summarize this NWS alert:\n" + string(input)

	// gpt-5-family compatibility mirrors internal/lib/alerts/enhancer.go: no
	// temperature, MaxCompletionTokens (budget also covers hidden reasoning
	// tokens, hence the headroom), reasoning_effort=low — condensing prose is
	// not a reasoning-heavy task.
	req := openai.ChatCompletionRequest{
		Model: e.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: nwsSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &nwsSummarySchema,
		},
		MaxCompletionTokens: 1500,
	}
	if isReasoningModel(e.model) {
		req.ReasoningEffort = "low"
	}

	resp, err := e.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return NWSEnhancement{}, fmt.Errorf("nws enhancer: %w", err)
	}
	if len(resp.Choices) == 0 {
		return NWSEnhancement{}, errors.New("nws enhancer: no choices in response")
	}

	rawResponse := resp.Choices[0].Message.Content
	var out struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(rawResponse), &out); err != nil {
		return NWSEnhancement{}, fmt.Errorf("nws enhancer: parsing response: %w", err)
	}
	if strings.TrimSpace(out.Summary) == "" {
		return NWSEnhancement{}, errors.New("nws enhancer: empty summary in response")
	}
	return NWSEnhancement{Summary: out.Summary, Request: userPrompt, Response: rawResponse}, nil
}

// isReasoningModel reports whether the model accepts reasoning_effort (gpt-5
// family / o-series); non-reasoning models (gpt-4o family) reject the param.
// Mirrors internal/lib/alerts/enhancer.go.
func isReasoningModel(model string) bool {
	return strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
}
