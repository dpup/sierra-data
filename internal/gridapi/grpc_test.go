package gridapi

import "testing"

// weakListTag is a conditional-GET validator for the list/query RPCs; a false
// collision (two different filter sets sharing a tag) would serve a stale 304
// and hide a hazard update, so pin its distinctness + stability.
func TestWeakListTag(t *testing.T) {
	base := weakListTag("v1", "place", "wildfire", "ACTIVE")

	// Same inputs -> same tag (stable within a version).
	if got := weakListTag("v1", "place", "wildfire", "ACTIVE"); got != base {
		t.Errorf("same inputs gave different tags: %q vs %q", base, got)
	}

	// It is a weak validator.
	if base[:2] != "W/" {
		t.Errorf("expected a weak validator, got %q", base)
	}

	// Any change — version, a field value, field count, or a boundary shift
	// between adjacent fields — must change the tag.
	for name, got := range map[string]string{
		"version":        weakListTag("v2", "place", "wildfire", "ACTIVE"),
		"value":          weakListTag("v1", "place", "earthquake", "ACTIVE"),
		"fewer fields":   weakListTag("v1", "place", "wildfire"),
		"empty vs unset": weakListTag("v1", "place", "wildfire", "ACTIVE", ""),
		"boundary shift": weakListTag("v1", "place", "wildfireACTIVE"), // must differ from ["wildfire","ACTIVE"]
	} {
		if got == base {
			t.Errorf("%s: tag collided with base %q", name, base)
		}
	}
}
