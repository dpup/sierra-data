package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emptyList mirrors the shipped prefab.yaml: an empty flow list, followed by a
// commented example at the KEY's indent. That trailing comment is the trap —
// naive "append to the end of the section" logic splices the new entry into the
// middle of it.
const emptyList = `grid:
  dbPath: "./data/grid.db"
  ingest:
    # Bounds on one request.
    maxBodyBytes: 1048576
    reporters: []
    # Example — uncomment and fill in the hash from ` + "`make ingest-token`" + `:
    # reporters:
    #   - id: alan-pi
    #     tokenSha256: "<64 hex characters; NEVER the token itself>"

  meshcore:
    enabled: true
`

var populatedList = `grid:
  ingest:
    reporters:
      - id: alan-pi
        name: "Alan's repeater monitor"
        tokenSha256: "` + strings.Repeat("a", 64) + `"
        streams: ["mesh.repeater"]
        # Hand-tuned after the January outage; do not widen.
        staleAfter: "90m"
        placeIds: ["area:ebbetts-pass"]
    # Example — uncomment and fill in the hash:
    # reporters:

  meshcore:
    enabled: true
`

func spec(id string) reporterSpec {
	return reporterSpec{
		ID:          id,
		Name:        "Test monitor",
		Streams:     []string{"mesh.repeater"},
		TokenSha256: strings.Repeat("b", 64),
		StaleAfter:  "30m",
		MinInterval: "30s",
		Priority:    10,
	}
}

func TestAddToEmptyList(t *testing.T) {
	out, act, err := upsertReporter(emptyList, spec("new-pi"))
	require.NoError(t, err)
	assert.Equal(t, actionAdded, act)

	// `[]` must become a block list, or nothing can nest under it.
	assert.NotContains(t, out, "reporters: []")
	assert.Contains(t, out, "    reporters:\n      - id: new-pi")
	assert.Contains(t, out, `        tokenSha256: "`+strings.Repeat("b", 64)+`"`)

	// The entry must land BEFORE the commented example, not inside it.
	entryAt := strings.Index(out, "- id: new-pi")
	exampleAt := strings.Index(out, "# Example")
	require.Positive(t, exampleAt)
	assert.Less(t, entryAt, exampleAt, "entry was spliced into the commented example")

	// Everything else is untouched — this file is half prose and the comments
	// are the only record of why several values are what they are.
	assert.Contains(t, out, "    # Bounds on one request.")
	assert.Contains(t, out, `    #     tokenSha256: "<64 hex characters; NEVER the token itself>"`)
	assert.Contains(t, out, "  meshcore:\n    enabled: true")
	assert.Contains(t, out, `  dbPath: "./data/grid.db"`)
}

func TestAppendToPopulatedList(t *testing.T) {
	out, act, err := upsertReporter(populatedList, spec("second-pi"))
	require.NoError(t, err)
	assert.Equal(t, actionAdded, act)

	assert.Contains(t, out, "- id: alan-pi", "the existing reporter survives")
	assert.Contains(t, out, "- id: second-pi")
	assert.Less(t, strings.Index(out, "- id: alan-pi"), strings.Index(out, "- id: second-pi"))
	assert.Less(t, strings.Index(out, "- id: second-pi"), strings.Index(out, "# Example"))
	assert.Contains(t, out, `        placeIds: ["area:ebbetts-pass"]`)
}

func TestRotateKeepsEverySettingButTheHash(t *testing.T) {
	out, act, err := upsertReporter(populatedList, spec("alan-pi"))
	require.NoError(t, err)
	assert.Equal(t, actionRotated, act)

	assert.NotContains(t, out, strings.Repeat("a", 64), "the old hash is gone")
	assert.Contains(t, out, `        tokenSha256: "`+strings.Repeat("b", 64)+`"`)
	assert.Equal(t, 1, strings.Count(out, "- id: alan-pi"), "rotation must not duplicate the entry")

	// A rotation is a credential change, not a reset. Someone rotating a token
	// is not expecting to silently lose a hand-tuned staleAfter.
	assert.Contains(t, out, `        staleAfter: "90m"`)
	assert.Contains(t, out, `        placeIds: ["area:ebbetts-pass"]`)
	assert.Contains(t, out, "        # Hand-tuned after the January outage; do not widen.")
	assert.Contains(t, out, `        name: "Alan's repeater monitor"`, "the name is not overwritten")
}

