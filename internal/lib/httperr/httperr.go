// Package httperr writes the /api/v1 error envelope from a hand-built handler.
//
// The gRPC-Gateway routes get their error bodies from prefab's gateway error
// handler: {code, codeName, message, details}. The handful of routes mounted by
// hand (the .geojson layers, /mesh/links, the ingest endpoint) do not pass
// through it, and each used to marshal a bare google.rpc.Status of its own, so
// the same API answered errors in two shapes ({code, message} on those
// routes). Write marshals the very message prefab does, with the same options,
// so a client parses one envelope whichever kind of route answered.
package httperr

import (
	"net/http"

	"github.com/dpup/prefab"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc/codes"
)

// fallbackBody is used if marshalling a literal ever fails; it keeps the shape.
const fallbackBody = `{"code":13,"codeName":"INTERNAL","message":"internal error","details":[]}`

// Write emits the error envelope under httpStatus. Errors are never cacheable,
// so the response is Cache-Control: no-store and carries no validator.
func Write(w http.ResponseWriter, httpStatus int, c codes.Code, msg string) {
	n := int32(c) //nolint:gosec // grpc codes are small non-negative constants
	body, err := prefab.JSONMarshalOptions.Marshal(&prefab.CustomErrorResponse{
		Code:     n,
		CodeName: code.Code_name[n],
		Message:  msg,
	})
	if err != nil {
		body = []byte(fallbackBody)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(body)
}
