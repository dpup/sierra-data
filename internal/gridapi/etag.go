package gridapi

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// writeJSON writes body with Cache-Control public,max-age and the given ETag,
// answering If-None-Match with 304. 304 responses keep the ETag and
// Cache-Control headers (RFC 9110 §15.4.5) and no body.
func writeJSON(w http.ResponseWriter, r *http.Request, body []byte, etag, contentType string, maxAge int) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	if inmMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}

// dataETag is a weak ETag over a response's DATA: the content type plus the
// serialized parts of the body that describe it, which deliberately excludes
// the per-request generatedAt stamp. Hashing the whole body made the validator
// change every second, so a conditional GET only ever matched within the same
// second. It is weak (W/) because two responses sharing it can differ in that
// stamp, so they are semantically, not byte-for-byte, equivalent (RFC 9110
// §8.8.1).
//
// The tag is the first 16 bytes of sha256 over the content type and each part,
// every one length-prefixed so no two part lists hash alike.
func dataETag(contentType string, parts ...[]byte) string {
	h := sha256.New()
	writePart := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	writePart([]byte(contentType))
	for _, p := range parts {
		writePart(p)
	}
	return `W/"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
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
