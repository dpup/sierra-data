package gridapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// contentTypeJSON is the content type the hand-built summary endpoint writes
// through writeJSON (the .geojson map layers pass "application/geo+json").
const contentTypeJSON = "application/json"

// writeJSON writes body with Cache-Control public,max-age and a strong ETag,
// answering If-None-Match with 304. It writes any content type — the ETag input
// includes the content type so two renderings never share a validator.
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
