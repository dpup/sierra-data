package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/config"
)

func TestRelayConfigFrom(t *testing.T) {
	cfg, err := relayConfigFrom(config.MeshRelayConfig{Admins: []config.MeshRelayAdmin{
		{Name: "dpup", TokenSHA256: strings.Repeat("AB", 32)},
	}, LeaseDuration: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("ab", 32), cfg.BootstrapAdmins["dpup"])
	assert.Equal(t, time.Minute, cfg.LeaseDuration)

	for _, bad := range []config.MeshRelayAdmin{
		{Name: "dpup", TokenSHA256: "short"},
		{Name: "dpup", TokenSHA256: "smc_rawtokenpastedbymistake"},
		{Name: "Not A Slug", TokenSHA256: strings.Repeat("ab", 32)},
	} {
		_, err := relayConfigFrom(config.MeshRelayConfig{Admins: []config.MeshRelayAdmin{bad}})
		assert.Error(t, err, "%+v", bad)
	}
}
