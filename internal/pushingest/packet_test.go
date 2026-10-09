package pushingest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpup/sierra-data/internal/clients/meshcore"
	"github.com/dpup/sierra-data/internal/config"
)

// fakeSink records what the handler hands to the registry and answers with a
// scripted outcome per packet, in order.
type fakeSink struct {
	outcomes []meshcore.PacketOutcome
	calls    []sinkCall
}

type sinkCall struct {
	payload string
	source  string
	gateway string
}

func (f *fakeSink) IngestEnvelope(payload []byte, source, gateway string) (meshcore.PacketOutcome, error) {
	f.calls = append(f.calls, sinkCall{string(payload), source, gateway})
	i := len(f.calls) - 1
	if i >= len(f.outcomes) {
		return meshcore.PacketAccepted, nil
	}
	out := f.outcomes[i]
	if out == meshcore.PacketAccepted || out == meshcore.PacketIgnored {
		return out, nil
	}
	return out, fmt.Errorf("scripted %s", out)
}

func packetRegistry(t *testing.T, sink PacketSink, mutate ...func(*config.Reporter)) *Registry {
	t.Helper()
	both := func(rep *config.Reporter) { rep.Streams = []string{MeshStream, MeshPacketStream} }
	r := testRegistry(t, append([]func(*config.Reporter){both}, mutate...)...)
	if sink != nil {
		r.SetPacketSink(sink)
	}
	return r
}

func packetBody(observer string, packets ...string) string {
	return fmt.Sprintf(`{"schema_version":1,"generated_at":"2026-10-09T05:00:00Z","observer":%q,"packets":[%s]}`,
		observer, strings.Join(packets, ","))
}

const (
	advertPacket = `{"packet_type":4,"raw":"1100aabb","SNR":"8.5","RSSI":"-95"}`
	textPacket   = `{"packet_type":2,"raw":"0800cc"}`
)

func TestPacketStreamIsOffWithoutASink(t *testing.T) {
	r := packetRegistry(t, nil)
	w := post(r, testToken, MeshPacketStream, packetBody("", advertPacket))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not enabled")
}

func TestPacketStreamHandsEachPacketToTheRegistry(t *testing.T) {
	sink := &fakeSink{outcomes: []meshcore.PacketOutcome{
		meshcore.PacketAccepted, meshcore.PacketIgnored, meshcore.PacketMalformed, meshcore.PacketRejected,
	}}
	r := packetRegistry(t, sink)
	w := post(r, testToken, MeshPacketStream,
		packetBody("E3635C65DCD443E7", advertPacket, textPacket, `{"packet_type":4,"raw":"zz"}`, advertPacket))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	var resp ingestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, MeshPacketStream, resp.Stream)
	assert.Equal(t, 1, resp.Accepted, "accepted counts adverts applied, not packets received")
	require.Len(t, resp.Warnings, 2, "a non-advert is fine; a dropped packet is a warning")
	assert.Contains(t, resp.Warnings[0], "packets[2]: malformed")
	assert.Contains(t, resp.Warnings[1], "packets[3]: rejected")

	require.Len(t, sink.calls, 4)
	for _, c := range sink.calls {
		assert.Equal(t, "reporter:alan-pi", c.source, "the reporter occupies a broker's slot")
		assert.Equal(t, "e3635c65dcd443e7", c.gateway, "the observer key, lowercased, is the default gateway")
	}
	assert.JSONEq(t, advertPacket, sink.calls[0].payload, "the envelope goes through verbatim")

	// The reporter is alive for health purposes, on this stream alone.
	h := r.Health(MeshPacketStream)
	require.Len(t, h, 1)
	assert.Equal(t, ReporterOK, h[0].State)
	assert.EqualValues(t, 1, h[0].Reports)
}

func TestPacketStreamDefaultsTheGatewayToTheReporter(t *testing.T) {
	sink := &fakeSink{}
	r := packetRegistry(t, sink)
	w := post(r, testToken, MeshPacketStream, packetBody("", advertPacket))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Len(t, sink.calls, 1)
	assert.Equal(t, "alan-pi", sink.calls[0].gateway)
}

