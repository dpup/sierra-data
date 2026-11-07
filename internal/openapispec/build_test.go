package openapispec

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

// fixtureSwagger is a minimal protoc-gen-openapiv2-shaped document: placeholder
// info, one generated tag, one RPC and the google.rpc.Status definition.
const fixtureSwagger = `{
  "swagger": "2.0",
  "info": {"title": "grid.proto", "version": "version not set"},
  "tags": [{"name": "GridService"}],
  "consumes": ["application/json"],
  "produces": ["application/json"],
  "paths": {
    "/api/v1/places/{place}": {
      "get": {
        "operationId": "GridService_GetPlace",
        "parameters": [{"name": "place", "in": "path", "required": true, "type": "string"}],
        "responses": {
          "200": {"description": "A successful response.", "schema": {"$ref": "#/definitions/v1Place"}},
          "default": {"description": "An unexpected error response.", "schema": {"$ref": "#/definitions/rpcStatus"}}
        },
        "tags": ["GridService"]
      }
    }
  },
  "definitions": {
    "v1Place": {"type": "object", "properties": {"slug": {"type": "string"}}},
    "rpcStatus": {"type": "object", "properties": {"code": {"type": "integer", "format": "int32"}, "message": {"type": "string"}}}
  }
}`

func build(t *testing.T, overlay string) (map[string]any, Report, error) {
	t.Helper()
	out, rep, err := Build([]byte(fixtureSwagger), []byte(overlay), nil)
	if err != nil {
		return nil, rep, err
	}
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	return doc, rep, nil
}

func at(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		require.Truef(t, ok, "not an object at %q in %v", p, path)
		cur = m[p]
	}
	return cur
}

// Importing a well-known type file (struct.proto) makes the generator emit all
// its types; one nothing references is dropped, one in use is kept.
func TestBuild_PrunesUnusedWellKnownSchemas(t *testing.T) {
	swagger := strings.Replace(fixtureSwagger,
		`"rpcStatus": {"type": "object", "properties": {"code": {"type": "integer", "format": "int32"}, "message": {"type": "string"}}}`,
		`"rpcStatus": {"type": "object", "properties": {"code": {"type": "integer", "format": "int32"}, "message": {"type": "string"},
		   "details": {"type": "array", "items": {"$ref": "#/definitions/protobufAny"}}}},
		 "protobufAny": {"type": "object", "properties": {"@type": {"type": "string"}}},
		 "protobufNullValue": {"type": "string", "enum": ["NULL_VALUE"], "default": "NULL_VALUE"}`, 1)
	require.NotEqual(t, fixtureSwagger, swagger, "fixture edit applied")
	out, _, err := Build([]byte(swagger), nil, nil)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	schemas := at(t, doc, "components", "schemas").(map[string]any)
	assert.Contains(t, schemas, "protobufAny", "referenced by rpcStatus.details")
	assert.NotContains(t, schemas, "protobufNullValue", "referenced by nothing")
	assert.Contains(t, schemas, "v1Place")
}

func TestBuild_ConvertsToOpenAPI3(t *testing.T) {
	doc, _, err := build(t, "")
	require.NoError(t, err)
	assert.Equal(t, "3.0.3", doc["openapi"])
	assert.Equal(t, "#/components/schemas/v1Place",
		at(t, doc, "paths", "/api/v1/places/{place}", "get", "responses", "200", "content", "application/json", "schema", "$ref"))
}

func TestBuild_OwnedContentIsAdded(t *testing.T) {
	doc, _, err := build(t, `
spec:
  paths:
    /api/v1/mesh/links:
      get:
        responses:
          "200": {description: ok, content: {application/json: {schema: {$ref: "#/components/schemas/Links"}}}}
  components:
    schemas:
      Links: {type: object}
`)
	require.NoError(t, err)
	assert.NotNil(t, at(t, doc, "paths", "/api/v1/mesh/links", "get"))
	assert.NotNil(t, at(t, doc, "components", "schemas", "Links"))
	assert.NotNil(t, at(t, doc, "components", "schemas", "v1Place"), "generated schemas survive")
}