func TestRotateMatchesTheRightEntry(t *testing.T) {
	two := `grid:
  ingest:
    reporters:
      - id: first-pi
        tokenSha256: "` + strings.Repeat("1", 64) + `"
      - id: second-pi
        tokenSha256: "` + strings.Repeat("2", 64) + `"

  meshcore:
    enabled: true
`
	out, act, err := upsertReporter(two, spec("second-pi"))
	require.NoError(t, err)
	assert.Equal(t, actionRotated, act)
	assert.Contains(t, out, strings.Repeat("1", 64), "the other reporter's token is untouched")
	assert.NotContains(t, out, strings.Repeat("2", 64))

	out, act, err = upsertReporter(two, spec("first-pi"))
	require.NoError(t, err)
	assert.Equal(t, actionRotated, act)
	assert.NotContains(t, out, strings.Repeat("1", 64))
	assert.Contains(t, out, strings.Repeat("2", 64))
}

// Splicing a credential into the wrong section would be a silent, dangerous
// success, so every ambiguity is an error instead.
func TestRefusesWhenItCannotBeSure(t *testing.T) {
	cases := map[string]string{
		"no ingest block":     "grid:\n  dbPath: x\n",
		"no reporters key":    "grid:\n  ingest:\n    maxItems: 10\n",
		"unsupported value":   "grid:\n  ingest:\n    reporters: &anchor\n",
		"two ingest blocks":   "grid:\n  ingest:\n    reporters: []\nother:\n  ingest:\n    reporters: []\n",
		"reporters elsewhere": "grid:\n  meshcore:\n    reporters: []\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := upsertReporter(src, spec("x"))
			require.Error(t, err)
		})
	}
}

func TestNameIsEscaped(t *testing.T) {
	s := spec("odd-pi")
	s.Name = `He said "hi" \ bye`
	out, _, err := upsertReporter(emptyList, s)
	require.NoError(t, err)
	assert.Contains(t, out, `        name: "He said \"hi\" \\ bye"`)
}

// The shipped config is the input this tool actually runs against, so edit it
// for real rather than only a hand-written fixture.
func TestAgainstTheRealPrefabYAML(t *testing.T) {
	src, err := os.ReadFile("../../prefab.yaml")
	require.NoError(t, err)

	// A deliberately synthetic id: asserting `added` against a real reporter name
	// would turn this test into a tripwire that fires the day someone actually
	// configures that reporter.
	const id = "zz-fixture-only"
	out, act, err := upsertReporter(string(src), spec(id))
	require.NoError(t, err)
	assert.Equal(t, actionAdded, act)
	assert.Contains(t, out, "      - id: "+id)

	// The entry must land inside the reporters list, not spill past the end of
	// the ingest block into the section that follows it.
	const nextSection = "  # MeshCore mesh-node presence source."
	require.Contains(t, out, nextSection)
	assert.Less(t, strings.Index(out, "      - id: "+id), strings.Index(out, nextSection))

	// Comments around the edit survive. (The stronger case — an entry spliced
	// into a COMMENTED-OUT example — is covered by TestAddToEmptyList, whose
	// fixture still carries one.)
	assert.Contains(t, out, "  # Authenticated push ingest")

	// Nothing outside the reporters list moved: same line count plus the entry,
	// and the sections on either side are intact.
	added := len(strings.Split(out, "\n")) - len(strings.Split(string(src), "\n"))
	assert.Equal(t, len(renderEntry(spec(id), 6)), added,
		"exactly the new entry's lines were added")
	assert.Contains(t, out, `    telemetryPersistInterval: "10m"`)
	assert.Contains(t, out, "  meshcore:")

	// Round-trip: rotating what we just added stays stable. Counted on real list
	// items only — the commented example carries the same text, which is exactly
	// the kind of near-miss that makes a naive substring search wrong here.
	rotated, act, err := upsertReporter(out, spec(id))
	require.NoError(t, err)
	assert.Equal(t, actionRotated, act)
	assert.Equal(t, 1, countEntries(rotated, id))
}

// countEntries counts real list entries with an id, ignoring commented lines.
func countEntries(src, id string) int {
	n := 0
	for _, l := range strings.Split(src, "\n") {
		if strings.TrimRight(l, " ") == "      - id: "+id {
			n++
		}
	}
	return n
}
