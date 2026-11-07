package meshcore

import (
	"encoding/hex"
	"strings"
)

// Bounds on a node identifier. A MeshCore public key is 32 bytes (64 hex);
// operator monitors commonly identify a node by a PREFIX of it (8 bytes / 16
// hex in practice), and until an advert supplies the full key the Grid files
// that node under the prefix. Anything shorter than 4 bytes is too
// collision-prone to name a node with any confidence.
const (
	MinNodeIDHex = 8
	MaxNodeIDHex = 64
)

// EventIDPrefix namespaces a mesh node's event id: "meshcore:<node id>".
const EventIDPrefix = "meshcore:"

// ValidNodeID reports whether id is an even-length lowercase hex string within
// the bounds a full key or a key prefix can carry.
func ValidNodeID(id string) bool {
	if len(id) < MinNodeIDHex || len(id) > MaxNodeIDHex || len(id)%2 != 0 {
		return false
	}
	if strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// ParseNodeKey normalizes a caller-supplied node reference to the key the
// store files it under: either form a client holds (the bare public key or
// prefix, or the event id "meshcore:<key>" from /api/v1/events), any case,
// surrounding space ignored. ok is false when what remains is not a node id.
func ParseNodeKey(s string) (key string, ok bool) {
	key = strings.ToLower(strings.TrimSpace(s))
	key = strings.TrimPrefix(key, EventIDPrefix)
	return key, ValidNodeID(key)
}
