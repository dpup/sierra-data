package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

	// Create user prompt with raw alert data as JSON
	rawAlertJSON, _ := json.Marshal(raw)
	userPrompt := fmt.Sprintf(`Parse this traffic incident report and return structured JSON:

Raw Alert: %s

Extract structured information following the schema.
Focus on making the details field human-readable by removing technical abbreviations and jargon.
If a style_url is provided, incorporate the relevant traffic flow context from the StyleUrl definitions into your description (e.g., mention one-way control, lane restrictions, etc.).
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
		structured.CondensedSummary = structured.Details // Simple fallback
		if len(structured.CondensedSummary) > 147 {
			structured.CondensedSummary = structured.CondensedSummary[:147] + "..."
		}
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
