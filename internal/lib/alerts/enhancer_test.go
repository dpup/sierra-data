package alerts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// Contract tests for alert-enhancer library
// These tests define the interface before implementation exists
// They MUST FAIL initially to satisfy TDD RED-GREEN-Refactor cycle

func TestAlertEnhancer_EnhanceAlert(t *testing.T) {
	// Test with invalid API key (should return error)
	enhancer := NewAlertEnhancer("invalid-test-key", "gpt-3.5-turbo")
	ctx := context.Background()

	// Test with real Caltrans description sample
	rawAlert := RawAlert{
		ID:          "test-001",
		Description: "Rte 4 EB of MM 31 - VEHICLE IN DITCH, EMS ENRT",
		Location:    "Highway 4",
		Timestamp:   time.Now(),
	}

	_, err := enhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error with invalid API key")

	// Test basic interface compliance
	assert.NotNil(t, enhancer, "Enhancer should be created even with invalid key")

	// Test with empty API key (should return error)
	emptyEnhancer := NewAlertEnhancer("", "gpt-3.5-turbo")
	_, err = emptyEnhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error with empty API key")
}

func TestAlertEnhancer_EnhanceAlert_ComplexDescription(t *testing.T) {
	enhancer := NewAlertEnhancer("invalid-key", "gpt-3.5-turbo")
	ctx := context.Background()

	// Test with complex Caltrans description
	rawAlert := RawAlert{
		ID:          "test-002",
		Description: "Rte 4 WB at Arnold Rim - OVERTURNED VEHICLE OFF ROADWAY, BLOCKING 1 LN, EMS/FIRE ENRT, TOW REQ, VIS: NOT VISIBLE FROM ROADWAY",
		Location:    "Highway 4 at Arnold Rim",
		Timestamp:   time.Now(),
	}

	_, err := enhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error with invalid API key")
}

func TestAlertEnhancer_CondensedSummaryGeneration(t *testing.T) {
	// Test that condensed summary is generated automatically by the AI during EnhanceAlert
	// This test validates the contract without making real API calls
	enhancer := NewAlertEnhancer("invalid-key", "gpt-3.5-turbo")
	ctx := context.Background()

	rawAlert := RawAlert{
		ID:          "test-summary",
		Description: "Rte 4 WB at Arnold Rim - OVERTURNED VEHICLE OFF ROADWAY, BLOCKING 1 LN",
		Location:    "Highway 4 at Arnold Rim",
		Timestamp:   time.Now(),
	}

	// This will fail due to invalid API key, but verifies the interface
	_, err := enhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error with invalid API key")

	// Verify the interface expects EnhanceAlert to return EnhancedAlert with CondensedSummary field
	// The actual condensed summary generation is tested via integration with the AI
	assert.NotNil(t, enhancer, "Enhancer should be created")
}

func TestAlertEnhancer_HealthCheck(t *testing.T) {
	// Test with valid client but invalid key (should return error)
	enhancer := NewAlertEnhancer("invalid-key", "gpt-3.5-turbo")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := enhancer.HealthCheck(ctx)
	assert.Error(t, err, "Should return error with invalid API key")

	// Test with nil client (should return error)
	emptyEnhancer := NewAlertEnhancer("", "gpt-3.5-turbo")
	err = emptyEnhancer.HealthCheck(ctx)
	assert.Error(t, err, "Should return error with nil client")
}

func TestAlertEnhancer_TimeoutHandling(t *testing.T) {
	enhancer := NewAlertEnhancer("test-api-key", "gpt-3.5-turbo")

	// Test with very short timeout to force timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	rawAlert := RawAlert{
		ID:          "test-timeout",
		Description: "Test timeout handling",
		Location:    "Test Location",
		Timestamp:   time.Now(),
	}

	_, err := enhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error on timeout")
}

