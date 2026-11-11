package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The examples from prefab's own TransformEnv doc comment. This mapping lives in
// prefab's internal/ package (not importable), so it is replicated here — if
// prefab ever changes it, this test is the tripwire.
func TestEnvToConfigKey(t *testing.T) {
	cases := map[string]string{
		"PF__SERVER__PORT":             "server.port",
		"PF__SERVER__INCOMING_HEADERS": "server.incomingHeaders",
		"PF__FOO_BAR__BAZ":             "fooBar.baz",
		// The pair at the heart of the bug: only the underscore form resolves.
		"PF__MESH_RELAY__DB_PATH": "meshRelay.dbPath",
		"PF__MESH_RELAY__DBPATH":  "meshRelay.dbpath",
	}
	for env, want := range cases {
		assert.Equal(t, want, envToConfigKey(env), env)
	}
}

func TestConfigKeyToEnv(t *testing.T) {
	assert.Equal(t, "PF__MESH_RELAY__DB_PATH", configKeyToEnv([]string{"meshRelay", "dbPath"}))
	assert.Equal(t, "PF__GRID__DATABASE__MAX_CONNS", configKeyToEnv([]string{"grid", "database", "maxConns"}))
	assert.Equal(t, "PF__SERVER__PORT", configKeyToEnv([]string{"server", "port"}))
	assert.Equal(t, "PF__GRID__WILDFIRE__PLACE_BUFFER_METERS",
		configKeyToEnv([]string{"grid", "wildfire", "placeBufferMeters"}))
}

func TestValidateEnvOverridesAcceptsRealKeys(t *testing.T) {
	require.NoError(t, ValidateEnvOverrides([]string{
		"PF__GRID__DATABASE__URL=postgres://localhost/grid",
		"PF__GRID__DATABASE__MAX_CONNS=8",
		"PF__GRID__DATABASE__LOCK_TIMEOUT=90s",
		"PF__MESH_RELAY__DB_PATH=/data/relay.db",
		"PF__GRID__WILDFIRE__MARGIN_DEGREES=0.75",
		"PF__GRID__WILDFIRE__PLACE_BUFFER_METERS=20000",
		"PF__GRID__MESHCORE__USERNAME=someone",
		"PF__GRID__INGEST__REPORTER_TOKEN_SHA256__RELAY=abc",
		"PF__MESH_RELAY__TELEMETRY__INGEST_TOKEN=x",
		"PF__MESH_RELAY__SECRET_KEY=x",
		"PF__MESH_RELAY__TELEMETRY__GRID_URL=http://localhost:8181",
		"PF__OPENAI__API_KEY=sk-x",
		"PF__GOOGLE_ROUTES__API_KEY=x",
		"PF__WEATHER__NWS__USER_AGENT=The Grid",
		// A map key: grid.sources.<id>.<field> is validated down to the leaf.
		"PF__GRID__SOURCES__USGS__POLL_INTERVAL=5m",
		"PF__GRID__SOURCES__CALFIRE__DISAPPEARANCE=resolve",
		// Prefab's own namespace stays prefab's business.
		"PF__SERVER__PORT=8080",
		"PF__SERVER__SECURITY__CORS_MAX_AGE=72h",
		// Not a PF__ var at all.
		"PATH=/usr/bin",
		"HOME=/root",
	}))
}

// The regression this whole check exists for.
func TestValidateEnvOverridesCatchesMissingUnderscore(t *testing.T) {
	err := ValidateEnvOverrides([]string{"PF__MESH_RELAY__DBPATH=/data/relay.db"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PF__MESH_RELAY__DBPATH")
	assert.Contains(t, err.Error(), `"meshRelay.dbpath"`)
	assert.Contains(t, err.Error(), "did you mean PF__MESH_RELAY__DB_PATH")

	err = ValidateEnvOverrides([]string{"PF__GRID__DATABASE__MAXCONNS=WAL"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean PF__GRID__DATABASE__MAX_CONNS")

	// Nested, and one we just added — proves it isn't a hardcoded list.
	err = ValidateEnvOverrides([]string{"PF__GRID__WILDFIRE__PLACEBUFFERMETERS=20000"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean PF__GRID__WILDFIRE__PLACE_BUFFER_METERS")
}

func TestValidateEnvOverridesCatchesUnknownKeys(t *testing.T) {
	// A wholly unknown leaf in our namespace: reported, with no suggestion.
	err := ValidateEnvOverrides([]string{"PF__GRID__NO_SUCH_KEY=1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PF__GRID__NO_SUCH_KEY")
	assert.NotContains(t, err.Error(), "did you mean")

	// Too deep: a scalar addressed as an object.
	require.Error(t, ValidateEnvOverrides([]string{"PF__MESH_RELAY__DB_PATH__EXTRA=x"}))

	// A bad leaf under a map key is caught too.
	require.Error(t, ValidateEnvOverrides([]string{"PF__GRID__SOURCES__USGS__POLLINTERVAL=5m"}))
}

// Every offender is reported at once, so a fix is one pass rather than a game of
// whack-a-mole across restarts.
func TestValidateEnvOverridesReportsAllProblems(t *testing.T) {
	err := ValidateEnvOverrides([]string{
		"PF__MESH_RELAY__DBPATH=/data/relay.db",
		"PF__GRID__DATABASE__MAXCONNS=WAL",
		"PF__SERVER__PORT=8080",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PF__MESH_RELAY__DBPATH")
	assert.Contains(t, err.Error(), "PF__GRID__DATABASE__MAXCONNS")
	assert.NotContains(t, err.Error(), "PF__SERVER__PORT")
}

// Slice-valued config (hazards.areas, grid.meshcore.brokers) cannot be targeted
// by env at all, so it must never be flagged.
func TestValidateEnvOverridesIgnoresSliceConfig(t *testing.T) {
	require.NoError(t, ValidateEnvOverrides([]string{
		"PF__HAZARDS__AREAS=whatever",
		"PF__GRID__MESHCORE__BOUNDS__0__MIN_LATITUDE=36.0",
	}))
}

// The SQLite-era Grid keys are gone. A deploy that still sets them must fail
// to start rather than run against whatever grid.database.url says.
func TestValidateEnvOverridesRejectsLegacyGridStoreKeys(t *testing.T) {
	for _, kv := range []string{"PF__GRID__DB_PATH=/data/grid.db", "PF__GRID__JOURNAL_MODE=TRUNCATE",
		"PF__GRID__CACHE_SIZE_MB=24", "PF__GRID__LOCK_TIMEOUT=90s", "PF__GRID__MAX_OPEN_CONNS=4"} {
		assert.Error(t, ValidateEnvOverrides([]string{kv}), kv)
	}
}
