package alerts

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The model returns every fixed key, null where the input says nothing. Only
// stated facts survive, and a key outside the set (an older cache entry) is
// dropped, so metadata keys stay comparable across revisions.
func TestStructuredMetadataKeepsOnlyStatedFixedKeys(t *testing.T) {
	var s StructuredDescription
	require.NoError(t, json.Unmarshal([]byte(`{"additional_info": {
		"incident_type": "traffic collision", "injuries": "unknown",
		"vehicles_involved": null, "lanes_blocked": " ", "emergency_services": null,
		"work_type": null, "duration": null, "intersection": "SR 132 and N G St"}}`), &s))
	assert.Equal(t, map[string]string{"incident_type": "traffic collision", "injuries": "unknown"}, s.Metadata())
}

func TestSchemaListsEveryMetadataKey(t *testing.T) {
	var schema struct {
		Properties struct {
			AdditionalInfo struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"additional_info"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	raw, err := AlertEnhancementSchema.Schema.MarshalJSON()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.ElementsMatch(t, MetadataKeys, schema.Properties.AdditionalInfo.Required)
	assert.Len(t, schema.Properties.AdditionalInfo.Properties, len(MetadataKeys))
	assert.Contains(t, schema.Required, "additional_info", "strict mode needs every property required")
}

func TestCleanCondensedDropsOneTrailingStop(t *testing.T) {
	assert.Equal(t, "Traffic collision, injuries unknown", CleanCondensed("Traffic collision, injuries unknown."))
	assert.Equal(t, "Tire debris in one lane", CleanCondensed(" Tire debris in one lane "))
	assert.Equal(t, "Road closed...", CleanCondensed("Road closed..."))
}
