package openapispec

import (
	"fmt"
	"sort"
	"strings"
)

// errorSchemaRef is the body of every non-2xx JSON response the API sends.
const errorSchemaRef = "#/components/schemas/rpcStatus"

// completeGeneratedResponses fills in the responses protoc-gen-openapiv2 emits
// for an `openapiv2_operation` response that grid.proto gives only a
// description. The generator writes those with an empty schema (`schema: {}`,
// which says nothing), and leaves the error body and the 304's lack of one
// unstated. Here a 304 gets no content (it has no body) and every other status
// gets the error envelope, which is what the gateway sends for each of them.
// The result is a complete inline Response Object that overlay `defaults` can
// annotate (headers, examples) without ever needing a $ref beside it.
func completeGeneratedResponses(doc map[string]any) {
	forEachResponse(doc, func(_ string, code string, resp map[string]any) {
		content, _ := resp["content"].(map[string]any)
		for mediaType, v := range content {
			mt, _ := v.(map[string]any)
			if schema, ok := mt["schema"].(map[string]any); !ok || len(schema) > 0 {
				continue
			}
			if code == "304" {
				delete(content, mediaType)
				continue
			}
			mt["schema"] = map[string]any{"$ref": errorSchemaRef}
		}
		if content != nil && len(content) == 0 {
			delete(resp, "content")
		}
	})
}

// checkResponses rejects a Response Object that is a $ref with siblings (in
// OpenAPI 3.0 a Reference Object's siblings are ignored, so the description or
// content beside it never reaches a reader), and one whose schema is empty. A
// response is either a pure $ref to components/responses or a complete inline
// object. The usual cause is an overlay default that $refs a shared response
// onto a status grid.proto already describes: annotate the generated response
// (headers, examples) instead.
func checkResponses(doc map[string]any) error {
	var bad []string
	forEachResponse(doc, func(at, _ string, resp map[string]any) {
		if _, isRef := resp["$ref"]; isRef && len(resp) > 1 {
			bad = append(bad, at+": $ref with siblings ("+strings.Join(siblingKeys(resp), ", ")+")")
		}
		content, _ := resp["content"].(map[string]any)
		for _, mediaType := range sortedKeys(content) {
			mt, _ := content[mediaType].(map[string]any)
			if schema, ok := mt["schema"].(map[string]any); ok && len(schema) == 0 {
				bad = append(bad, at+"/content/"+escape(mediaType)+": empty schema")
			}
		}
	})
	if len(bad) > 0 {
		return fmt.Errorf("invalid response objects:\n  %s", strings.Join(bad, "\n  "))
	}
	return nil
}

// forEachResponse calls fn for every Response Object under /paths and
// /components/responses, in a stable order, with its JSON pointer and status
// code (the component name for a shared response).
func forEachResponse(doc map[string]any, fn func(at, code string, resp map[string]any)) {
	paths, _ := doc["paths"].(map[string]any)
	for _, path := range sortedKeys(paths) {
		item, _ := paths[path].(map[string]any)
		for _, method := range sortedKeys(item) {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			responses, _ := op["responses"].(map[string]any)
			for _, code := range sortedKeys(responses) {
				if resp, ok := responses[code].(map[string]any); ok {
					fn("/paths/"+escape(path)+"/"+method+"/responses/"+code, code, resp)
				}
			}
		}
	}
	shared, _ := lookup(doc, "components", "responses").(map[string]any)
	for _, name := range sortedKeys(shared) {
		if resp, ok := shared[name].(map[string]any); ok {
			fn("/components/responses/"+escape(name), name, resp)
		}
	}
}

func siblingKeys(m map[string]any) []string {
	var out []string
	for k := range m {
		if k != "$ref" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
