// Package mcp exposes The Grid's read-only /api/v1 data to LLM agents via the
// Model Context Protocol (MCP) over Streamable HTTP, mounted at /mcp
// (docs/mcp-design.md). It is a thin adapter: each tool issues an in-process GET
// against the /api/v1 gRPC-Gateway mux and reshapes the JSON for LLMs — geometry
// is stripped (a polygon is a token bomb and useless to a model), and the
// fail-loud honesty contract (sourceStatus, evacuation null-vs-0, a
// reference-only disclaimer) is preserved so a model can never render "unknown"
// as "all-clear".
//
// The transport is deliberately minimal: JSON-RPC 2.0 over HTTP POST with a JSON
// response. We serve no server-initiated SSE stream (the tools are stateless
// reads), so GET /mcp returns 405 per the Streamable HTTP spec. This mirrors the
// project's hand-built handlers rather than pulling in an MCP SDK.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/dpup/prefab/logging"
)

// requestTimeout bounds a single MCP call. resolvePlace can chain several
// in-process /api/v1 calls including a ~10s Census geocode, and prefab sets no
// server write timeout, so without this a slow request could hang indefinitely.
const requestTimeout = 30 * time.Second

// logWarn/logErr are panic-safe: prefab injects a request-context logger in
// production, but a background/logger-less context (or the recover handler
// itself) must never panic on a nil logger — that would defeat the recovery.
func logWarn(ctx context.Context, msg string, kv ...interface{}) {
	if logging.FromContext(ctx) != nil {
		logging.Warnw(ctx, msg, kv...)
	}
}

func logErr(ctx context.Context, msg string, kv ...interface{}) {
	if logging.FromContext(ctx) != nil {
		logging.Errorw(ctx, msg, kv...)
	}
}

// protocolVersion is the MCP revision we implement.
const protocolVersion = "2025-06-18"

// disclaimer rides on every tool result. The Grid is reference-only, life-safety
// data — a model relaying it must never present absence as safety.
const disclaimer = "Reference only — verify with official sources (linked in each " +
	"result). Absence of data is not an all-clear; a source marked UNAVAILABLE " +
	"means its status is unknown, not zero."

// gridHandler is the /api/v1 handler the tools call in-process — the prefab
// gRPC-Gateway mux, which serves the whole /api/v1 surface.
type gridHandler interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// Server is the MCP endpoint. It holds the /api/v1 handler and the tool registry.
type Server struct {
	grid  gridHandler
	tools []tool
}

// NewHandler builds the MCP handler mounted at /mcp. grid is the /api/v1
// gRPC-Gateway mux (an http.Handler) the tools query in-process.
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
	errInternal  = -32603
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
		// JSON-RPC 2.0: id MUST be null when it can't be determined.
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errParse, "parse error: " + err.Error()}})
		return
	}
	// Notifications (no id) get no response body, just a 202.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Bound the call and contain panics as a clean JSON-RPC error — a tool (or
	// the in-process gateway it invokes) must not drop the connection.
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			logErr(ctx, "MCP: recovered panic in handler", "method", req.Method, "panic", rec)
			writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{errInternal, "internal error"}})
		}
	}()
	resp := s.dispatch(ctx, req)
	resp.JSONRPC = "2.0"
	resp.ID = req.ID
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	if resp.JSONRPC == "" {
		resp.JSONRPC = "2.0"
	}
	// CORS is owned by prefab's securityMiddleware (it wraps every mounted
	// handler and sets Access-Control-* from server.security.corsOrigins, and
	// handles OPTIONS preflight) — don't set it here (see internal/hazards/CLAUDE.md).
	// NOTE: corsOrigins is now "*" (open), but this POST JSON-RPC surface is still
	// not reachable cross-origin from a browser: corsAllowMethods is GET-only, so
	// the preflight this endpoint triggers is denied regardless of origin. The
	// open origin applies only to the GET data endpoints.
	w.Header().Set("Content-Type", "application/json")
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

/* ---------------------------------------------- in-process /api/v1 call helper */

// callAPI issues an in-process GET against the /api/v1 gateway mux and returns the parsed
// JSON body and HTTP status. Reuses all the existing query, projection, and
// fail-loud logic — the tools only reshape the result.
func (s *Server) callAPI(ctx context.Context, path string) (map[string]interface{}, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, err
	}
	// The /api/v1 surface is served by prefab's gRPC-Gateway, which guards
	// mutating requests with CSRF. These are trusted, read-only, in-process
	// calls: prefab's custom-header exemption (the OWASP XHR/API pattern) — the
	// mere presence of x-csrf-protection — is the intended path for a
	// programmatic client, so set it rather than thread a token/cookie.
	req.Header.Set("X-CSRF-Protection", "1")
	cw := &captureWriter{header: http.Header{}, status: http.StatusOK}
	s.grid.ServeHTTP(cw, req)
	body := map[string]interface{}{}
	if cw.body.Len() > 0 {
		if err := json.Unmarshal(cw.body.Bytes(), &body); err != nil {
			// An /api/v1 response that isn't a JSON object is a real fault — surface
			// it, never a silent empty body (which would read as "no hazards").
			return nil, cw.status, fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return body, cw.status, nil
}

// callAPIJSON is callAPI plus fail-loud status handling: a non-2xx status is an
// error, not a silently-empty result. Tools that must not present a backend
// failure as empty/no-hazard data use this; callers that need the raw status
// (grid_event's 404) use callAPI directly.
func (s *Server) callAPIJSON(ctx context.Context, path string) (map[string]interface{}, error) {
	body, code, err := s.callAPI(ctx, path)
	if err != nil {
		logWarn(ctx, "MCP: /api/v1 call failed", "path", path, "error", err.Error())
		return nil, err
	}
	if code < 200 || code >= 300 {
		msg := "upstream returned HTTP " + strconv.Itoa(code)
		if m, _ := body["message"].(string); m != "" {
			msg += ": " + m
		}
		logWarn(ctx, "MCP: /api/v1 non-2xx", "path", path, "status", code)
		return nil, fmt.Errorf("%s", msg)
	}
	return body, nil
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
