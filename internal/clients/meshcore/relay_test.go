package meshcore

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Payload types used below (the header's bits 2-5).
const (
	ptTxtMsg = 2
	ptGrpTxt = 5
	ptTrace  = 9
)

// rawFrame builds an over-the-air frame of any type: header, transport codes
// (only for the transport route variants), path_len, hops, then a payload.
// Every hop must be hashSize bytes.
func rawFrame(t *testing.T, routeType, payloadType byte, hashSize int, hops [][]byte, payload []byte) []byte {
	t.Helper()
	frame := []byte{payloadType<<2 | routeType}
	if routeType == routeTransportFlood || routeType == routeTransportDirect {
		frame = append(frame, 0xde, 0xad, 0xbe, 0xef)
	}
	frame = append(frame, byte(hashSize-1)<<6|byte(len(hops)))
	for _, h := range hops {
		require.Len(t, h, hashSize)
		frame = append(frame, h...)
	}
	return append(frame, payload...)
}

func packetJSON(t *testing.T, packetType int, frame []byte) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"origin_id":   "gw",
		"packet_type": packetType,
		"raw":         hex.EncodeToString(frame),
	})
	require.NoError(t, err)
	return b
}

// hop returns the first n bytes of a hex public key: its path hash.
func hop(t *testing.T, pubkey string, n int) []byte {
	t.Helper()
	b, err := hex.DecodeString(pubkey[:2*n])
	require.NoError(t, err)
	return b
}

// Only a flood path records who relayed. A direct path is the route still to
// travel, and a TRACE path is SNR samples, so neither may name a relay.
func TestFrameHeaderRelayHops(t *testing.T) {
	hops := [][]byte{{0xab, 0x07}, {0x2c, 0xfe}}
	cases := []struct {
		name         string
		route, ptype byte
		want         []string
	}{
		{"flood message", routeFlood, ptTxtMsg, []string{"ab07", "2cfe"}},
		{"transport flood", routeTransportFlood, ptGrpTxt, []string{"ab07", "2cfe"}},
		{"flood advert", routeFlood, payloadTypeAdvert, []string{"ab07", "2cfe"}},
		{"direct: the route still to travel", 0x02, ptTxtMsg, nil},
		{"transport direct", routeTransportDirect, ptTxtMsg, nil},
		{"trace: the path is SNR samples", routeFlood, ptTrace, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := DecodeFrameHeader(rawFrame(t, c.route, c.ptype, 2, hops, []byte{1, 2, 3}))
			require.NoError(t, err)
			assert.Equal(t, []string{"ab07", "2cfe"}, h.Path, "the path parses the same either way")
			assert.Equal(t, c.want, h.RelayHops())
		})
	}
}

func TestDecodeFrameHeaderMatchesDecodeFrame(t *testing.T) {
	h, err := DecodeFrameHeader(mustHex(t, frameGilroy))
	require.NoError(t, err)
	assert.EqualValues(t, routeFlood, h.RouteType)
	assert.EqualValues(t, payloadTypeAdvert, h.PayloadType)
	assert.Equal(t, 2, h.HashSize)
	assert.Equal(t, []string{"cd32", "ad77", "fa57"}, h.Path)
	assert.Equal(t, 2+6, h.PayloadOffset)

	_, err = DecodeFrameHeader([]byte{0x09, 0x03})
	assert.ErrorContains(t, err, "path exceeds length")
}

// relayRegistry returns a registry that knows two repeaters, by advert, and
// their keys.
func relayRegistry(t *testing.T, now time.Time) (*Registry, string, string) {
	t.Helper()
	r := NewRegistry(Config{RequireValidSignature: true})
	r.now = func() time.Time { return now }
	var keys []string
	for _, name := range []string{"Camp Connell", "Snowshoe Lake"} {
		pub, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		payload := buildAdvert(t, priv, 2, &[2]float64{38.3, -120.2}, name, 1_700_000_000)
		out, err := r.IngestEnvelope(envelopeJSON(t, 4, payload, 4, -90, "gw"), "broker-a", "")
		require.NoError(t, err)
		require.Equal(t, PacketAccepted, out)
		keys = append(keys, hex.EncodeToString(pub))
	}
	require.Empty(t, r.DrainRelays(nil), "zero-hop adverts name no relay")
	return r, keys[0], keys[1]
}

