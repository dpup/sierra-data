package meshcore

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildAdvert assembles a signed ADVERT payload the way MeshCore firmware does:
// pubkey(32) ‖ ts(4 LE) ‖ sig(64) ‖ appdata, sig over pubkey ‖ ts ‖ appdata.
func buildAdvert(t *testing.T, priv ed25519.PrivateKey, role byte, loc *[2]float64, name string, ts uint32) []byte {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)

	flags := role & roleMask
	app := []byte{0} // placeholder for the flags byte
	if loc != nil {
		flags |= flagLocation
		var b [8]byte
		binary.LittleEndian.PutUint32(b[0:4], uint32(int32(loc[0]*1e6)))
		binary.LittleEndian.PutUint32(b[4:8], uint32(int32(loc[1]*1e6)))
		app = append(app, b[:]...)
	}
	if name != "" {
		flags |= flagName
		app = append(app, []byte(name)...)
	}
	app[0] = flags

	tsb := make([]byte, 4)
	binary.LittleEndian.PutUint32(tsb, ts)

	msg := make([]byte, 0, 36+len(app))
	msg = append(msg, pub...)
	msg = append(msg, tsb...)
	msg = append(msg, app...)
	sig := ed25519.Sign(priv, msg)

	payload := make([]byte, 0, advAppDataAt+len(app))
	payload = append(payload, pub...)
	payload = append(payload, tsb...)
	payload = append(payload, sig...)
	payload = append(payload, app...)
	return payload
}

func TestDecodeAdvertRepeaterWithLocation(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	ts := uint32(time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC).Unix())
	loc := [2]float64{38.1374, -120.4579} // Murphys, CA
	payload := buildAdvert(t, priv, 2, &loc, "Murphys Ridge", ts)

	adv, err := DecodeAdvert(payload)
	require.NoError(t, err)
	assert.Equal(t, RoleRepeater, adv.Role)
	assert.Equal(t, "Murphys Ridge", adv.Name)
	assert.True(t, adv.HasLocation)
	assert.InDelta(t, 38.1374, adv.Lat, 1e-6)
	assert.InDelta(t, -120.4579, adv.Lng, 1e-6)
	assert.True(t, adv.SignatureValid)
	assert.Equal(t, hex.EncodeToString(priv.Public().(ed25519.PublicKey)), adv.PubKey)
	assert.Equal(t, ts, uint32(adv.Timestamp.Unix()))
}

func TestDecodeAdvertCompanionNoLocation(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 1, nil, "Handheld", 1_700_000_000)

	adv, err := DecodeAdvert(payload)
	require.NoError(t, err)
	assert.Equal(t, RoleCompanion, adv.Role)
	assert.False(t, adv.HasLocation)
	assert.Equal(t, "Handheld", adv.Name)
	assert.True(t, adv.SignatureValid)
}

func TestDecodeAdvertZeroLocationTreatedAsNone(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	loc := [2]float64{0, 0} // firmware default for an unset fixed location
	payload := buildAdvert(t, priv, 2, &loc, "Unset", 1_700_000_000)

	adv, err := DecodeAdvert(payload)
	require.NoError(t, err)
	assert.False(t, adv.HasLocation, "(0,0) must not geo-place a node")
}

func TestDecodeAdvertBadSignature(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, nil, "Tampered", 1_700_000_000)
	payload[len(payload)-1] ^= 0xFF // flip a name byte, breaking the signature

	adv, err := DecodeAdvert(payload)
	require.NoError(t, err)
	assert.False(t, adv.SignatureValid)
}

func TestDecodeAdvertTooShort(t *testing.T) {
	_, err := DecodeAdvert(make([]byte, 50))
	require.Error(t, err)
}

// advertFrame wraps a bare advert payload in the MeshCore transport frame the
// bridges publish in `raw`: header (advert+flood) + path_len + path + payload.
// Each hop is a 1-byte repeater hash (hash_size 1); no hops → a zero-hop frame.
// DecodeFrame strips this back off (and recovers the path).
func advertFrame(payload []byte, hops ...byte) []byte {
	pathLen := byte(len(hops)) // hash_size 1 (top bits 0), hash_count = len(hops)
	frame := append([]byte{0x11, pathLen}, hops...)
	return append(frame, payload...)
}

