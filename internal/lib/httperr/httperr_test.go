package httperr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// The hand-built routes must answer errors in the gateway's envelope, so a
// client needs one parser: {code, codeName, message, details}.
func TestWriteEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	Write(w, http.StatusBadRequest, codes.InvalidArgument, "bad window")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	assert.JSONEq(t, `3`, string(body["code"]))
	assert.JSONEq(t, `"INVALID_ARGUMENT"`, string(body["codeName"]))
	assert.JSONEq(t, `"bad window"`, string(body["message"]))
	assert.JSONEq(t, `[]`, string(body["details"]))
	assert.Len(t, body, 4)
}
