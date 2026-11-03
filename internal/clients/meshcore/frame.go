package meshcore

import (
	"encoding/hex"
	"fmt"
)

// MeshCore over-the-air packet framing. The map-ecosystem bridges publish the
// full received frame (not just the advert payload) in the envelope's `raw`
// field, so we must strip the transport header before decoding an ADVERT.
//
// Frame layout (docs.meshcore.io/packet_format):
//
//	[header:1][transport_codes:4?][path_len:1][path:0..][payload:0..]
//
// header byte bitfields are VVPPPPRR:
//
//	bits 0-1  route_type     (0 transport-flood, 1 flood, 2 direct, 3 transport-direct)
//	bits 2-5  payload_type   (4 = ADVERT)
//	bits 6-7  payload_version
//
// transport_codes (4 bytes) are present only for the transport route variants.
// path_len is NOT a raw byte count: it packs bytes-per-hop and hop-count as
//
//	path_len = ((hash_size-1) << 6) | (hash_count & 0x3F)
//
// so the path occupies hash_size*hash_count bytes. (This is why a naive
// raw[2+raw[1]:] strip is wrong for hash_size>1 meshes.)
const (
	hdrRouteTypeMask    = 0x03
	hdrPayloadTypeShift = 2
	hdrPayloadTypeMask  = 0x0F

	routeTransportFlood  = 0x00
	routeFlood           = 0x01
	routeTransportDirect = 0x03

	payloadTypeAdvert = 0x04
	// payloadTypeTrace frames reuse the path field for per-hop SNR samples, not
	// repeater hashes, so their "path" names no node at all.
	payloadTypeTrace  = 0x09
	transportCodesLen = 4
)

// FrameHeader is the transport framing of one over-the-air packet: everything
// before the payload. Every packet type carries it in the clear, including the
// ones whose payload is encrypted.
type FrameHeader struct {
	RouteType   byte
	PayloadType byte
	// HashSize is bytes per hop (1..4), from the top two bits of path_len.
	HashSize int
	// Path is the hop hashes, lowercase hex, in the order they were written.
	// What a hop MEANS depends on the route type; see RelayHops.
	Path []string
	// PayloadOffset is where the payload starts in the frame.
	PayloadOffset int
}

// DecodeFrameHeader parses the transport header and path of a full MeshCore
// frame (a bridge `raw` payload) of any packet type.
func DecodeFrameHeader(raw []byte) (FrameHeader, error) {
	if len(raw) < 2 {
		return FrameHeader{}, fmt.Errorf("meshcore: frame too short: %d bytes", len(raw))
	}
	h := FrameHeader{
		RouteType:   raw[0] & hdrRouteTypeMask,
		PayloadType: (raw[0] >> hdrPayloadTypeShift) & hdrPayloadTypeMask,
	}
	off := 1
	if h.RouteType == routeTransportFlood || h.RouteType == routeTransportDirect {
		off += transportCodesLen // transport_codes present on transport routes
	}
	if off >= len(raw) {
		return FrameHeader{}, fmt.Errorf("meshcore: frame truncated before path_len")
	}

	pathLen := raw[off]
	off++
	h.HashSize = int(pathLen>>6) + 1 // 2 top bits → 1..4 bytes per hop
	hashCount := int(pathLen & 0x3F)
	pathBytes := h.HashSize * hashCount
	if off+pathBytes > len(raw) {
		return FrameHeader{}, fmt.Errorf("meshcore: frame path exceeds length (hashSize=%d hashCount=%d, have %d)", h.HashSize, hashCount, len(raw)-off)
	}
	path := raw[off : off+pathBytes]
	h.Path = make([]string, hashCount)
	for i := 0; i < hashCount; i++ {
		h.Path[i] = hex.EncodeToString(path[i*h.HashSize : (i+1)*h.HashSize])
	}
	h.PayloadOffset = off + pathBytes
	return h, nil
}

// RelayHops returns the hops this frame proves RELAYED it, or nil.
//
// Only a FLOOD-routed frame's path is that record: each repeater appends its
// own hash before it retransmits, so every hop a receiver sees has already
// forwarded the packet. A DIRECT-routed frame's path is the opposite, the
// source route still to travel (each repeater strips itself before
// forwarding), so a hop listed there may never have heard the packet. Observed
// live: one direct request heard as [2cfe ab07], then [ab07], then []. And a
// TRACE frame's path holds SNR samples, not hashes.
//
// The path is not signed, by anyone. It can extend the presence of a node a
// signed advert already established (Config.RelayPresence); it can never
// introduce one, or say where it is or what it is called.
func (h FrameHeader) RelayHops() []string {
	if h.RouteType != routeFlood && h.RouteType != routeTransportFlood {
		return nil
	}
	if h.PayloadType == payloadTypeTrace {
		return nil
	}
	return h.Path
}

// DecodeFrame parses a full MeshCore over-the-air packet (a bridge `raw`
// payload), strips the transport header/path, and decodes the ADVERT payload.
// It returns an error for non-advert frames or structurally invalid framing;
// signature validity is reported on Advert.SignatureValid (see DecodeAdvert).
func DecodeFrame(raw []byte) (*Advert, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("meshcore: frame too short: %d bytes", len(raw))
	}
	if payloadType := (raw[0] >> hdrPayloadTypeShift) & hdrPayloadTypeMask; payloadType != payloadTypeAdvert {
		return nil, fmt.Errorf("meshcore: not an ADVERT frame (payload_type=%d)", payloadType)
	}
	h, err := DecodeFrameHeader(raw)
	if err != nil {
		return nil, err
	}
	adv, err := DecodeAdvert(raw[h.PayloadOffset:])
	if err != nil {
		return nil, err
	}
	// Carry the relay path: hash_count hops of hash_size-byte repeater hashes,
	// one hop hash per hex string, sender→observer order. This is the mesh
	// topology the payload alone doesn't carry.
	adv.HopCount = len(h.Path)
	adv.Path = h.Path
	return adv, nil
}