// The point of the change: a quiet repeater shows up in other nodes' traffic
// long before its own next advert.
func TestRegistryRecordsRelaysFromEveryPacketType(t *testing.T) {
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	r, connell, snowshoe := relayRegistry(t, base)

	r.now = func() time.Time { return base.Add(time.Minute) }
	out, err := r.IngestEnvelope(packetJSON(t, ptTxtMsg,
		rawFrame(t, routeFlood, ptTxtMsg, 2, [][]byte{hop(t, connell, 2)}, []byte{0xaa, 0xbb})), "broker-a", "")
	require.NoError(t, err)
	assert.Equal(t, PacketIgnored, out, "a message still updates no node")

	r.now = func() time.Time { return base.Add(3 * time.Minute) }
	r.IngestEnvelope(packetJSON(t, ptGrpTxt,
		rawFrame(t, routeFlood, ptGrpTxt, 2, [][]byte{hop(t, snowshoe, 2), hop(t, connell, 2)}, []byte{0xcc})), "broker-a", "")

	// Direct and TRACE frames naming them later must not move either time.
	r.now = func() time.Time { return base.Add(9 * time.Minute) }
	r.IngestEnvelope(packetJSON(t, ptTxtMsg,
		rawFrame(t, 0x02, ptTxtMsg, 2, [][]byte{hop(t, snowshoe, 2), hop(t, connell, 2)}, []byte{0x01})), "broker-a", "")
	r.IngestEnvelope(packetJSON(t, ptTrace,
		rawFrame(t, routeFlood, ptTrace, 2, [][]byte{hop(t, snowshoe, 2)}, []byte{0x01})), "broker-a", "")

	got := r.DrainRelays(nil)
	assert.Equal(t, map[string]time.Time{
		connell:  base.Add(3 * time.Minute),
		snowshoe: base.Add(3 * time.Minute),
	}, got)
	assert.Empty(t, r.DrainRelays(nil), "drain clears the buffer")
	assert.Len(t, r.Snapshot(), 2, "relay evidence adds no node")
}

func TestRegistryRelayFromAcceptedAdvertPathOnly(t *testing.T) {
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	r, connell, _ := relayRegistry(t, base)

	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 1, &[2]float64{38.2, -120.3}, "companion", 1_700_000_000)
	frame := rawFrame(t, routeFlood, payloadTypeAdvert, 2, [][]byte{hop(t, connell, 2)}, payload)

	tampered := append([]byte(nil), frame...)
	tampered[len(tampered)-len(payload)+advSignatureAt] ^= 0xff
	out, _ := r.IngestEnvelope(packetJSON(t, 4, tampered), "broker-a", "")
	require.Equal(t, PacketRejected, out)
	assert.Empty(t, r.DrainRelays(nil), "a rejected advert is evidence of nothing")

	r.now = func() time.Time { return base.Add(time.Minute) }
	out, _ = r.IngestEnvelope(packetJSON(t, 4, frame), "broker-a", "")
	require.Equal(t, PacketAccepted, out)
	assert.Equal(t, map[string]time.Time{connell: base.Add(time.Minute)}, r.DrainRelays(nil))
}

