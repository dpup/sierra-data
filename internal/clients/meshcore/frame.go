package meshcore

import "fmt"

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
	routeTransportDirect = 0x03

	payloadTypeAdvert = 0x04
	transportCodesLen = 4
)

// DecodeFrame parses a full MeshCore over-the-air packet (a bridge `raw`
// payload), strips the transport header/path, and decodes the ADVERT payload.
// It returns an error for non-advert frames or structurally invalid framing;
// signature validity is reported on Advert.SignatureValid (see DecodeAdvert).
func DecodeFrame(raw []byte) (*Advert, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("meshcore: frame too short: %d bytes", len(raw))
	}
	header := raw[0]
	routeType := header & hdrRouteTypeMask
	payloadType := (header >> hdrPayloadTypeShift) & hdrPayloadTypeMask
	if payloadType != payloadTypeAdvert {
		return nil, fmt.Errorf("meshcore: not an ADVERT frame (payload_type=%d)", payloadType)
	}

	off := 1
	if routeType == routeTransportFlood || routeType == routeTransportDirect {
		off += transportCodesLen // transport_codes present on transport routes
	}
	if off >= len(raw) {
		return nil, fmt.Errorf("meshcore: frame truncated before path_len")
	}

	pathLen := raw[off]
	off++
	hashSize := int(pathLen>>6) + 1 // 2 top bits → 1..4 bytes per hop
	hashCount := int(pathLen & 0x3F)
	pathBytes := hashSize * hashCount
	if off+pathBytes > len(raw) {
		return nil, fmt.Errorf("meshcore: frame path exceeds length (hashSize=%d hashCount=%d, have %d)", hashSize, hashCount, len(raw)-off)
	}
	off += pathBytes

	return DecodeAdvert(raw[off:])
}