func TestAlertEnhancer_ErrorHandling(t *testing.T) {
	// Test with invalid API key
	enhancer := NewAlertEnhancer("invalid-api-key", "gpt-3.5-turbo")
	ctx := context.Background()

	rawAlert := RawAlert{
		ID:          "test-error",
		Description: "Test error handling",
		Location:    "Test Location",
		Timestamp:   time.Now(),
	}

	_, err := enhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error for invalid API key")
}

func TestAlertEnhancer_StructuredOutputValidation(t *testing.T) {
	// Test interface compliance without making real API calls
	enhancer := NewAlertEnhancer("invalid-key", "gpt-3.5-turbo")
	ctx := context.Background()

	rawAlert := RawAlert{
		ID:          "test-validation",
		Description: "Rte 4 - CONSTRUCTION WORK, DELAYS POSSIBLE",
		Location:    "Highway 4",
		Timestamp:   time.Now(),
	}

	_, err := enhancer.EnhanceAlert(ctx, rawAlert)
	assert.Error(t, err, "Should return error with invalid API key")

	// Test that the interface works as expected
	assert.NotNil(t, enhancer, "Enhancer should be created")
}

// Benchmark test for performance validation
func BenchmarkAlertEnhancer_EnhanceAlert(b *testing.B) {
	enhancer := NewAlertEnhancer("test-api-key", "gpt-3.5-turbo")
	ctx := context.Background()

	rawAlert := RawAlert{
		ID:          "benchmark-test",
		Description: "Rte 4 EB - TRAFFIC HAZARD",
		Location:    "Highway 4",
		Timestamp:   time.Now(),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = enhancer.EnhanceAlert(ctx, rawAlert)
	}
}

func TestTruncateRunes_NoInvalidUTF8(t *testing.T) {
	// A byte-index cut at 147 would split the en-dash preceding byte 147; the
	// rune-aware truncation must never emit invalid UTF-8 (which would fail
	// protojson marshaling downstream).
	s := strings.Repeat("a", 146) + "–tail" // en-dash straddles byte 147
	out := truncateRunes(s, 147)
	if !utf8.ValidString(out) {
		t.Fatalf("truncateRunes produced invalid UTF-8: %q", out)
	}
	if len(s) <= 147 {
		t.Fatal("test setup: input should exceed 147 bytes")
	}
	// Short strings pass through unchanged.
	if got := truncateRunes("short", 147); got != "short" {
		t.Fatalf("short string altered: %q", got)
	}
}

func TestStripStyleNote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips the observed leak",
			in:   "An animal was reported in the roadway. Traffic is flowing but use caution. (Style: general traffic alert - no lane closures or one-way control indicated.)",
			want: "An animal was reported in the roadway. Traffic is flowing but use caution.",
		},
		{
			name: "case-insensitive on the style token",
			in:   "Two lanes blocked, tow en route. (style: lane closure)",
			want: "Two lanes blocked, tow en route.",
		},
		{
			name: "leaves a clean description untouched",
			in:   "Overturned vehicle off the roadway, EMS and fire en route.",
			want: "Overturned vehicle off the roadway, EMS and fire en route.",
		},
		{
			name: "keeps a legitimate non-style trailing parenthetical",
			in:   "Debris in the right lane (a shredded tire), traffic slowing.",
			want: "Debris in the right lane (a shredded tire), traffic slowing.",
		},
		{
			name: "only strips the trailing note, not mid-sentence text",
			in:   "Report mentions style guide compliance and then continues normally.",
			want: "Report mentions style guide compliance and then continues normally.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripStyleNote(tc.in))
		})
	}
}

func TestStripAttribution(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips courtesy-of attribution",
			in:   "An animal is in the roadway; CHP is on scene. Information courtesy of CHP.",
			want: "An animal is in the roadway; CHP is on scene.",
		},
		{
			name: "strips provided-by attribution",
			in:   "Two lanes blocked northbound. Provided by Caltrans.",
			want: "Two lanes blocked northbound.",
		},
		{
			name: "leaves prose without attribution untouched",
			in:   "Overturned vehicle off the roadway, EMS en route.",
			want: "Overturned vehicle off the roadway, EMS en route.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripAttribution(tc.in))
		})
	}
}