// A hop resolves only to a UNIQUE known key. The caller's extra keys both
// rescue a node the registry has pruned and expose a collision it cannot see.
func TestDrainRelaysResolution(t *testing.T) {
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	r, connell, snowshoe := relayRegistry(t, base)
	send := func(hashSize int, hops ...[]byte) {
		r.IngestEnvelope(packetJSON(t, ptTxtMsg, rawFrame(t, routeFlood, ptTxtMsg, hashSize, hops, []byte{1})), "broker-a", "")
	}

	t.Run("a pruned repeater still resolves through a known key", func(t *testing.T) {
		pruned := "e4f1" + connell[4:]
		send(2, hop(t, pruned, 2))
		assert.Empty(t, r.DrainRelays(nil), "unknown to the registry: unresolved")
		send(2, hop(t, pruned, 2))
		assert.Equal(t, map[string]time.Time{pruned: base}, r.DrainRelays([]string{pruned}))
	})

	t.Run("a known key that shares a prefix makes the hop ambiguous", func(t *testing.T) {
		third := "00"
		if connell[4:6] == third {
			third = "01"
		}
		twin := connell[:4] + third + connell[6:]
		send(2, hop(t, connell, 2))
		assert.Empty(t, r.DrainRelays([]string{twin}), "two candidates: attribute to neither")
	})

	t.Run("a known key the registry also holds is not its own twin", func(t *testing.T) {
		send(2, hop(t, snowshoe, 2))
		assert.Contains(t, r.DrainRelays([]string{snowshoe, connell}), snowshoe)
	})

	t.Run("a 1-byte hop never earns credit, even when it is unique", func(t *testing.T) {
		send(1, hop(t, connell, 1))
		assert.Empty(t, r.DrainRelays(nil), "an unknown relayer could share the byte")
	})

	t.Run("3-byte hops resolve; unmatched hops drop", func(t *testing.T) {
		send(3, hop(t, connell, 3), []byte{0x99, 0x99, 0x99})
		assert.Equal(t, map[string]time.Time{connell: base}, r.DrainRelays(nil))
	})
}

// ReceivedAt answers "when did we last hear it", so unlike LastHeardAt it is
// never the seed's last_seen_at fallback.
func TestRegistryReceivedAt(t *testing.T) {
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	r := NewRegistry(Config{GraceFloor: 100 * time.Hour, GraceCeil: 200 * time.Hour})
	r.now = func() time.Time { return base }
	r.Seed([]SeedNode{
		{PubKey: "aa", HasLocation: true, Lat: 38, Lng: -120,
			HeardTimes: []time.Time{base.Add(-30 * time.Hour), base.Add(-20 * time.Hour)}},
		{PubKey: "bb", HasLocation: true, Lat: 38, Lng: -120, LastHeard: base.Add(-time.Hour)},
	})
	got := map[string]NodeState{}
	for _, n := range r.Snapshot() {
		got[n.PubKey] = n
	}
	assert.Equal(t, base.Add(-20*time.Hour), got["aa"].ReceivedAt, "replayed receptions are real")
	assert.True(t, got["bb"].ReceivedAt.IsZero(), "a last_seen_at fallback is not a reception")
	assert.Equal(t, base.Add(-time.Hour), got["bb"].LastHeardAt, "presence still uses the fallback")

	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, &[2]float64{38, -120}, "Ridge", 1_700_000_000)
	r.now = func() time.Time { return base.Add(time.Minute) }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -90, "gw"), "broker-a")
	for _, n := range r.Snapshot() {
		if n.Name == "Ridge" {
			assert.Equal(t, base.Add(time.Minute), n.ReceivedAt)
		}
	}
}

