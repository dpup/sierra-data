// Package meshcore ingests MeshCore LoRa-mesh node presence from community MQTT
// bridges. MeshCore has no native MQTT and no official broker; the de-facto
// map-ecosystem bridges (Cisien/meshcoretomqtt, mqtt.meshmapper.net) publish a
// JSON envelope per received packet to meshcore/{IATA}/{PUBLIC_KEY}/packets,
// carrying gateway-added SNR/RSSI/path plus a hex `raw` payload. We only care
// about ADVERT packets (packet_type 4): they are unencrypted and carry a node's
// identity, role, optional location, and name.
//
// This file is the pure, dependency-light advert decoder (std-lib crypto only),
// unit-tested against constructed and captured payloads. The byte layout is
// taken from the MeshCore firmware (helpers/AdvertDataHelpers) and the
// michaelhart/meshcore-decoder reference:
//
//	[0:32]    Ed25519 public key (the node identity)
//	[32:36]   uint32 LE  advert timestamp (sender's clock, unix seconds)
//	[36:100]  Ed25519 signature over  pubkey ‖ timestamp ‖ appdata
//	[100:]    app-data (≤32 bytes), a flags byte then flag-gated fields:
//	            byte0 bits 0-3 : role  (1=companion 2=repeater 3=room 4=sensor)
//	            byte0 bit 4 0x10: lat,lng follow as two int32 LE, degrees×1e6
//	            byte0 bit 5 0x20: reserved 2-byte field present (skipped)
//	            byte0 bit 6 0x40: reserved 2-byte field present (skipped)
//	            byte0 bit 7 0x80: UTF-8 name occupies the remaining bytes
package meshcore

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// Advert byte layout.
const (
	advPubKeyLen   = 32
	advTimestampAt = 32
	advSignatureAt = 36
	advSignatureN  = 64
	advAppDataAt   = 100
	advMinLen      = advAppDataAt // signature block is fixed; app-data may be empty
)

// App-data flag bits (byte 0 of app-data).
const (
	roleMask     = 0x0F
	flagLocation = 0x10
	flagReserv5  = 0x20
	flagReserv6  = 0x40
	flagName     = 0x80
)

// Node roles by nibble.
const (
	RoleCompanion  = "companion"
	RoleRepeater   = "repeater"
	RoleRoomServer = "room_server"
	RoleSensor     = "sensor"
)

// Advert is a decoded MeshCore ADVERT packet.
type Advert struct {
	PubKey      string    // full public key, lowercase hex — the node identity
	Role        string    // companion | repeater | room_server | sensor | unknown(n)
	Name        string    // advertised name, empty if the name flag was unset
	HasLocation bool      // the location flag was set
	Lat, Lng    float64   // valid only when HasLocation
	Timestamp   time.Time // sender-stamped advert time (UTC) — node clocks are
	// unreliable (frequently skewed), so this is diagnostic only; never use it for
	// ordering or freshness (use our receive time).
	// Path/HopCount are the transport relay path — the repeater-hash chain the
	// advert traversed to reach the observing bridge. Set by DecodeFrame (frame
	// level); DecodeAdvert alone leaves them empty (the payload carries no path).
	Path           []string // per-hop repeater hashes, lowercase hex, sender→observer order
	HopCount       int      // relay hop count (frame hash_count)
	SignatureValid bool     // Ed25519 signature verified against pubkey ‖ ts ‖ appdata
}

// roleName maps the role nibble to a stable slug.
func roleName(flags byte) string {
	switch flags & roleMask {
	case 1:
		return RoleCompanion
	case 2:
		return RoleRepeater
	case 3:
		return RoleRoomServer
	case 4:
		return RoleSensor
	default:
		return fmt.Sprintf("unknown(%d)", flags&roleMask)
	}
}

// DecodeAdvert parses an ADVERT payload (starting at the public key). It returns
// an error only on a structurally invalid packet (too short, truncated
// flag-gated field); a bad signature is reported via Advert.SignatureValid so
// the caller chooses the trust policy. Location, when present, is decoded as
// int32 LE ÷ 1e6 and range-checked.
func DecodeAdvert(payload []byte) (*Advert, error) {
	if len(payload) < advMinLen {
		return nil, fmt.Errorf("meshcore: advert too short: %d bytes (need ≥%d)", len(payload), advMinLen)
	}
	pub := payload[0:advPubKeyLen]
	tsUnix := binary.LittleEndian.Uint32(payload[advTimestampAt : advTimestampAt+4])
	sig := payload[advSignatureAt : advSignatureAt+advSignatureN]
	app := payload[advAppDataAt:]

	a := &Advert{
		PubKey:    hex.EncodeToString(pub),
		Role:      "unknown(0)",
		Timestamp: time.Unix(int64(tsUnix), 0).UTC(),
	}

	if len(app) > 0 {
		flags := app[0]
		a.Role = roleName(flags)
		p := 1
		if flags&flagLocation != 0 {
			if len(app) < p+8 {
				return nil, fmt.Errorf("meshcore: advert location flag set but app-data truncated")
			}
			lat := int32(binary.LittleEndian.Uint32(app[p : p+4]))
			lng := int32(binary.LittleEndian.Uint32(app[p+4 : p+8]))
			a.Lat = float64(lat) / 1e6
			a.Lng = float64(lng) / 1e6
			// (0,0) is the firmware default for an unset fixed location; treat it
			// as "no location" so a node that never set one isn't geo-placed.
			a.HasLocation = a.Lat != 0 || a.Lng != 0
			if a.Lat < -90 || a.Lat > 90 || a.Lng < -180 || a.Lng > 180 {
				return nil, fmt.Errorf("meshcore: advert location out of range: %f,%f", a.Lat, a.Lng)
			}
			p += 8
		}
		if flags&flagReserv5 != 0 {
			p += 2
		}
		if flags&flagReserv6 != 0 {
			p += 2
		}
		if flags&flagName != 0 {
			if p > len(app) {
				return nil, fmt.Errorf("meshcore: advert name flag set but app-data truncated")
			}
			a.Name = string(app[p:])
		}
	}

	// Signature covers pubkey ‖ timestamp ‖ appdata. ed25519.Verify never
	// panics for a 32-byte key, but guard anyway.
	if len(pub) == ed25519.PublicKeySize {
		msg := make([]byte, 0, 36+len(app))
		msg = append(msg, payload[0:advSignatureAt]...) // pubkey + timestamp
		msg = append(msg, app...)
		a.SignatureValid = ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
	}

	return a, nil
}