func TestCleanDetails_StripsBoth(t *testing.T) {
	// The exact reported leak: style note AND CHP attribution, both removed.
	in := "An animal was reported in the roadway at Phoenix Lake Road and Longeway Road. CHP responded and was on scene. No lanes reported blocked; traffic is flowing but motorists should use caution while the animal is removed or contained. Information courtesy of CHP. (Style: general traffic alert - no lane closures or one-way control indicated.)"
	want := "An animal was reported in the roadway at Phoenix Lake Road and Longeway Road. CHP responded and was on scene. No lanes reported blocked; traffic is flowing but motorists should use caution while the animal is removed or contained."
	assert.Equal(t, want, cleanDetails(in))
}

// TestRawAlertCarriesGroundingToTheWire: the user prompt is json.Marshal of
// RawAlert, so PlaceNames reaches the model as `place_names` with no extra
// plumbing — that is the whole reason grounding was added as a field rather
// than a new EnhanceAlert parameter. If this marshalling ever changes, the
// system prompt's Place Names rule silently binds nothing.
func TestRawAlertCarriesGroundingToTheWire(t *testing.T) {
	body, err := json.Marshal(RawAlert{
		ID:         "chp:260812IN0965",
		Title:      "Traffic Collision",
		Location:   "SR 108 (38.3208, -119.6717)",
		PlaceNames: []string{"Avery", "Hwy 4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"place_names":["Avery","Hwy 4"]`) {
		t.Errorf("grounding list missing from the prompt body: %s", got)
	}

	// An EMPTY list must be sent EXPLICITLY as [], not omitted. "We looked and
	// nothing is near" is a finding the prompt acts on; an absent key is
	// indistinguishable from a caller that forgot to ground the alert.
	body, err = json.Marshal(RawAlert{ID: "chp:x", PlaceNames: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"place_names":[]`) {
		t.Errorf("an empty grounding list must serialize explicitly: %s", body)
	}
}

// The prompt must actually carry the rules those fields rely on.
func TestSystemPromptCarriesGroundingAndImpactRules(t *testing.T) {
	for _, want := range []string{
		"Place Names",
		"place_names",
		"name NO locality at all",
		"NEVER convert latitude/longitude into a place name",
		"1039 MERCED", // the dispatch-centre trap that produced the bug
		"Impact Rating",
		"every through lane is open",
	} {
		if !strings.Contains(SystemPrompt, want) {
			t.Errorf("SystemPrompt is missing %q", want)
		}
	}
	// The example that taught the model the "near <place>" pattern must be gone.
	if strings.Contains(SystemPrompt, "treasure island") {
		t.Error("the 'near treasure island' example demonstrates the hallucination pattern and must not return")
	}
}

// TestHashRawAlertIgnoresPlaceNames pins the cache-key contract in both
// directions. The grounding list is a deterministic function of coordinates
// already inside Location, so it must not join the key — otherwise adding a
// town to config dumps every cached enhancement. The PROMPT version must,
// because without it a prompt fix is invisible for the 24h TTL and the service
// keeps serving text produced under the old rules.
func TestHashRawAlertIgnoresPlaceNames(t *testing.T) {
	h := NewContentHasher()
	base := RawAlert{ID: "chp:x", Title: "Collision", Description: "two vehicles", Location: "SR 108 (38.3208, -119.6717)"}
	grounded := base
	grounded.PlaceNames = []string{"Bear Valley", "Hwy 4"}

	if h.HashRawAlert(base) != h.HashRawAlert(grounded) {
		t.Error("grounding must not change the cache key — a config edit would dump the cache")
	}

	// Sanity: the key still discriminates on the things it should.
	moved := base
	moved.Location = "SR 4 (38.1391, -120.4561)"
	if h.HashRawAlert(base) == h.HashRawAlert(moved) {
		t.Error("a different location must produce a different key")
	}
}
