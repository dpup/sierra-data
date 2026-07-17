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

// advertFrame wraps a bare advert payload in the minimal MeshCore transport
// frame the bridges actually publish in `raw`: header (advert+flood) + a
// zero-hop path_len. DecodeFrame strips this back off.
func advertFrame(payload []byte) []byte {
	frame := []byte{0x11, 0x00} // payload_type=ADVERT, route=FLOOD; path_len=0 (no hops)
	return append(frame, payload...)
}

// envelopeJSON wraps a payload the way the map-ecosystem bridges do.
func envelopeJSON(t *testing.T, packetType int, payload []byte, snr float64, rssi int, origin, path string) []byte {
	t.Helper()
	m := map[string]any{
		"origin":      origin,
		"origin_id":   origin,
		"timestamp":   "2026-07-15T10:00:00.000000",
		"packet_type": fmt.Sprintf("%d", packetType), // bridges send numbers as strings
		"route":       "F",
		"raw":         hex.EncodeToString(advertFrame(payload)),
		"SNR":         fmt.Sprintf("%g", snr),
		"RSSI":        fmt.Sprintf("%d", rssi),
		"hash":        "AC9D2DDDD8395712",
		"path":        path,
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
	r.ingestRaw(envelopeJSON(t, 4, payload, 4.5, -93, "ag loft rpt", "C2 -> E2"), "broker-a")

	nodes := r.Snapshot(0)
	require.Len(t, nodes, 1)
	n := nodes[0]
	assert.Equal(t, RoleRepeater, n.Role)
	assert.Equal(t, "Murphys Ridge", n.Name)
	assert.True(t, n.HasLocation)
	assert.InDelta(t, 4.5, n.SNR, 1e-9)
	assert.EqualValues(t, -93, n.RSSI)
	assert.EqualValues(t, 2, n.HopCount)
	assert.Equal(t, []string{"C2", "E2"}, n.Path)
	assert.Equal(t, []string{"ag loft rpt"}, n.Gateways)
}

func TestRegistryIgnoresNonAdvert(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, nil, "n", 1_700_000_000)

	r := NewRegistry(Config{})
	r.ingestRaw(envelopeJSON(t, 2, payload, 5, -90, "gw", ""), "broker-a") // TXT_MSG
	assert.Empty(t, r.Snapshot(0))
}

func TestRegistryMergesAcrossBrokers(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	loc := [2]float64{38.1, -120.4}
	payload := buildAdvert(t, priv, 2, &loc, "Ridge", 1_700_000_000)

	r := NewRegistry(Config{})
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw-east", ""), "broker-a")
	r.ingestRaw(envelopeJSON(t, 4, payload, 6, -80, "gw-west", ""), "broker-b")

	nodes := r.Snapshot(0)
	require.Len(t, nodes, 1, "same pubkey collapses to one node")
	assert.Equal(t, []string{"gw-east", "gw-west"}, nodes[0].Gateways)
}

func TestRegistrySnapshotActiveWindow(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	payload := buildAdvert(t, priv, 2, &[2]float64{38, -120}, "Ridge", 1_700_000_000)

	base := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	r := NewRegistry(Config{})
	r.now = func() time.Time { return base }
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw", ""), "broker-a")

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
	r.ingestRaw(envelopeJSON(t, 4, payload, 4, -93, "gw", ""), "broker-a")
	assert.Len(t, r.Snapshot(0), 1)

	r.now = func() time.Time { return base.Add(2 * time.Hour) }
	assert.Empty(t, r.Snapshot(0), "node pruned past RetainFor")
}
