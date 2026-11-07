package openapispec

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// describedOnly is what protoc-gen-openapiv2 emits for an openapiv2_operation
// response that grid.proto gives only a description: an empty schema.
const describedOnly = `{
  "swagger": "2.0",
  "info": {"title": "The Grid API", "version": "v1"},
  "produces": ["application/json"],
  "paths": {
    "/api/v1/places/{place}": {
      "get": {
        "operationId": "GridService_GetPlace",
        "parameters": [{"name": "place", "in": "path", "required": true, "type": "string"}],
        "responses": {
          "200": {"description": "ok", "schema": {"$ref": "#/definitions/rpcStatus"}},
          "304": {"description": "Not Modified.", "schema": {}},
          "404": {"description": "NOT_FOUND: unknown place.", "schema": {}}
        }
      }
    }
  },
  "definitions": {
    "rpcStatus": {"type": "object", "properties": {"code": {"type": "integer", "format": "int32"}}}
  }
}`

func TestBuild_CompletesDescribedOnlyResponses(t *testing.T) {
	out, _, err := Build([]byte(describedOnly), []byte(`
defaults:
  paths:
    /api/v1/places/{place}:
      get:
        responses:
          "304":
            headers:
              ETag: {schema: {type: string}}
`), nil)
	require.NoError(t, err)
	doc := decodeJSON(t, out)
	responses := at(t, doc, "paths", "/api/v1/places/{place}", "get", "responses").(map[string]any)

	notModified := responses["304"].(map[string]any)
	assert.NotContains(t, notModified, "content", "a 304 has no body")
	assert.Equal(t, "Not Modified.", notModified["description"])
	assert.NotNil(t, at(t, notModified, "headers", "ETag"), "defaults annotate the completed object")

	notFound := responses["404"].(map[string]any)
	assert.Equal(t, "NOT_FOUND: unknown place.", notFound["description"], "the per-RPC description is kept")
	assert.Equal(t, "#/components/schemas/rpcStatus",
		at(t, notFound, "content", "application/json", "schema", "$ref"), "errors carry the error envelope")
}

func TestBuild_RefResponseWithSiblingsFails(t *testing.T) {
	_, _, err := Build([]byte(describedOnly), []byte(`
spec:
  components:
    responses:
      NotFound: {description: Not found.}
defaults:
  paths:
    /api/v1/places/{place}:
      get:
        responses:
          "404": {$ref: "#/components/responses/NotFound"}
`), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/paths/~1api~1v1~1places~1{place}/get/responses/404: $ref with siblings (content, description)")
}

func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc
}
