package gridapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutgoingHeaderMatcher(t *testing.T) {
	for key, want := range map[string]string{
		"grpc-metadata-etag":          "Etag",
		"grpc-metadata-cache-control": "Cache-Control",
		"Grpc-Metadata-Location":      "Location",
	} {
		got, ok := OutgoingHeaderMatcher(key)
		assert.True(t, ok, key)
		assert.Equal(t, want, got, key)
	}
	// gRPC's own and prefab's internal metadata never become HTTP headers.
	for _, key := range []string{"content-type", "x-http-code", "prefab-not-modified", "grpc-metadata-", "etag", "cache-control"} {
		_, ok := OutgoingHeaderMatcher(key)
		assert.False(t, ok, key)
	}
}