func TestBuild_OwnedPathAlreadyGeneratedFails(t *testing.T) {
	_, _, err := build(t, `
spec:
  paths:
    /api/v1/places/{place}:
      get: {responses: {"200": {description: ok}}}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/paths/~1api~1v1~1places~1{place} is already in the generated spec")
}

func TestBuild_OwnedSchemaAlreadyGeneratedFails(t *testing.T) {
	_, _, err := build(t, `
spec:
  components:
    schemas:
      rpcStatus: {type: object}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/components/schemas/rpcStatus is already in the generated spec")
}

func TestBuild_OwnedScalarConflictFails(t *testing.T) {
	_, _, err := build(t, `
spec:
  openapi: 3.1.0
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/openapi: overlay value conflicts")
}

func TestBuild_DefaultsFillGapsAndGeneratedWins(t *testing.T) {
	doc, rep, err := build(t, `
defaults:
  components:
    schemas:
      rpcStatus:
        properties:
          code: {type: string, description: Numeric gRPC code.}
          codeName: {type: string}
`)
	require.NoError(t, err)
	code := at(t, doc, "components", "schemas", "rpcStatus", "properties", "code").(map[string]any)
	assert.Equal(t, "integer", code["type"], "generated value wins")
	assert.Equal(t, "Numeric gRPC code.", code["description"], "gap filled")
	assert.NotNil(t, at(t, doc, "components", "schemas", "rpcStatus", "properties", "codeName"))
	assert.Equal(t, []string{"/components/schemas/rpcStatus/properties/code/type"}, rep.Shadowed)
}

func TestBuild_DefaultsReplaceGeneratorPlaceholders(t *testing.T) {
	doc, rep, err := build(t, `
defaults:
  info: {title: The Grid API, version: v1}
`)
	require.NoError(t, err)
	assert.Equal(t, "The Grid API", at(t, doc, "info", "title"))
	assert.Equal(t, "v1", at(t, doc, "info", "version"))
	assert.Empty(t, rep.Shadowed)
}

func TestBuild_DefaultsDoNotReplaceRealProtoValues(t *testing.T) {
	annotated := strings.Replace(fixtureSwagger,
		`"info": {"title": "grid.proto", "version": "version not set"}`,
		`"info": {"title": "Grid", "version": "2"}`, 1)
	out, rep, err := Build([]byte(annotated), []byte("defaults:\n  info: {title: The Grid API, version: v1}\n"), nil)
	require.NoError(t, err)
	assert.Contains(t, string(out), `"title": "Grid"`)
	assert.ElementsMatch(t, []string{"/info/title", "/info/version"}, rep.Shadowed)
}

func TestBuild_DefaultsOnMissingOperationFails(t *testing.T) {
	_, _, err := build(t, `
defaults:
  paths:
    /api/v1/renamed:
      get: {responses: {"404": {description: gone}}}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist in the generated spec")

	_, _, err = build(t, `
defaults:
  paths:
    /api/v1/places/{place}:
      post: {responses: {"404": {description: gone}}}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/post does not exist in the generated spec")
}

func TestBuild_TagsMergeByName(t *testing.T) {
	doc, _, err := build(t, `
defaults:
  tags:
    - {name: GridService, description: The read surface.}
spec:
  tags:
    - {name: Map layers, description: GeoJSON.}
  paths:
    /api/v1/map:
      get:
        tags: [Map layers]
        responses:
          "200": {description: ok}
`)
	require.NoError(t, err)
	tags := doc["tags"].([]any)
	require.Len(t, tags, 2)
	assert.Equal(t, map[string]any{"name": "GridService", "description": "The read surface."}, tags[0])
	assert.Equal(t, "Map layers", tags[1].(map[string]any)["name"])
}

func TestBuild_PrunesUnusedTags(t *testing.T) {
	doc, _, err := build(t, `
spec:
  tags:
    - {name: Orphan, description: No operation carries this.}
`)
	require.NoError(t, err)
	var names []string
	for _, tag := range doc["tags"].([]any) {
		names = append(names, tag.(map[string]any)["name"].(string))
	}
	assert.Equal(t, []string{"GridService"}, names)
}

func TestBuild_ServerURLHasNoTrailingSlash(t *testing.T) {
	// The generator emits host + schemes when grid.proto sets them.
	swagger := strings.Replace(fixtureSwagger, `"swagger": "2.0",`, `"swagger": "2.0", "host": "example.org", "schemes": ["https"],`, 1)
	require.NotEqual(t, fixtureSwagger, swagger, "fixture shape changed")
	out, _, err := Build([]byte(swagger), nil, nil)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	servers := doc["servers"].([]any)
	require.NotEmpty(t, servers)
	for _, srv := range servers {
		assert.NotRegexp(t, `/$`, srv.(map[string]any)["url"])
	}
}

func TestBuild_DanglingRefFails(t *testing.T) {
	_, _, err := build(t, `
spec:
  paths:
    /api/v1/x:
      get:
        responses:
          "200": {description: ok, content: {application/json: {schema: {$ref: "#/components/schemas/Nope"}}}}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dangling $ref")
	assert.Contains(t, err.Error(), "-> #/components/schemas/Nope")
}

func TestBuild_ExampleMustMatchSchema(t *testing.T) {
	_, _, err := build(t, `
spec:
  paths:
    /api/v1/x:
      get:
        responses:
          "200":
            description: ok
            content:
              application/json:
                schema: {type: object, properties: {severity: {type: string, enum: [INFO, MINOR]}}}
                example: {severity: BOGUS}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")

	// Named examples (the form the map-layer route uses) are checked too.
	_, _, err = build(t, `
spec:
  paths:
    /api/v1/x:
      get:
        responses:
          "200":
            description: ok
            content:
              application/json:
                schema: {type: object, properties: {severity: {type: string, enum: [INFO, MINOR]}}}
                examples:
                  good: {value: {severity: INFO}}
                  bad: {value: {severity: BOGUS}}
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad")
}

func TestBuild_UnknownOverlaySectionFails(t *testing.T) {
	_, _, err := build(t, "paths: {}\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown top-level key "paths"`)
}

func TestBuild_IsDeterministic(t *testing.T) {
	overlay := "spec:\n  components:\n    schemas:\n      A: {type: object}\n      B: {type: object}\n"
	first, _, err := Build([]byte(fixtureSwagger), []byte(overlay), nil)
	require.NoError(t, err)
	for range 5 {
		again, _, err := Build([]byte(fixtureSwagger), []byte(overlay), nil)
		require.NoError(t, err)
		require.True(t, bytes.Equal(first, again))
	}
	assert.True(t, bytes.HasPrefix(first, []byte("{\n  \"openapi\": \"3.0.3\",\n  \"info\":")), "top-level order is fixed")
}

// TestCommittedSpecIsCurrent fails when api/grid/v1/openapi.json was not rebuilt
// after an edit to the overlay or a proto regeneration. Fix: `make openapi`.
func TestCommittedSpecIsCurrent(t *testing.T) {
	dir := "../../api/grid/v1/"
	swagger, err := os.ReadFile(dir + "grid.swagger.json")
	require.NoError(t, err)
	overlay, err := os.ReadFile(dir + "openapi.overlay.yaml")
	require.NoError(t, err)
	committed, err := os.ReadFile(dir + "openapi.json")
	require.NoError(t, err)

	want, _, err := Build(swagger, overlay, gridv1.File_grid_proto)
	require.NoError(t, err)
	if !bytes.Equal(want, committed) {
		t.Fatal("api/grid/v1/openapi.json is stale: run `make openapi` (or `make proto`) and commit the result")
	}
}
