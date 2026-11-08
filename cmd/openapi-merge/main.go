// Command openapi-merge builds the OpenAPI document the server publishes at
// /api/openapi.json: the protoc-gen-openapiv2 output for grid.proto, converted to
// OpenAPI 3.0, with the hand-written overlay merged in (see internal/openapispec).
//
// `make proto` runs it after protoc, and `make openapi` runs it alone (an overlay
// edit needs no protoc). It fails, writing nothing, on a merge conflict, a
// dangling $ref or an invalid result.
//
//	go run ./cmd/openapi-merge \
//	  -swagger api/grid/v1/grid.swagger.json \
//	  -overlay api/grid/v1/openapi.overlay.yaml \
//	  -out api/grid/v1/openapi.json
//
// -check compares instead of writing, and exits 1 when -out is stale.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/openapispec"
)

func main() {
	swaggerPath := flag.String("swagger", "api/grid/v1/grid.swagger.json", "generated Swagger 2.0 input")
	overlayPath := flag.String("overlay", "api/grid/v1/openapi.overlay.yaml", "hand-written overlay")
	outPath := flag.String("out", "api/grid/v1/openapi.json", "merged OpenAPI 3.0 output")
	check := flag.Bool("check", false, "fail if -out is not up to date instead of writing it")
	flag.Parse()

	if err := run(*swaggerPath, *overlayPath, *outPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-merge:", err)
		os.Exit(1)
	}
}

func run(swaggerPath, overlayPath, outPath string, check bool) error {
	swagger, err := os.ReadFile(swaggerPath)
	if err != nil {
		return err
	}
	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		return err
	}
	out, rep, err := openapispec.Build(swagger, overlay, gridv1.File_grid_proto)
	if err != nil {
		return err
	}
	for _, p := range rep.Shadowed {
		fmt.Fprintf(os.Stderr, "openapi-merge: note: grid.proto now supplies %s; the overlay default for it is unused and can be deleted\n", p)
	}
	if check {
		cur, err := os.ReadFile(outPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(cur, out) {
			return fmt.Errorf("%s is stale; run `make openapi`", outPath)
		}
		return nil
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("OpenAPI spec written to %s\n", outPath)
	return nil
}
