package pushingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMountAnswersWrongMethodWith405 drives the endpoint through a real gateway
// mux, because that is where the bug lived: with only POST mounted, the
// gateway's own routing answered GET as 501 UNIMPLEMENTED ("Method Not
// Allowed") before ServeStream ever saw it.
func TestMountAnswersWrongMethodWith405(t *testing.T) {
	r := testRegistry(t)
	mux := runtime.NewServeMux()
	require.NoError(t, r.Mount(mux, nil))

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/v1/ingest/"+MeshStream, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			require.Equal(t, http.StatusMethodNotAllowed, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, http.MethodPost, w.Header().Get("Allow"))
			// The /api/v1 envelope, the same shape the gateway routes return.
			var body struct {
				Code     int             `json:"code"`
				CodeName string          `json:"codeName"`
				Message  string          `json:"message"`
				Details  json.RawMessage `json:"details"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body: %s", w.Body.String())
			assert.Equal(t, 12, body.Code)
			assert.Equal(t, "UNIMPLEMENTED", body.CodeName)
			assert.Contains(t, body.Message, "POST")
			assert.JSONEq(t, `[]`, string(body.Details))
		})
	}

	t.Run("HEAD", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/api/v1/ingest/"+MeshStream, nil))
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		assert.Equal(t, http.MethodPost, w.Header().Get("Allow"))
	})

	t.Run("POST still routes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/"+MeshStream, strings.NewReader(sampleReport))
		req.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		assert.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())
	})
}
