package meshcore

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real frames captured live from mqtt.gomesh.dev (2026-07-17). The bridge `raw`
// is the full over-the-air MeshCore packet, so DecodeFrame must strip the
// transport header + path before decoding. The three "Janet" frames are the
// same advert relayed at 0/1/2 hops — they must all yield the identical payload,
// proving the hash_size/hash_count path math (path_len 0x40 → 2 bytes/hop).
const (
	// companion "Janet", no location. path_len 0x40 (hash_size=2, hop=0).
	frameJanet0 = "11403C283A1DAF1350BEE0F29D0ABC2961FC7D9A7279AF904F775861D3B0F4110C5B535A5A6AECA3CB37CCB779A993053DACDC7AF264C674317AF63A05EC3C68E4C41DA19F00E03D45DC4E43CF6FF9BA974F2703D5DFA45FB98444A7A4118BC957087A9F5F04814A616E6574"
	// same advert, 1 hop (path_len 0x41, one 2-byte hash ABAB).
	frameJanet1 = "1141ABAB3C283A1DAF1350BEE0F29D0ABC2961FC7D9A7279AF904F775861D3B0F4110C5B535A5A6AECA3CB37CCB779A993053DACDC7AF264C674317AF63A05EC3C68E4C41DA19F00E03D45DC4E43CF6FF9BA974F2703D5DFA45FB98444A7A4118BC957087A9F5F04814A616E6574"
	// same advert, 2 hops (path_len 0x42, two 2-byte hashes DC5060 95).
	frameJanet2 = "1142DC5060953C283A1DAF1350BEE0F29D0ABC2961FC7D9A7279AF904F775861D3B0F4110C5B535A5A6AECA3CB37CCB779A993053DACDC7AF264C674317AF63A05EC3C68E4C41DA19F00E03D45DC4E43CF6FF9BA974F2703D5DFA45FB98444A7A4118BC957087A9F5F04814A616E6574"
	// repeater "ESP4 Gilroy Repeater" WITH location, 3 hops (path_len 0x43).
	frameGilroy = "1143CD32AD77FA57FE664BBB1CC8C8898482843419653BB0E72C47281754D1921AE8038CCF44BAEA415A5A6AA37B2B4BF1118120132CF6FE225F5D498A8C5E3E996F3E5B0D4E3406008CF4E325AFC6607AD55822F6975DAF745FB6F0A4BB11C8CC6623E495DF8FA0EDE21904926AE43402D1A1C0F8455350342047696C726F79205265706561746572"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestDecodeFrame_CompanionNoLocation(t *testing.T) {
	adv, err := DecodeFrame(mustHex(t, frameJanet0))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(adv.PubKey, "3c283a1daf1350be"), "pubkey %s", adv.PubKey)
	assert.Equal(t, RoleCompanion, adv.Role)
	assert.Equal(t, "Janet", adv.Name)
	assert.False(t, adv.HasLocation)
	assert.True(t, adv.SignatureValid, "signature must verify after correct framing strip")
}

func TestDecodeFrame_RepeaterWithLocation(t *testing.T) {
	adv, err := DecodeFrame(mustHex(t, frameGilroy))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(adv.PubKey, "fe664bbb1cc8"), "pubkey %s", adv.PubKey)
	assert.Equal(t, RoleRepeater, adv.Role)
	assert.Equal(t, "ESP4 Gilroy Repeater", adv.Name)
	require.True(t, adv.HasLocation)
	assert.InDelta(t, 37.02078, adv.Lat, 0.0005)
	assert.InDelta(t, -121.59339, adv.Lng, 0.0005)
	assert.True(t, adv.SignatureValid)
}

// The same advert relayed at different hop counts must decode identically —
// this is what breaks if the hash_size (top 2 bits of path_len) is ignored.
func TestDecodeFrame_HopInvariant(t *testing.T) {
	var pubkeys, names []string
	for _, f := range []string{frameJanet0, frameJanet1, frameJanet2} {
		adv, err := DecodeFrame(mustHex(t, f))
		require.NoError(t, err)
		assert.True(t, adv.SignatureValid, "frame %s… sig", f[:8])
		pubkeys = append(pubkeys, adv.PubKey)
		names = append(names, adv.Name)
	}
	assert.Equal(t, pubkeys[0], pubkeys[1])
	assert.Equal(t, pubkeys[0], pubkeys[2])
	assert.Equal(t, "Janet", names[0])
	assert.Equal(t, names[0], names[2])
}

func TestDecodeFrame_RelayPath(t *testing.T) {
	// 0 hops (heard directly): empty path. (frameJanet0 path_len 0x40)
	adv, err := DecodeFrame(mustHex(t, frameJanet0))
	require.NoError(t, err)
	assert.Equal(t, 0, adv.HopCount)
	assert.Empty(t, adv.Path)

	// 1 hop, hash_size 2 (path_len 0x41): one 2-byte hash.
	adv, err = DecodeFrame(mustHex(t, frameJanet1))
	require.NoError(t, err)
	assert.Equal(t, 1, adv.HopCount)
	assert.Equal(t, []string{"abab"}, adv.Path)

	// 2 hops (path_len 0x42): two 2-byte hashes.
	adv, err = DecodeFrame(mustHex(t, frameJanet2))
	require.NoError(t, err)
	assert.Equal(t, 2, adv.HopCount)
	assert.Equal(t, []string{"dc50", "6095"}, adv.Path)

	// 3 hops (Gilroy, path_len 0x43): three 2-byte hashes — the relay chain.
	adv, err = DecodeFrame(mustHex(t, frameGilroy))
	require.NoError(t, err)
	assert.Equal(t, 3, adv.HopCount)
	assert.Equal(t, []string{"cd32", "ad77", "fa57"}, adv.Path)
}

func TestDecodeFrame_RejectsNonAdvert(t *testing.T) {
	// header 0x08 → payload_type 2 (TXT_MSG), route 0. Not an advert.
	_, err := DecodeFrame([]byte{0x08, 0x00, 0x01, 0x02})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an ADVERT")
}

func TestDecodeFrame_TruncatedPath(t *testing.T) {
	// advert header, path_len claims 3 hops of 1 byte but no path bytes follow.
	_, err := DecodeFrame([]byte{0x11, 0x03})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path exceeds length")
}

// Guards the hash_size math directly: path_len 0x43 = hash_size 2, hop 3.
func TestPathLenEncoding(t *testing.T) {
	pathLen := byte(0x43)
	hashSize := int(pathLen>>6) + 1
	hashCount := int(pathLen & 0x3F)
	assert.Equal(t, 2, hashSize)
	assert.Equal(t, 3, hashCount)
	assert.Equal(t, 6, hashSize*hashCount)
}
