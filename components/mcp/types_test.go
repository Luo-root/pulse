package mcp

import (
	"encoding/json"
	"testing"
)

func TestRequest_Marshal(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed Request
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed.JSONRPC != "2.0" {
		t.Fatalf("expected 2.0, got %s", parsed.JSONRPC)
	}
	if parsed.Method != "tools/list" {
		t.Fatalf("expected tools/list, got %s", parsed.Method)
	}
}

func TestRequest_WithParams(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: CallToolParams{
			Name:      "file_read",
			Arguments: map[string]any{"path": "/test"},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed map[string]any
	json.Unmarshal(data, &parsed)

	params, ok := parsed["params"].(map[string]any)
	if !ok {
		t.Fatal("params should be an object")
	}
	if params["name"] != "file_read" {
		t.Fatalf("expected file_read, got %s", params["name"])
	}
}

func TestResponse_WithError(t *testing.T) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      1,
		Error: &RPCError{
			Code:    -32600,
			Message: "Invalid Request",
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed Response
	json.Unmarshal(data, &parsed)

	if parsed.Error == nil {
		t.Fatal("expected error")
	}
	if parsed.Error.Code != -32600 {
		t.Fatalf("expected -32600, got %d", parsed.Error.Code)
	}
}

func TestResponse_WithResult(t *testing.T) {
	tools := []MCPTool{
		{Name: "file_read", Description: "read file"},
	}

	resultData, _ := json.Marshal(ListToolsResult{Tools: tools})

	resp := Response{
		JSONRPC: "2.0",
		ID:      1,
		Result:  resultData,
	}

	var parsed ListToolsResult
	if err := json.Unmarshal(resp.Result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(parsed.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(parsed.Tools))
	}
	if parsed.Tools[0].Name != "file_read" {
		t.Fatalf("expected file_read, got %s", parsed.Tools[0].Name)
	}
}

func TestRPCError_Error(t *testing.T) {
	e := &RPCError{Code: -1, Message: "test error"}
	if e.Error() != "test error" {
		t.Fatalf("expected 'test error', got '%s'", e.Error())
	}
}

func TestInitializeParams_Marshal(t *testing.T) {
	params := InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities: ClientCapabilities{
			Roots: &RootsCapability{ListChanged: true},
		},
		ClientInfo: ClientInfo{
			Name:    "pulse-agent",
			Version: "1.0.0",
		},
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed InitializeParams
	json.Unmarshal(data, &parsed)

	if parsed.ProtocolVersion != "2024-11-05" {
		t.Fatalf("expected 2024-11-05, got %s", parsed.ProtocolVersion)
	}
	if parsed.ClientInfo.Name != "pulse-agent" {
		t.Fatalf("expected pulse-agent, got %s", parsed.ClientInfo.Name)
	}
}

func TestMCPTool_Marshal(t *testing.T) {
	tool := MCPTool{
		Name:        "file_read",
		Description: "Read a file",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type": "string",
				},
			},
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed MCPTool
	json.Unmarshal(data, &parsed)

	if parsed.Name != "file_read" {
		t.Fatalf("expected file_read, got %s", parsed.Name)
	}
	if parsed.InputSchema["type"] != "object" {
		t.Fatalf("expected object, got %v", parsed.InputSchema["type"])
	}
}

func TestCallToolParams_Marshal(t *testing.T) {
	params := CallToolParams{
		Name: "file_read",
		Arguments: map[string]any{
			"path": "/etc/passwd",
		},
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed CallToolParams
	json.Unmarshal(data, &parsed)

	if parsed.Name != "file_read" {
		t.Fatalf("expected file_read, got %s", parsed.Name)
	}
}

func TestCallToolResult_TextContent(t *testing.T) {
	result := CallToolResult{
		Content: []ContentItem{
			{Type: "text", Text: "file contents here"},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed CallToolResult
	json.Unmarshal(data, &parsed)

	if len(parsed.Content) != 1 {
		t.Fatalf("expected 1, got %d", len(parsed.Content))
	}
	if parsed.Content[0].Text != "file contents here" {
		t.Fatalf("expected file contents here, got %s", parsed.Content[0].Text)
	}
}

func TestCallToolResult_ErrorResult(t *testing.T) {
	result := CallToolResult{
		Content: []ContentItem{
			{Type: "text", Text: "permission denied"},
		},
		IsError: true,
	}

	data, _ := json.Marshal(result)
	var parsed CallToolResult
	json.Unmarshal(data, &parsed)

	if !parsed.IsError {
		t.Fatal("expected IsError=true")
	}
}

func TestListToolsResult_Pagination(t *testing.T) {
	result := ListToolsResult{
		Tools: []MCPTool{
			{Name: "tool1"},
			{Name: "tool2"},
		},
		NextCursor: "cursor_abc",
	}

	data, _ := json.Marshal(result)
	var parsed ListToolsResult
	json.Unmarshal(data, &parsed)

	if parsed.NextCursor != "cursor_abc" {
		t.Fatalf("expected cursor_abc, got %s", parsed.NextCursor)
	}
}

func TestInitializeResult_RoundTrip(t *testing.T) {
	result := InitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: ServerCapabilities{
			Tools: &ToolsCapability{ListChanged: true},
		},
		ServerInfo: ServerInfo{
			Name:    "test-server",
			Version: "0.1.0",
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var parsed InitializeResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if parsed.ServerInfo.Name != "test-server" {
		t.Fatalf("expected test-server, got %s", parsed.ServerInfo.Name)
	}
	if parsed.Capabilities.Tools == nil {
		t.Fatal("expected tools capability")
	}
}