// Relay presence: a node whose adverts are a day apart stays present while it
// forwards traffic, and drops out RelayPresence after it stops.
func TestRegistryRelayPresence(t *testing.T) {
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	newReg := func(relayPresence time.Duration) (*Registry, string) {
		r := NewRegistry(Config{GraceFloor: time.Hour, GraceCeil: 2 * time.Hour,
			RetainFor: 3 * time.Hour, RelayPresence: relayPresence})
		r.now = func() time.Time { return base }
		pub, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		payload := buildAdvert(t, priv, 2, &[2]float64{38.3, -120.2}, "Camp Connell", 1_700_000_000)
		r.ingestRaw(envelopeJSON(t, 4, payload, 4, -90, "gw"), "broker-a")
		return r, hex.EncodeToString(pub)
	}
	present := func(r *Registry) bool { return len(r.Snapshot()) == 1 }
	relayAt := func(r *Registry, pk string, at time.Time) {
		r.now = func() time.Time { return at }
		r.IngestEnvelope(packetJSON(t, ptTxtMsg,
			rawFrame(t, routeFlood, ptTxtMsg, 2, [][]byte{hop(t, pk, 2)}, []byte{1})), "broker-a", "")
		r.DrainRelays(nil)
	}

	t.Run("a relay keeps a node present past its advert window", func(t *testing.T) {
		r, pk := newReg(2 * time.Hour)
		r.now = func() time.Time { return base.Add(90 * time.Minute) }
		require.False(t, present(r), "advert window (1h) has passed")

		relayAt(r, pk, base.Add(90*time.Minute))
		assert.True(t, present(r), "relayed just now")
		got := r.Snapshot()[0]
		assert.Equal(t, base.Add(90*time.Minute), got.LastRelayedAt)
		assert.Equal(t, "Camp Connell", got.Name, "identity is the advert's")

		r.now = func() time.Time { return base.Add(90*time.Minute + 2*time.Hour) }
		assert.True(t, present(r), "inclusive at the window's edge")
		r.now = func() time.Time { return base.Add(90*time.Minute + 2*time.Hour + time.Second) }
		assert.False(t, present(r), "relays stopped: gone after RelayPresence")
	})

	t.Run("a relaying node is retained past RetainFor since its advert", func(t *testing.T) {
		r, pk := newReg(2 * time.Hour)
		for at := base.Add(time.Hour); at.Before(base.Add(6 * time.Hour)); at = at.Add(time.Hour) {
			relayAt(r, pk, at)
			require.True(t, present(r), "relaying hourly at %s", at.Sub(base))
		}
	})

	t.Run("disabled, a relay is only activity", func(t *testing.T) {
		r, pk := newReg(0)
		r.now = func() time.Time { return base.Add(90 * time.Minute) }
		relayAt(r, pk, base.Add(90*time.Minute))
		assert.False(t, present(r))
	})

	t.Run("a key known only from the store is never made present", func(t *testing.T) {
		r, _ := newReg(2 * time.Hour)
		stranger := "e4f1" + strings.Repeat("0", 60)
		r.IngestEnvelope(packetJSON(t, ptTxtMsg,
			rawFrame(t, routeFlood, ptTxtMsg, 2, [][]byte{hop(t, stranger, 2)}, []byte{1})), "broker-a", "")
		assert.Contains(t, r.DrainRelays([]string{stranger}), stranger, "its activity still resolves")
		for _, n := range r.Snapshot() {
			assert.NotEqual(t, stranger, n.PubKey, "no advert-established identity to keep present")
		}
	})
}

// Seed restores both activity clocks, so relay presence and "last heard"
// survive a restart; a seeded node carries no signal reading of its own.
func TestRegistrySeedRestoresActivity(t *testing.T) {
	base := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	r := NewRegistry(Config{GraceFloor: time.Hour, GraceCeil: 2 * time.Hour, RetainFor: 4 * time.Hour, RelayPresence: 2 * time.Hour})
	r.now = func() time.Time { return base }
	r.Seed([]SeedNode{{
		PubKey: "aa", HasLocation: true, Lat: 38, Lng: -120,
		LastHeard:    base.Add(-3 * time.Hour), // last_seen_at: past the advert window
		LastReceived: base.Add(-5 * time.Hour),
		LastRelayed:  base.Add(-30 * time.Minute),
	}})
	nodes := r.Snapshot()
	require.Len(t, nodes, 1, "present on the restored relay clock alone")
	assert.Equal(t, base.Add(-5*time.Hour), nodes[0].ReceivedAt)
	assert.Equal(t, base.Add(-30*time.Minute), nodes[0].LastRelayedAt)
	assert.False(t, nodes[0].HasSignal, "a seeded node's zero SNR is not a reading")
}
