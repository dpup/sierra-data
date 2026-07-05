package gridapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// jsonOpts is the one protojson configuration for the /v1 surface: proto
// field names verbatim, so the wire is snake_case end to end (plan §2.4).
//
// Determinism note: protojson output is deliberately unstable ACROSS binaries
// (protobuf's detrand varies whitespace per process), but within one running
// process the bytes for equal messages are stable. That is exactly the
// lifetime a strong body-hash ETag needs for If-None-Match revalidation: a
// restart/deploy changes the ETag once and clients simply refetch.
var jsonOpts = protojson.MarshalOptions{UseProtoNames: true}

const (
	contentTypeJSON  = "application/json"
	contentTypeProto = "application/proto"
)

// wantsProto reports whether the client asked for binary proto. Simple
// substring membership over the Accept list — no q-value ranking; JSON is the
// default for absent/other/*/* accepts.
func wantsProto(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), contentTypeProto)
}

// marshalMessage renders m per the request's Accept header:
// application/proto -> proto.Marshal binary, anything else -> protojson.
func marshalMessage(r *http.Request, m proto.Message) (body []byte, contentType string, err error) {
	if wantsProto(r) {
		body, err = proto.Marshal(m)
		return body, contentTypeProto, err
	}
	body, err = jsonOpts.Marshal(m)
	return body, contentTypeJSON, err
}

// writeMessage is the proto-message write path every entity endpoint uses:
// content negotiation, then ETag/304 handling via writeJSON.
func writeMessage(w http.ResponseWriter, r *http.Request, m proto.Message, maxAge int) {
	body, ct, err := marshalMessage(r, m)
	if err != nil {
		internal(r.Context(), w, err)
		return
	}
	writeJSON(w, r, body, ct, maxAge)
}

// writeJSON writes body with Cache-Control public,max-age and a strong ETag,
// answering If-None-Match with 304. Despite the name (the task contract's) it
// writes any content type — the ETag input includes the content type so the
// JSON and proto renderings of one resource never share a validator.
//
// The ETag is the first 16 bytes of sha256(contentType + NUL + body), hex,
// quoted. 304 responses keep the ETag and Cache-Control headers (RFC 9110
// §15.4.5) and no body.
func writeJSON(w http.ResponseWriter, r *http.Request, body []byte, contentType string, maxAge int) {
	h := sha256.New()
	h.Write([]byte(contentType))
	h.Write([]byte{0})
	h.Write(body)
	etag := `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`

	w.Header().Set("ETag", etag)
	// The body is content-negotiated on Accept (protojson vs binary proto),
	// and Cache-Control is public: without Vary a shared cache would serve
	// one rendering to a client that asked for the other within max-age (the
	// content-type-salted ETag can't help — caches don't revalidate while
	// fresh). Add, not Set: prefab's middleware sets a static Vary: Origin
	// that must be merged, never clobbered.
	w.Header().Add("Vary", "Accept")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	if inmMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}

// inmMatches implements the If-None-Match check (RFC 9110 §13.1.2): the
// header is "*" or a comma-separated entity-tag list, compared weakly — a W/
// prefix on either side is ignored (weak comparison is the correct one for
// If-None-Match). Our own tags are hex and never contain commas; a foreign
// tag mangled by the comma split can only fail to match, never false-match.
func inmMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	etag = strings.TrimPrefix(etag, "W/")
	for _, cand := range strings.Split(header, ",") {
		cand = strings.TrimSpace(cand)
		if cand == "*" {
			return true
		}
		if strings.TrimPrefix(cand, "W/") == etag {
			return true
		}
	}
	return false
}