// envelopeJSON wraps a payload the way the map-ecosystem bridges do; `hops` are
// the frame's relay-path hash bytes (the authoritative path source now).
func envelopeJSON(t *testing.T, packetType int, payload []byte, snr float64, rssi int, origin string, hops ...byte) []byte {
	t.Helper()
	m := map[string]any{
		"origin":      origin,
		"origin_id":   origin,
		"timestamp":   "2026-07-15T10:00:00.000000",
		"packet_type": fmt.Sprintf("%d", packetType), // bridges send numbers as strings
		"route":       "F",
		"raw":         hex.EncodeToString(advertFrame(payload, hops...)),
		"SNR":         fmt.Sprintf("%g", snr),
		"RSSI":        fmt.Sprintf("%d", rssi),
		"hash":        "AC9D2DDDD8395712",
	}
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestRegistryIngestsAdvert(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	loc := [2]float64{38.1374, -120.4579}
	payload := buildAdvert(t, priv, 2, &loc, "Murphys Ridge", 1_700_000_000)

	r := NewRegistry(Config{})
	r.ingestRaw(envelopeJSON(t, 4, payload, 4.5, -93, "ag loft rpt", 0xc2, 0xe2), "broker-a")

	nodes := r.Snapshot(0)
	require.Len(t, nodes, 1)
	n := nodes[0]
	assert.Equal(t, RoleRepeater, n.Role)
	assert.Equal(t, "Murphys Ridge", n.Name)
	assert.True(t, n.HasLocation)
	assert.InDelta(t, 4.5, n.SNR, 1e-9)
	assert.EqualValues(t, -93, n.RSSI)
	assert.EqualValues(t, 2, n.HopCount)
	assert.Equal(t, []string{"c2", "e2"}, n.Path) // from the frame relay path, hex per hop
	assert.Len(t, n.PathNodes, 2)                 // resolved parallel to Path (no catalog match here)
	assert.Equal(t, []string{"ag loft rpt"}, n.Gateways)
	assert.Equal(t, []string{"broker-a"}, n.Brokers)
}

func TestRegistryDrainObservations(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	loc := [2]float64{38.1, -120.4}
	payload := buildAdvert(t, priv, 2, &loc, "Ridge", 1_700_000_000)

	r := NewRegistry(Config{})
	// Same node heard on two gateways: presence collapses to one, but EVERY
	// reception is captured for the topology firehose.
	r.ingestRaw(envelopeJSON(t, 4, payload, 4.5, -93, "gw-east", 0xc2, 0xe2), "broker-a")
	r.ingestRaw(envelopeJSON(t, 4, payload, 6, -80, "gw-west", 0xc2, 0xe2), "broker-b")

	obs := r.DrainObservations()
	require.Len(t, obs, 2, "every reception captured, not collapsed like presence")
	assert.Equal(t, "broker-a", obs[0].Broker)
	assert.Equal(t, "gw-east", obs[0].Gateway)
	assert.InDelta(t, 4.5, obs[0].SNR, 1e-9)
	assert.EqualValues(t, -93, obs[0].RSSI)
	assert.EqualValues(t, 2, obs[0].HopCount)
	assert.Equal(t, []string{"c2", "e2"}, obs[0].Path)
	assert.Len(t, obs[0].PathNodes, 2) // resolved parallel to Path (no catalog match here)

	assert.Empty(t, r.DrainObservations(), "drain clears the buffer")
}

func TestRegistrySpamFloor(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, &[2]float64{38, -120}, "Ridge", 1_700_000_000)

	base := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	r := NewRegistry(Config{SpamFloor: time.Minute})
	r.now = func() time.Time { return base }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw"), "broker-a")

	// 10s later, SAME node+gateway → dropped by the floor.
	r.now = func() time.Time { return base.Add(10 * time.Second) }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw"), "broker-a")

	// Same instant, DIFFERENT gateway → kept (multi-gateway is resilience signal).
	r.ingestRaw(envelopeJSON(t, 4, payload, 5, -80, "gw2"), "broker-a")

	// 90s from the first advert → past the floor on the original gateway → kept.
	r.now = func() time.Time { return base.Add(90 * time.Second) }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw"), "broker-a")

	obs := r.DrainObservations()
	assert.Len(t, obs, 3, "floor drops only the same node+gateway within the window")
}

func TestResolvePath(t *testing.T) {
	// abcdef00 and ab998877 share the 1-byte prefix "ab"; cd123456 is alone.
	pubkeys := []string{"abcdef00", "ab998877", "cd123456"}
	idx := map[string][]string{}
	for _, pk := range pubkeys {
		for _, n := range prefixLens {
			if len(pk) >= n {
				idx[pk[:n]] = append(idx[pk[:n]], pk)
			}
		}
	}
	got := resolvePath([]string{"ab", "abcd", "cd12", "9999"}, idx)
	assert.Equal(t, []string{
		"",         // "ab" (1-byte) matches two nodes → ambiguous, unresolved
		"abcdef00", // "abcd" (2-byte) is unique
		"cd123456", // "cd12" (2-byte) is unique
		"",         // "9999" matches nothing
	}, got)
	assert.Nil(t, resolvePath(nil, idx))
}

func TestRegistryIgnoresNonAdvert(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, nil, "n", 1_700_000_000)

	r := NewRegistry(Config{})
	r.ingestRaw(envelopeJSON(t, 2, payload, 5, -90, "gw"), "broker-a") // TXT_MSG
	assert.Empty(t, r.Snapshot(0))
}

func TestRegistryMergesAcrossBrokers(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	loc := [2]float64{38.1, -120.4}
	payload := buildAdvert(t, priv, 2, &loc, "Ridge", 1_700_000_000)

	r := NewRegistry(Config{})
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw-east"), "broker-a")
	r.ingestRaw(envelopeJSON(t, 4, payload, 6, -80, "gw-west"), "broker-b")

	nodes := r.Snapshot(0)
	require.Len(t, nodes, 1, "same pubkey collapses to one node")
	assert.Equal(t, []string{"gw-east", "gw-west"}, nodes[0].Gateways)
	assert.Equal(t, []string{"broker-a", "broker-b"}, nodes[0].Brokers, "brokers union, sorted")
}

func TestRegistrySnapshotActiveWindow(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, &[2]float64{38, -120}, "Ridge", 1_700_000_000)

	base := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	r := NewRegistry(Config{})
	r.now = func() time.Time { return base }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw"), "broker-a")

	// 20m later, within a 30m window.
	r.now = func() time.Time { return base.Add(20 * time.Minute) }
	assert.Len(t, r.Snapshot(30*time.Minute), 1)

	// 40m later, outside it.
	r.now = func() time.Time { return base.Add(40 * time.Minute) }
	assert.Empty(t, r.Snapshot(30*time.Minute))
}

func TestRegistryPrunesStaleNodes(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, &[2]float64{38, -120}, "Ridge", 1_700_000_000)

	base := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	r := NewRegistry(Config{RetainFor: time.Hour})
	r.now = func() time.Time { return base }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw"), "broker-a")
	assert.Len(t, r.Snapshot(0), 1)

	r.now = func() time.Time { return base.Add(2 * time.Hour) }
	assert.Empty(t, r.Snapshot(0), "node pruned past RetainFor")
}
