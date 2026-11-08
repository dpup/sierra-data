package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/config"
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

func TestCheckTelemetry(t *testing.T) {
	ok := config.MeshRelayTelemetryConfig{Gateways: []string{"my-test-box"},
		GridURL: "https://data.sierragridteam.org", IngestToken: "x"}
	require.NoError(t, checkTelemetry(testCtx(), ok))
	assert.True(t, ok.Enabled())

	noToken := ok
	noToken.IngestToken = ""
	require.NoError(t, checkTelemetry(testCtx(), noToken), "a local relay without the token is not an error")
	assert.False(t, noToken.Enabled(), "and it does not poll")

	badName := ok
	badName.Gateways = []string{"Not A Gateway"}
	assert.Error(t, checkTelemetry(testCtx(), badName))

	badURL := ok
	badURL.GridURL = "data.sierragridteam.org"
	assert.Error(t, checkTelemetry(testCtx(), badURL))
}
