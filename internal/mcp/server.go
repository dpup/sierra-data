// Package mcp exposes The Grid's read-only /v1 data to LLM agents via the Model
// Context Protocol (MCP) over Streamable HTTP, mounted at /mcp
// (docs/mcp-design.md). It is a thin adapter: each tool calls the existing
// gridapi /v1 handler in-process and reshapes the JSON for LLMs — geometry is
// stripped (a polygon is a token bomb and useless to a model), and the fail-loud
// honesty contract (source_status, evacuation null-vs-0, a reference-only
// disclaimer) is preserved so a model can never render "unknown" as "all-clear".
//
// The transport is deliberately minimal: JSON-RPC 2.0 over HTTP POST with a JSON
// response. We serve no server-initiated SSE stream (the tools are stateless
// reads), so GET /mcp returns 405 per the Streamable HTTP spec. This mirrors the
// project's hand-built /v1 handlers rather than pulling in an MCP SDK.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// protocolVersion is the MCP revision we implement.
const protocolVersion = "2025-06-18"

// disclaimer rides on every tool result. The Grid is reference-only, life-safety
// data — a model relaying it must never present absence as safety.
const disclaimer = "Reference only — verify with official sources (linked in each " +
	"result). Absence of data is not an all-clear; a source marked UNAVAILABLE " +
	"means its status is unknown, not zero."

// gridHandler is the /v1 handler the tools call in-process (gridapi.Service).
type gridHandler interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// Server is the MCP endpoint. It holds the /v1 handler and the tool registry.
type Server struct {
	grid  gridHandler
	tools []tool
}

// NewHandler builds the MCP handler mounted at /mcp. grid is the gridapi /v1
// service (an http.Handler) the tools query in-process.
func NewHandler(grid gridHandler) *Server {
	s := &Server{grid: grid}
	s.tools = s.registerTools()
	return s
}

/* ------------------------------------------------------------- JSON-RPC */

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	errParse     = -32700
	errNoMethod  = -32601
	errBadParams = -32602
)

/* --------------------------------------------------------------- HTTP */

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Streamable HTTP: POST carries JSON-RPC. We expose no server→client SSE
	// stream, so GET/other verbs get 405 (spec-compliant: the client just uses
	// POST request/response).
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP endpoint accepts POST (JSON-RPC)", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{errParse, "parse error: " + err.Error()}})
		return
	}
	// Notifications (no id) get no response body, just a 202.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := s.dispatch(r.Context(), req)
	resp.JSONRPC = "2.0"
	resp.ID = req.ID
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	if resp.JSONRPC == "" {
		resp.JSONRPC = "2.0"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{Result: map[string]interface{}{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]interface{}{
				"tools":     map[string]interface{}{},
				"resources": map[string]interface{}{},
				"prompts":   map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{"name": "sierra-grid", "version": "1"},
			"instructions": "The Grid: public, read-only hazard/road/weather data for the " +
				"central Sierra (Calaveras & Tuolumne). Start with grid_situation for a place " +
				"or address. All data is reference-only; never present absence as safety.",
		}}
	case "ping":
		return rpcResponse{Result: map[string]interface{}{}}
	case "tools/list":
		return rpcResponse{Result: map[string]interface{}{"tools": s.toolList()}}
	case "tools/call":
		return s.callTool(ctx, req.Params)
	case "resources/list":
		return rpcResponse{Result: map[string]interface{}{"resources": resourceList()}}
	case "resources/read":
		return readResource(req.Params)
	case "prompts/list":
		return rpcResponse{Result: map[string]interface{}{"prompts": promptList()}}
	case "prompts/get":
		return getPrompt(req.Params)
	default:
		return rpcResponse{Error: &rpcError{errNoMethod, "method not found: " + req.Method}}
	}
}

/* ---------------------------------------------- in-process /v1 call helper */

// callV1 issues an in-process GET against the /v1 handler and returns the parsed
// JSON body and HTTP status. Reuses all the existing query, projection, and
// fail-loud logic — the tools only reshape the result.
func (s *Server) callV1(ctx context.Context, path string) (map[string]interface{}, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	cw := &captureWriter{header: http.Header{}, status: http.StatusOK}
	s.grid.ServeHTTP(cw, req)
	var body map[string]interface{}
	if cw.body.Len() > 0 {
		_ = json.Unmarshal(cw.body.Bytes(), &body)
	}
	return body, cw.status, nil
}

// captureWriter is a tiny in-memory http.ResponseWriter for the in-process call
// (avoids importing net/http/httptest into non-test code).
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) WriteHeader(code int)        { c.status = code }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
