package alerts

import "testing"

// The CHP feed re-serves the same incident with ticking timestamps ("Last
// updated: 12:55pm", per-line dispatch times). Those must not change the
// content hash, or the 24h enhancement cache never hits and every refresh
// re-buys the same OpenAI call.
func TestHashRawAlert_StableAcrossTimestampTicks(t *testing.T) {
	h := NewContentHasher()

	v1 := RawAlert{
		ID:    "260704ST0248",
		Title: "Traffic Collision-No Injury",
		Description: "Jul 4 2026 12:27PM Hwy 49 / Parrotts Ferry Rd\n" +
			"Jul 4 2026 12:32PM [2] GRY HOND CIV VS TREE\n" +
			"Last updated: 07/04/2026 12:55pm",
		Location: "Hwy 49 (38.0671, -120.5402)",
		StyleUrl: "#chp",
	}
	v2 := v1
	v2.Description = "Jul 4 2026 12:27PM Hwy 49 / Parrotts Ferry Rd\n" +
		"Jul 4 2026 12:32PM [2] GRY HOND CIV VS TREE\n" +
		"Last updated: 07/04/2026 1:10pm" // only the stamp ticked

	if h.HashRawAlert(v1) != h.HashRawAlert(v2) {
		t.Error("hash must be stable when only embedded timestamps change")
	}

	// A genuinely new detail line must still change the hash.
	v3 := v1
	v3.Description = v1.Description + "\nJul 4 2026 1:05PM [3] TOW ENRT"
	if h.HashRawAlert(v1) == h.HashRawAlert(v3) {
		t.Error("hash must change when a new detail line is appended")
	}
}
