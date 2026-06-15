package hitl

import (
	"context"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/flowchart/agent"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

// mockApprover 用于测试的确认器
type mockApprover struct {
	responses map[string]ApprovalResponse
	calls     []ApprovalRequest
}

func newMockApprover() *mockApprover {
	return &mockApprover{
		responses: make(map[string]ApprovalResponse),
	}
}

func (m *mockApprover) setResponse(id string, resp ApprovalResponse) {
	m.responses[id] = resp
}

func (m *mockApprover) RequestApproval(_ context.Context, req ApprovalRequest) (ApprovalResponse, error) {
	m.calls = append(m.calls, req)
	if resp, ok := m.responses[req.ID]; ok {
		return resp, nil
	}
	return ApprovalResponse{Approved: true}, nil
}

func TestAutoApprover(t *testing.T) {
	approver := AutoApprover{}
	resp, err := approver.RequestApproval(context.Background(), ApprovalRequest{ID: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Approved {
		t.Error("expected AutoApprover to approve")
	}
}

func TestChannelApprover(t *testing.T) {
	reqCh := make(chan ApprovalRequest, 1)
	respCh := make(chan ApprovalResponse, 1)
	approver := NewChannelApprover(reqCh, respCh)

	go func() {
		req := <-reqCh
		if req.ID != "test-tool" {
			t.Errorf("expected request ID 'test-tool', got %q", req.ID)
		}
		respCh <- ApprovalResponse{Approved: true}
	}()

	resp, err := approver.RequestApproval(context.Background(), ApprovalRequest{ID: "test-tool"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Approved {
		t.Error("expected approval")
	}
}

func TestChannelApprover_Timeout(t *testing.T) {
	reqCh := make(chan ApprovalRequest) // unbuffered, nobody reads
	respCh := make(chan ApprovalResponse)
	approver := NewChannelApprover(reqCh, respCh)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := approver.RequestApproval(ctx, ApprovalRequest{ID: "timeout-test"})
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestChannelApprover_Rejection(t *testing.T) {
	reqCh := make(chan ApprovalRequest, 1)
	respCh := make(chan ApprovalResponse, 1)
	approver := NewChannelApprover(reqCh, respCh)

	go func() {
		<-reqCh
		respCh <- ApprovalResponse{Approved: false, Reason: "not safe"}
	}()

	resp, err := approver.RequestApproval(context.Background(), ApprovalRequest{ID: "reject"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Approved {
		t.Error("expected rejection")
	}
	if resp.Reason != "not safe" {
		t.Errorf("expected reason 'not safe', got %q", resp.Reason)
	}
}

func TestToolConfirmationHook_Approved(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterSimple("file_read", "read file", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermReadOnly))

	approver := newMockApprover()
	hook := NewToolConfirmationHook(approver, registry, func(name string, perm tools.ToolPermission) bool {
		return perm >= tools.PermDangerous
	}, 5*time.Second)

	hookFn := hook.Hook()
	err := hookFn(context.Background(), "file_read", map[string]any{"path": "/tmp/test"})
	if err != nil {
		t.Fatalf("expected no error for ReadOnly tool, got: %v", err)
	}
	// ReadOnly should not trigger confirmation
	if len(approver.calls) != 0 {
		t.Errorf("expected no approval calls for ReadOnly tool, got %d", len(approver.calls))
	}
}

func TestToolConfirmationHook_DangerousApproved(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterSimple("cmd_exec", "execute command", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermDangerous))

	approver := newMockApprover()
	approver.setResponse("cmd_exec", ApprovalResponse{Approved: true})

	hook := NewToolConfirmationHook(approver, registry, func(name string, perm tools.ToolPermission) bool {
		return perm >= tools.PermDangerous
	}, 5*time.Second)

	hookFn := hook.Hook()
	err := hookFn(context.Background(), "cmd_exec", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatalf("expected no error when approved, got: %v", err)
	}
	if len(approver.calls) != 1 {
		t.Fatalf("expected 1 approval call, got %d", len(approver.calls))
	}
	if approver.calls[0].Details["permission"] != tools.PermDangerous.String() {
		t.Errorf("expected permission 'dangerous', got %v", approver.calls[0].Details["permission"])
	}
}

func TestToolConfirmationHook_DangerousRejected(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterSimple("cmd_exec", "execute command", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermDangerous))

	approver := newMockApprover()
	approver.setResponse("cmd_exec", ApprovalResponse{Approved: false, Reason: "too risky"})

	hook := NewToolConfirmationHook(approver, registry, nil, 5*time.Second)

	hookFn := hook.Hook()
	err := hookFn(context.Background(), "cmd_exec", map[string]any{"command": "rm -rf /"})
	if err == nil {
		t.Fatal("expected error when rejected")
	}
}

func TestToolConfirmationHook_NilRegistry(t *testing.T) {
	approver := newMockApprover()
	hook := NewToolConfirmationHook(approver, nil, nil, 5*time.Second)

	hookFn := hook.Hook()
	// Without registry, should default to PermReadOnly, which is not dangerous
	err := hookFn(context.Background(), "unknown_tool", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestToolConfirmationHook_Timeout(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterSimple("cmd_exec", "execute command", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermDangerous))

	reqCh := make(chan ApprovalRequest) // nobody reads
	respCh := make(chan ApprovalResponse)
	approver := NewChannelApprover(reqCh, respCh)

	hook := NewToolConfirmationHook(approver, registry, func(name string, perm tools.ToolPermission) bool {
		return true
	}, 100*time.Millisecond)

	hookFn := hook.Hook()
	err := hookFn(context.Background(), "cmd_exec", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRegisterWithConfirmation(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterSimple("file_write", "write file", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermReadWrite))
	registry.RegisterSimple("file_read", "read file", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermReadOnly))

	approver := newMockApprover()
	RegisterWithConfirmation(registry, approver)

	// ReadOnly should not trigger
	result := registry.Execute(context.Background(), schema.ToolCall{
		ID:       "call-1",
		Function: schema.FunctionCall{Name: "file_read", Arguments: `{"path":"/tmp"}`},
	})
	if result.IsError {
		t.Fatalf("ReadOnly should not need confirmation: %s", result.Content)
	}

	// ReadWrite should trigger (auto-approved by mock)
	result = registry.Execute(context.Background(), schema.ToolCall{
		ID:       "call-2",
		Function: schema.FunctionCall{Name: "file_write", Arguments: `{"path":"/tmp/test","content":"data"}`},
	})
	if result.IsError {
		t.Fatalf("ReadWrite with auto-approve should succeed: %s", result.Content)
	}
	if len(approver.calls) != 1 {
		t.Errorf("expected 1 approval call for ReadWrite, got %d", len(approver.calls))
	}
}

func TestPlanConfirmation_StepCallback(t *testing.T) {
	approver := newMockApprover()
	approver.setResponse("step-1", ApprovalResponse{Approved: true})

	pc := NewPlanConfirmation(approver, nil, 5*time.Second)
	callback := pc.StepCallback(context.Background())

	step := &agent.ExecStep{
		PlanStep: agent.PlanStep{
			ID:          "step-1",
			Description: "test step",
			DependsOn:   []string{},
		},
	}

	callback(step)

	if step.Status == agent.StepSkipped {
		t.Error("expected step to not be skipped when approved")
	}
	if len(approver.calls) != 1 {
		t.Fatalf("expected 1 approval call, got %d", len(approver.calls))
	}
}

func TestPlanConfirmation_StepCallback_Rejected(t *testing.T) {
	approver := newMockApprover()
	approver.setResponse("step-2", ApprovalResponse{Approved: false, Reason: "skip this"})

	pc := NewPlanConfirmation(approver, nil, 5*time.Second)
	callback := pc.StepCallback(context.Background())

	step := &agent.ExecStep{
		PlanStep: agent.PlanStep{
			ID:          "step-2",
			Description: "risky step",
		},
	}

	callback(step)

	if step.Status != agent.StepSkipped {
		t.Errorf("expected step to be skipped, got %v", step.Status)
	}
	if step.Error != "skipped by user: skip this" {
		t.Errorf("expected error message about skipping, got %q", step.Error)
	}
}

func TestPlanConfirmation_StepCallback_ShouldCheck(t *testing.T) {
	approver := newMockApprover()

	pc := NewPlanConfirmation(approver, func(step *agent.ExecStep) bool {
		return step.PlanStep.ID == "needs-approval"
	}, 5*time.Second)
	callback := pc.StepCallback(context.Background())

	// This step should NOT trigger approval
	step1 := &agent.ExecStep{
		PlanStep: agent.PlanStep{ID: "auto-step"},
	}
	callback(step1)
	if len(approver.calls) != 0 {
		t.Errorf("expected no approval for filtered-out step, got %d", len(approver.calls))
	}

	// This step SHOULD trigger approval
	step2 := &agent.ExecStep{
		PlanStep: agent.PlanStep{ID: "needs-approval"},
	}
	callback(step2)
	if len(approver.calls) != 1 {
		t.Errorf("expected 1 approval call, got %d", len(approver.calls))
	}
}

func TestPlanConfirmation_StepCallback_Timeout(t *testing.T) {
	reqCh := make(chan ApprovalRequest)
	respCh := make(chan ApprovalResponse)
	approver := NewChannelApprover(reqCh, respCh)

	pc := NewPlanConfirmation(approver, nil, 100*time.Millisecond)
	callback := pc.StepCallback(context.Background())

	step := &agent.ExecStep{
		PlanStep: agent.PlanStep{ID: "timeout-step"},
	}

	callback(step)

	if step.Status != agent.StepSkipped {
		t.Errorf("expected step to be skipped on timeout, got %v", step.Status)
	}
	if step.Error == "" {
		t.Error("expected error message about timeout")
	}
}

func TestApprovalRequest_Fields(t *testing.T) {
	req := ApprovalRequest{
		ID:      "test-123",
		Type:    ApprovalTool,
		Summary: "run dangerous command",
		Details: map[string]any{
			"command": "rm -rf /tmp/test",
			"args":    []string{"-rf"},
		},
		Timeout: 30 * time.Second,
	}

	if req.ID != "test-123" {
		t.Errorf("expected ID 'test-123', got %q", req.ID)
	}
	if req.Type != ApprovalTool {
		t.Errorf("expected type 'tool', got %q", req.Type)
	}
	if req.Timeout != 30*time.Second {
		t.Errorf("expected 30s timeout, got %v", req.Timeout)
	}
}

func TestApprovalTypes(t *testing.T) {
	tests := []struct {
		input    ApprovalType
		expected string
	}{
		{ApprovalTool, "tool"},
		{ApprovalNode, "node"},
		{ApprovalStep, "step"},
		{ApprovalCustom, "custom"},
	}

	for _, tt := range tests {
		if string(tt.input) != tt.expected {
			t.Errorf("expected %q, got %q", tt.expected, string(tt.input))
		}
	}
}

func TestRegisterWithConfirmationForDangerous(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.RegisterSimple("dangerous_cmd", "dangerous", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermDangerous))
	registry.RegisterSimple("safe_read", "safe", func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	}, tools.WithPermission(tools.PermReadOnly))

	approver := newMockApprover()
	RegisterWithConfirmationForDangerous(registry, approver)

	// Safe tool should not trigger
	result := registry.Execute(context.Background(), schema.ToolCall{
		ID:       "call-1",
		Function: schema.FunctionCall{Name: "safe_read"},
	})
	if result.IsError {
		t.Fatalf("safe tool should not need confirmation: %s", result.Content)
	}

	// Dangerous tool should trigger (auto-approved)
	result = registry.Execute(context.Background(), schema.ToolCall{
		ID:       "call-2",
		Function: schema.FunctionCall{Name: "dangerous_cmd"},
	})
	if result.IsError {
		t.Fatalf("dangerous tool with auto-approve should succeed: %s", result.Content)
	}
	if len(approver.calls) != 1 {
		t.Errorf("expected 1 approval for dangerous, got %d", len(approver.calls))
	}
}
