package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// An omitted key must not drop the safety check; a negative one is an explicit
// opt-out and must survive verbatim (clamping it to the default would silently
// re-enable a check an operator turned off).
func TestMeshcoreSilence(t *testing.T) {
	assert.Equal(t, DefaultMeshcoreSilenceAfter, MeshcoreConfig{}.Silence())
	assert.Equal(t, 5*time.Minute, MeshcoreConfig{SilenceAfter: 5 * time.Minute}.Silence())
	assert.Equal(t, -time.Second, MeshcoreConfig{SilenceAfter: -time.Second}.Silence())
}
