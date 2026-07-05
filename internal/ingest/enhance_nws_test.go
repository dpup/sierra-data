package ingest

import (
	"io"
	"net/http"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/info.ersn.net/server/internal/config"
)

// fakeRoundTripper injects a canned OpenAI response (the repo's fake-HTTP
// convention, adapted to go-openai's http.Client seam) and records the
// request body for prompt assertions.
type fakeRoundTripper struct {
	resp     string
	err      error
	lastBody string
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	f.lastBody = string(body)
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(f.resp)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func newFakeNWSOpenAI(model string, rt *fakeRoundTripper) *openaiNWSEnhancer {
	cfg := openai.DefaultConfig("test-key")
	cfg.HTTPClient = &http.Client{Transport: rt}
	return &openaiNWSEnhancer{client: openai.NewClientWithConfig(cfg), model: model}
}

func chatCompletionJSON(content string) string {
	resp := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":` +
		content + `}}]}`
	return resp
}

func TestNewNWSEnhancerDisabledWithoutKey(t *testing.T) {
	assert.Nil(t, NewNWSEnhancer(config.OpenAIClient{Model: "gpt-5-mini"}))
	assert.NotNil(t, NewNWSEnhancer(config.OpenAIClient{APIKey: "sk-test", Model: "gpt-5-mini"}))
}

func TestNWSEnhance(t *testing.T) {
	rt := &fakeRoundTripper{
		resp: chatCompletionJSON(`"{\"summary\":\"Gusty winds and low humidity near Arnold through Friday evening.\"}"`),
	}
	e := newFakeNWSOpenAI("gpt-5-mini", rt)

	summary, err := e.Enhance(testCtx(),
		"Red Flag Warning until 8 PM PDT",
		"* WHAT...Gusty winds and low humidity.",
		[]string{"Calaveras County", "Arnold, CA"})
	require.NoError(t, err)
	assert.Equal(t, "Gusty winds and low humidity near Arnold through Friday evening.", summary)

	// Request assembly: model, low reasoning effort (gpt-5 family), the
	// structured-output schema, the policy prompt, and the grounding inputs.
	assert.Contains(t, rt.lastBody, `"model":"gpt-5-mini"`)
	assert.Contains(t, rt.lastBody, `"reasoning_effort":"low"`)
	assert.Contains(t, rt.lastBody, `"nws_alert_summary"`)
	assert.Contains(t, rt.lastBody, "translates, never asserts")
	assert.Contains(t, rt.lastBody, "Never paraphrase directives")
	assert.Contains(t, rt.lastBody, "Red Flag Warning until 8 PM PDT")
	assert.Contains(t, rt.lastBody, "Calaveras County")
}

func TestNWSEnhanceNonReasoningModelOmitsEffort(t *testing.T) {
	rt := &fakeRoundTripper{resp: chatCompletionJSON(`"{\"summary\":\"ok\"}"`)}
	e := newFakeNWSOpenAI("gpt-4o-mini", rt)

	_, err := e.Enhance(testCtx(), "h", "d", nil)
	require.NoError(t, err)
	assert.NotContains(t, rt.lastBody, "reasoning_effort")
}

func TestNWSEnhanceErrors(t *testing.T) {
	// Transport failure.
	e := newFakeNWSOpenAI("gpt-5-mini", &fakeRoundTripper{err: assert.AnError})
	_, err := e.Enhance(testCtx(), "h", "d", nil)
	assert.Error(t, err)

	// Well-formed response with an empty summary.
	e = newFakeNWSOpenAI("gpt-5-mini", &fakeRoundTripper{resp: chatCompletionJSON(`"{\"summary\":\"  \"}"`)})
	_, err = e.Enhance(testCtx(), "h", "d", nil)
	assert.ErrorContains(t, err, "empty summary")

	// Content that isn't the schema'd JSON.
	e = newFakeNWSOpenAI("gpt-5-mini", &fakeRoundTripper{resp: chatCompletionJSON(`"not json"`)})
	_, err = e.Enhance(testCtx(), "h", "d", nil)
	assert.ErrorContains(t, err, "parsing response")

	// No choices at all.
	e = newFakeNWSOpenAI("gpt-5-mini", &fakeRoundTripper{
		resp: `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`,
	})
	_, err = e.Enhance(testCtx(), "h", "d", nil)
	assert.ErrorContains(t, err, "no choices")
}
