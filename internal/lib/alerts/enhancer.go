package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

// alertEnhancer implements the AlertEnhancer interface using OpenAI
type alertEnhancer struct {
	client *openai.Client
	model  string
}

// NewAlertEnhancer creates a new AlertEnhancer implementation
func NewAlertEnhancer(apiKey, model string) AlertEnhancer {
	if apiKey == "" {
		return &alertEnhancer{client: nil, model: model} // Will cause errors - for testing
	}

	client := openai.NewClient(apiKey)
	return &alertEnhancer{
		client: client,
		model:  model,
	}
}

// EnhanceAlert enhances a raw alert using OpenAI GPT with structured output
func (a *alertEnhancer) EnhanceAlert(ctx context.Context, raw RawAlert) (EnhancedAlert, error) {
	if a.client == nil {
		return EnhancedAlert{}, errors.New("OpenAI client not initialized - invalid API key")
	}

	// Normalize nil to an empty slice BEFORE marshalling, so the model always
	// receives the key: an absent place_names is indistinguishable from a caller
	// that forgot to ground the alert, whereas an explicit [] says "we looked
	// and nothing is near" — which is what the prompt's Place Names rule keys
	// off. raw is a value copy, so the caller's struct is untouched.
	if raw.PlaceNames == nil {
		raw.PlaceNames = []string{}
	}
	// Create user prompt with raw alert data as JSON
	rawAlertJSON, _ := json.Marshal(raw)
	userPrompt := fmt.Sprintf(`Parse this traffic incident report and return structured JSON:

Raw Alert: %s

Extract structured information following the schema.
Focus on making the details field human-readable by removing technical abbreviations and jargon.
If a style_url is provided, use the StyleUrl definitions ONLY to set road_status and to phrase the description naturally (e.g. mention one-way control or lane restrictions when they actually apply). Never name the KML style or append a meta/classification note such as "(Style: ...)" to details or any other field — describe the situation, not its category.
For the condensed summary, follow the examples provided - do NOT include location, keep it under 120 characters.`,
		string(rawAlertJSON))

	// All models we configure (gpt-4o family, gpt-5 family) support JSON Schema
	// structured outputs.
	responseFormat := &openai.ChatCompletionResponseFormat{
		Type:       openai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &AlertEnhancementSchema,
	}

	// Make OpenAI API call with structured output request.
	// gpt-5-family compatibility: those models reject non-default `temperature`
	// and the legacy `max_tokens` param (replaced by `max_completion_tokens`),
	// and their completion budget also covers hidden reasoning tokens — hence
	// the headroom and low reasoning effort (this is jargon expansion +
	// classification, not a reasoning-heavy task).
	req := openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: SystemPrompt,
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: userPrompt,
			},
		},
		ResponseFormat:      responseFormat,
		MaxCompletionTokens: 3000,
	}
	if isReasoningModel(a.model) {
		req.ReasoningEffort = "low"
	}
	resp, err := a.client.CreateChatCompletion(ctx, req)

	if err != nil {
		return EnhancedAlert{}, fmt.Errorf("OpenAI API error: %w", err)
	}

	if len(resp.Choices) == 0 {
		return EnhancedAlert{}, errors.New("no response from OpenAI API")
	}

	// Parse the JSON response
	var structured StructuredDescription
	jsonResponse := resp.Choices[0].Message.Content
	if err := json.Unmarshal([]byte(jsonResponse), &structured); err != nil {
		return EnhancedAlert{}, fmt.Errorf("failed to parse OpenAI JSON response: %w", err)
	}

	// Defense-in-depth: despite the prompt, the model occasionally decorates
	// details with an internal style classification ("... (Style: general
	// traffic alert - no lane closures indicated.)") or a redundant source
	// attribution ("... Information courtesy of CHP.") — the latter duplicates
	// the source tag shown separately. Strip both before details reaches
	// traveler-facing text.
	structured.Details = cleanDetails(structured.Details)

	// Validate required fields
	if structured.Details == "" {
		structured.Details = raw.Description // Fallback to original
	}
	if structured.Location.Description == "" {
		structured.Location.Description = raw.Location // Fallback to original location string
	}
	// Ensure coordinates are populated from raw alert location if missing
	if structured.Location.Latitude == 0 && structured.Location.Longitude == 0 {
		// This shouldn't happen if AI follows instructions, but safety fallback
		structured.Location.Description = raw.Location
	}

	// Validate enum fields
	if !isValidImpact(structured.Impact) {
		structured.Impact = "unknown"
	}
	// Use AI-generated condensed summary (trust the AI to follow instructions)
	// Only fallback to a simple format if completely missing
	if structured.CondensedSummary == "" {
		structured.CondensedSummary = truncateRunes(structured.Details, 147) // Simple fallback
	}

	// Create enhanced alert. Request/Response record the exact model I/O (the
	// incident-specific user prompt and the raw structured response) so clients
	// can show what was sent and what came back.
	enhanced := EnhancedAlert{
		ID:                    raw.ID,
		OriginalDescription:   raw.Description,
		StructuredDescription: structured,
		CondensedSummary:      structured.CondensedSummary,
		ProcessedAt:           time.Now(),
		Request:               userPrompt,
		Response:              jsonResponse,
	}

	return enhanced, nil
}

