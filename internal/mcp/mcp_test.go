package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() *Server {
	return NewServer(
		ServerInfo{Name: "rag-svc-test", Version: "0.0.0"},
		[]Tool{
			{
				Name:        "echo",
				Description: "Returns its input.",
				InputSchema: mustJSON(map[string]any{"type": "object"}),
				Handler: func(_ context.Context, raw json.RawMessage) (ToolResult, error) {
					return TextResult(map[string]any{"got": string(raw)})
				},
			},
		},
		nil,
	)
}

func postJSON(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) == 0 {
		return resp.StatusCode, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return resp.StatusCode, decoded
}

func TestInitialize(t *testing.T) {
	s := newTestServer()
	code, resp := postJSON(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if code != 200 {
		t.Fatalf("code: got %d", code)
	}
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc: %v", resp["jsonrpc"])
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result: %+v", resp)
	}
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("echoed protocolVersion: %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "rag-svc-test" {
		t.Errorf("serverInfo: %v", info)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("tools capability missing: %v", caps)
	}
}

func TestToolsListExposesRegisteredTools(t *testing.T) {
	s := newTestServer()
	_, resp := postJSON(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len: %d", len(tools))
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "echo" {
		t.Errorf("tool name: %v", first["name"])
	}
	if first["inputSchema"] == nil {
		t.Errorf("input schema missing")
	}
}

func TestToolsCallHappy(t *testing.T) {
	s := newTestServer()
	_, resp := postJSON(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"hello":"world"}}}`)
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %+v", resp)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Errorf("content type: %v", first["type"])
	}
	if !strings.Contains(first["text"].(string), "hello") {
		t.Errorf("expected echoed args in text: %v", first["text"])
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	s := newTestServer()
	_, resp := postJSON(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error, got %+v", resp)
	}
	if int(errObj["code"].(float64)) != errInvalidParam {
		t.Errorf("code: %v", errObj["code"])
	}
	if !strings.Contains(errObj["message"].(string), "tool not found") {
		t.Errorf("message: %v", errObj["message"])
	}
}

func TestUnknownMethod(t *testing.T) {
	s := newTestServer()
	_, resp := postJSON(t, s, `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error")
	}
	if int(errObj["code"].(float64)) != errMethodNF {
		t.Errorf("code: %v want %d", errObj["code"], errMethodNF)
	}
}

func TestNotificationsDoNotReply(t *testing.T) {
	s := newTestServer()
	// Notifications omit the id field; per JSON-RPC 2.0 no response should
	// be sent. HTTP status 204 signals that.
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Errorf("code: got %d want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", w.Body.String())
	}
}

func TestParseErrorOnBadJSON(t *testing.T) {
	s := newTestServer()
	code, resp := postJSON(t, s, `not json`)
	if code != 400 {
		t.Errorf("code: got %d want 400", code)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || int(errObj["code"].(float64)) != errParse {
		t.Errorf("expected parse error, got %v", resp)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("GET", "/mcp", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 405 {
		t.Errorf("code: got %d want 405", w.Code)
	}
}

func TestBatchRequest(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`[
		{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}},
		{"jsonrpc":"2.0","id":2,"method":"tools/list"}
	]`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var arr []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 {
		t.Fatalf("batch length: got %d", len(arr))
	}
}

func TestToolHandlerReturnsErrorMarksIsError(t *testing.T) {
	// A handler that returns err produces an isError=true result, not a
	// JSON-RPC error — so the LLM sees the failure instead of a transport
	// error that would retry.
	s := NewServer(ServerInfo{Name: "x", Version: "0"}, []Tool{{
		Name:        "fail",
		Description: "Always fails.",
		InputSchema: mustJSON(map[string]any{"type": "object"}),
		Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{}, ioErr("boom")
		},
	}}, nil)
	_, resp := postJSON(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"fail","arguments":{}}}`)
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("expected result not error: %+v", resp)
	}
	if result["isError"] != true {
		t.Errorf("isError: %v", result["isError"])
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content")
	}
}

// ioErr wraps a string in a bare error so the handler's failure path is
// exercised without pulling errors.New's stdlib import noise.
type ioErr string

func (e ioErr) Error() string { return string(e) }
