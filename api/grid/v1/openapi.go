package gridv1

import _ "embed"

// OpenAPISpec is the generated OpenAPI (Swagger 2.0) description of the /api/v1
// GridService surface, produced by protoc-gen-openapiv2 (`make proto`). It is
// embedded so the server can publish it (served at /api/openapi.json) without a
// filesystem dependency, the same way the site is embedded.
//
//go:embed grid.swagger.json
var OpenAPISpec []byte
