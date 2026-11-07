package gridv1

import _ "embed"

// OpenAPISpec is the OpenAPI 3.0 description of the whole public surface, served
// at /api/openapi.json. It is built by cmd/openapi-merge (`make openapi`, also
// run by `make proto`): protoc-gen-openapiv2's grid.swagger.json converted to
// OpenAPI 3.0, with openapi.overlay.yaml merged in for everything the generator
// cannot see (the hand-built routes, the error envelope's codeName). Embedded so
// the server publishes it without a filesystem dependency, like the site.
//
//go:embed openapi.json
var OpenAPISpec []byte