func TestPacketStreamRejectsBadEnvelopes(t *testing.T) {
	sink := &fakeSink{}
	r := packetRegistry(t, sink)
	r.cfg.MaxItems = 2

	cases := map[string]string{
		"wrong schema":   `{"schema_version":2,"packets":[]}`,
		"not json":       `{"schema_version":1,`,
		"too many":       packetBody("", advertPacket, advertPacket, advertPacket),
		"bad observer":   packetBody("not-hex!", advertPacket),
		"short observer": packetBody("ab", advertPacket),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := post(r, testToken, MeshPacketStream, body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
	assert.Empty(t, sink.calls, "a rejected envelope hands nothing to the registry")
	h := r.Health(MeshPacketStream)
	require.Len(t, h, 1)
	assert.Equal(t, ReporterUnknown, h[0].State, "rejected reports never count as accepted")
	assert.NotEmpty(t, h[0].LastError)
}

func TestPacketStreamCapsWarnings(t *testing.T) {
	outcomes := make([]meshcore.PacketOutcome, maxPacketWarnings+5)
	packets := make([]string, len(outcomes))
	for i := range outcomes {
		outcomes[i] = meshcore.PacketMalformed
		packets[i] = `{"packet_type":4,"raw":"zz"}`
	}
	r := packetRegistry(t, &fakeSink{outcomes: outcomes})
	w := post(r, testToken, MeshPacketStream, packetBody("", packets...))
	require.Equal(t, http.StatusAccepted, w.Code)
	var resp ingestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Warnings, maxPacketWarnings+1)
	assert.Equal(t, "... and 5 more packets dropped", resp.Warnings[maxPacketWarnings])
}

// TestRateLimitIsPerStream: one reporter may run a telemetry monitor and a
// packet forwarder as two processes; the one must not 429 the other.
func TestRateLimitIsPerStream(t *testing.T) {
	r := packetRegistry(t, &fakeSink{}, func(rep *config.Reporter) { rep.MinInterval = time.Hour })
	assert.Equal(t, http.StatusAccepted, post(r, testToken, MeshStream, sampleReport).Code)
	assert.Equal(t, http.StatusAccepted, post(r, testToken, MeshPacketStream, packetBody("", advertPacket)).Code,
		"the telemetry report must not rate-limit the first packet report")
	assert.Equal(t, http.StatusTooManyRequests, post(r, testToken, MeshPacketStream, packetBody("", advertPacket)).Code)
	assert.Equal(t, http.StatusTooManyRequests, post(r, testToken, MeshStream, sampleReport).Code)
}

func TestPacketStreamRequiresItsOwnGrant(t *testing.T) {
	r := testRegistry(t) // mesh.repeater only
	r.SetPacketSink(&fakeSink{})
	w := post(r, testToken, MeshPacketStream, packetBody("", advertPacket))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestMeshReportsSeesPacketReporters pins what the normalizer reads: a packet
// forwarder is counted as live, listed for its source row, and suppresses the
// sweep while STALE — and never contributes node reports, because its packets
// are already in the registry.
func TestMeshReportsSeesPacketReporters(t *testing.T) {
	r := testRegistry(t, func(rep *config.Reporter) { rep.Streams = []string{MeshPacketStream} })
	r.SetPacketSink(&fakeSink{})
	now := time.Date(2026, 10, 9, 5, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	snap := r.MeshReports()
	assert.Empty(t, snap.Reporters, "not a mesh.repeater reporter")
	require.Len(t, snap.PacketReporters, 1)
	assert.Equal(t, ReporterUnknown, snap.PacketReporters[0].State)
	assert.Equal(t, 0, snap.PacketLive)
	assert.False(t, snap.SuppressSweep)

	require.Equal(t, http.StatusAccepted, post(r, testToken, MeshPacketStream, packetBody("", advertPacket)).Code)
	snap = r.MeshReports()
	assert.Equal(t, 1, snap.PacketLive)
	assert.Empty(t, snap.Reports)
	assert.False(t, snap.SuppressSweep)

	now = now.Add(31 * time.Minute) // past StaleAfter
	snap = r.MeshReports()
	assert.Equal(t, 0, snap.PacketLive)
	assert.True(t, snap.SuppressSweep, "a silent forwarder's nodes are about to age out for OUR reason")

	now = now.Add(2 * time.Hour) // past StaleAfter × reporterDeadMultiple
	snap = r.MeshReports()
	assert.Equal(t, ReporterDead, snap.PacketReporters[0].State)
	assert.False(t, snap.SuppressSweep, "suppression is bounded")
}

func TestPacketSourceRoundTrip(t *testing.T) {
	id, ok := ReporterFromPacketSource(PacketSource("alan-pi"))
	assert.True(t, ok)
	assert.Equal(t, "alan-pi", id)
	_, ok = ReporterFromPacketSource("wss://mqtt.gomesh.dev:443/mqtt")
	assert.False(t, ok)
}