// HealthCheck verifies OpenAI API connectivity and rate limits
func (a *alertEnhancer) HealthCheck(ctx context.Context) error {
	if a.client == nil {
		return errors.New("OpenAI client not initialized")
	}

	// Make a minimal API call to test connectivity. MaxCompletionTokens (not
	// the legacy MaxTokens) so this works on gpt-5-family models too; small
	// headroom because reasoning models spend completion budget on hidden
	// reasoning tokens.
	_, err := a.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: a.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: "Test",
			},
		},
		MaxCompletionTokens: 16,
	})

	if err != nil {
		return fmt.Errorf("OpenAI API health check failed: %w", err)
	}

	return nil
}

// Helper functions

// styleNoteRe matches a trailing "(Style: ...)" meta annotation the model
// sometimes appends to details (the KML style is an internal classification
// input, not traveler-facing text). Anchored to the end and gated on a leading
// "style" so genuine parentheticals elsewhere in the prose are left intact.
var styleNoteRe = regexp.MustCompile(`(?i)\s*\(style\b[^)]*\)\s*$`)

// attributionRe matches a redundant source-attribution sentence the model
// sometimes adds ("Information courtesy of CHP.", "Provided by Caltrans.") — the
// source is already shown as a tag, so this is duplication. Gated on the
// attribution lead-in and a sentence-ending period to avoid touching prose.
var attributionRe = regexp.MustCompile(`(?i)\s*(?:information\s+)?(?:courtesy of|provided by)\b[^.]*\.`)

// stripStyleNote removes a single trailing "(Style: ...)" note.
func stripStyleNote(s string) string {
	return strings.TrimSpace(styleNoteRe.ReplaceAllString(s, ""))
}

// stripAttribution removes redundant "courtesy of / provided by <source>"
// attribution sentences.
func stripAttribution(s string) string {
	return strings.TrimSpace(attributionRe.ReplaceAllString(s, ""))
}

// cleanDetails removes the model's occasional style-note and source-attribution
// decorations from the traveler-facing details text.
func cleanDetails(s string) string {
	return strings.TrimSpace(stripAttribution(stripStyleNote(s)))
}

// truncateRunes returns s unchanged if it is within maxBytes, otherwise the
// longest rune-aligned prefix that fits in maxBytes plus an ellipsis. Slicing
// on a raw byte index can split a multi-byte UTF-8 rune, which would make the
// value invalid UTF-8 and fail protojson/proto marshaling of the resulting
// proto3 string field (headline/condensed_summary).
func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// isReasoningModel reports whether the configured model is an OpenAI reasoning
// model (gpt-5 family / o-series), which accepts the reasoning_effort param.
// Non-reasoning models (gpt-4o family) reject it.
func isReasoningModel(model string) bool {
	return strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
}

// isValidImpact validates impact enum values
func isValidImpact(impact string) bool {
	validImpacts := []string{"none", "light", "moderate", "severe"}
	for _, valid := range validImpacts {
		if impact == valid {
			return true
		}
	}
	return false
}
