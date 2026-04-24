// Package mcp implements the streamable-HTTP JSON-RPC server Claude
// Code and other MCP clients talk to. Tools are registered up-front and
// invoked via the "tools/call" method; everything else in the MCP
// surface (resources, prompts, sampling) is out of scope for step 9.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

const (
	jsonrpcVersion = "2.0"

	// Error codes per JSON-RPC 2.0 + MCP extensions.
	errParse        = -32700
	errInvalidReq   = -32600
	errMethodNF     = -32601
	errInvalidParam = -32602
	errInternal     = -32603
)

// Server handles /mcp requests. A single Server instance is created at
// startup with the full set of Tools — it's safe for concurrent use.
type Server struct {
	info     ServerInfo
	tools    map[string]Tool
	toolList []Tool
	logger   *slog.Logger
}

// ServerInfo is echoed back in initialize.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool describes one callable tool. The Handler receives the raw
// arguments map and returns either a structured result or an error.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Handler     ToolHandler     `json:"-"`
}

// ToolHandler is the runtime contract for a tool call.
type ToolHandler func(ctx context.Context, args json.RawMessage) (ToolResult, error)

// ToolResult is what a handler returns. isError distinguishes between
// a tool *executing* successfully-with-bad-result vs a transport error.
// The MCP spec expects textual content in the common case; we return
// JSON-stringified structured data in the text field so Claude Code can
// parse it directly.
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock mirrors MCP's text content shape. We only use `text` —
// images/audio/resource_link aren't needed by our tools.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// NewServer wires a server with the given toolset. Nil logger → slog.Default.
func NewServer(info ServerInfo, tools []Tool, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		info:     info,
		tools:    make(map[string]Tool, len(tools)),
		toolList: tools,
		logger:   logger,
	}
	for _, t := range tools {
		s.tools[t.Name] = t
	}
	return s
}

// ---- JSON-RPC envelope ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent in notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ServeHTTP is the one entry point. Everything between bearer auth and
// tool execution lives in dispatch().
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2MB cap — plenty
	if err != nil {
		s.logger.Error("mcp: read body", "err", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// JSON-RPC 2.0 supports batches (array of requests). Claude Code
	// uses single requests today but we handle both for compliance.
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeParseError(w, "empty body")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if trimmed[0] == '[' {
		var reqs []rpcRequest
		if err := json.Unmarshal(trimmed, &reqs); err != nil {
			writeParseError(w, "batch parse: "+err.Error())
			return
		}
		responses := make([]rpcResponse, 0, len(reqs))
		for _, req := range reqs {
			if resp, ok := s.dispatch(r.Context(), req); ok {
				responses = append(responses, resp)
			}
		}
		if len(responses) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(responses)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeParseError(w, err.Error())
		return
	}
	resp, ok := s.dispatch(r.Context(), req)
	if !ok {
		// Notification — no response per JSON-RPC 2.0.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// dispatch runs one request and returns (response, shouldReply). A
// notification (missing id) never produces a reply even on error.
func (s *Server) dispatch(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	isNotification := len(bytes.TrimSpace(req.ID)) == 0 || string(req.ID) == "null"

	if req.JSONRPC != jsonrpcVersion {
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcErr(req.ID, errInvalidReq, "jsonrpc must be 2.0", nil), true
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req), true
	case "ping":
		return rpcOK(req.ID, map[string]any{}), true
	case "tools/list":
		return rpcOK(req.ID, map[string]any{"tools": s.toolList}), true
	case "tools/call":
		if isNotification {
			// Tool calls with no id are weird but technically allowed as
			// notifications. We still run the tool so side-effects fire;
			// just don't reply.
			_, _ = s.runToolCall(ctx, req.Params)
			return rpcResponse{}, false
		}
		result, err := s.runToolCall(ctx, req.Params)
		if err != nil {
			return rpcErr(req.ID, errInvalidParam, err.Error(), nil), true
		}
		return rpcOK(req.ID, result), true
	case "notifications/initialized":
		// Client ack — ignore.
		return rpcResponse{}, false
	default:
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcErr(req.ID, errMethodNF, "method not found: "+req.Method, nil), true
	}
}

func (s *Server) handleInitialize(req rpcRequest) rpcResponse {
	// Echo the client's requested protocol version back — MCP's spec
	// lists a handful of versions (2024-11-05, 2025-03-26, etc.).
	// We're compatible with the wire shape of all of them; trust the
	// client to pick.
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		ClientInfo      json.RawMessage `json:"clientInfo"`
	}
	_ = json.Unmarshal(req.Params, &params)
	protocolVersion := params.ProtocolVersion
	if protocolVersion == "" {
		protocolVersion = "2025-06-18"
	}
	return rpcOK(req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo":      s.info,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
	})
}

func (s *Server) runToolCall(ctx context.Context, rawParams json.RawMessage) (ToolResult, error) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}
	if params.Name == "" {
		return ToolResult{}, errors.New("tool name is required")
	}
	t, ok := s.tools[params.Name]
	if !ok {
		return ToolResult{}, fmt.Errorf("tool not found: %s", params.Name)
	}
	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := t.Handler(ctx, args)
	if err != nil {
		// Tool execution errors come back as result with isError=true so
		// the LLM can see what went wrong and recover.
		return ToolResult{
			Content: []ContentBlock{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil
	}
	return result, nil
}

// ---- response helpers ----

func rpcOK(id json.RawMessage, result any) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: jsonrpcVersion, ID: id, Result: result}
}

func rpcErr(id json.RawMessage, code int, message string, data any) rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return rpcResponse{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}

func writeParseError(w http.ResponseWriter, msg string) {
	resp := rpcErr(json.RawMessage("null"), errParse, msg, nil)
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(resp)
}

// TextResult is a small helper for tool handlers that want to return
// JSON-stringified structured data inside a single text block.
func TextResult(v any) (ToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: string(b)}},
	}, nil
}
