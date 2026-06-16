// Package hitl 提供 Human-in-the-Loop 能力
// 通过 Aspect 切面和工具钩子实现人类确认点
package hitl

import (
	"context"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/flowchart/agent"
	"github.com/Luo-root/pulse/components/flowchart/node"
	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// 核心类型
// ============================================================================

// ApprovalRequest 确认请求
type ApprovalRequest struct {
	ID      string         // 唯一标识
	Type    ApprovalType   // 确认类型
	Summary string         // 操作摘要
	Details map[string]any // 详细信息
	Timeout time.Duration  // 超时（0 = 不超时）
}

// ApprovalType 确认类型
type ApprovalType string

const (
	ApprovalTool   ApprovalType = "tool"   // 工具调用确认
	ApprovalNode   ApprovalType = "node"   // 工作流节点确认
	ApprovalStep   ApprovalType = "step"   // 计划步骤确认
	ApprovalCustom ApprovalType = "custom" // 自定义确认
)

// ApprovalResponse 确认响应
type ApprovalResponse struct {
	Approved bool   // 是否批准
	Reason   string // 拒绝原因（可选）
}

// Approver 确认器接口
// 实现此接口来提供确认 UI（TUI 弹窗、Web 前端、Slack 消息等）
type Approver interface {
	// RequestApproval 请求人类确认
	RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error)
}

// ============================================================================
// Approver 实现
// ============================================================================

// ChannelApprover 基于 channel 的确认器
// 适用于 TUI 等事件驱动的 UI
type ChannelApprover struct {
	requestCh  chan<- ApprovalRequest
	responseCh <-chan ApprovalResponse
}

// NewChannelApprover 创建 channel 确认器
func NewChannelApprover(requestCh chan<- ApprovalRequest, responseCh <-chan ApprovalResponse) *ChannelApprover {
	return &ChannelApprover{requestCh: requestCh, responseCh: responseCh}
}

func (a *ChannelApprover) RequestApproval(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
	select {
	case a.requestCh <- req:
	case <-ctx.Done():
		return ApprovalResponse{Approved: false}, fmt.Errorf("hitl: timeout waiting to send approval request for %s: %w", req.ID, ctx.Err())
	}

	select {
	case resp := <-a.responseCh:
		return resp, nil
	case <-ctx.Done():
		return ApprovalResponse{Approved: false}, fmt.Errorf("hitl: timeout waiting for approval response for %s: %w", req.ID, ctx.Err())
	}
}

// AutoApprover 自动批准（用于测试或 auto 模式）
type AutoApprover struct{}

func (AutoApprover) RequestApproval(_ context.Context, _ ApprovalRequest) (ApprovalResponse, error) {
	return ApprovalResponse{Approved: true}, nil
}

// ============================================================================
// Workflow 节点确认切面
// ============================================================================

// NodeConfirmationAspect 工作流节点确认切面
// 在节点执行前请求人类确认
type NodeConfirmationAspect struct {
	approver    Approver
	shouldCheck func(node.Node) bool // 判断哪些节点需要确认
	timeout     time.Duration
}

// NewNodeConfirmationAspect 创建节点确认切面
// shouldCheck 返回 true 的节点需要确认，nil = 所有节点都需要
func NewNodeConfirmationAspect(approver Approver, shouldCheck func(node.Node) bool, timeout time.Duration) *NodeConfirmationAspect {
	return &NodeConfirmationAspect{
		approver:    approver,
		shouldCheck: shouldCheck,
		timeout:     timeout,
	}
}

func (a *NodeConfirmationAspect) Around(ctx *node.AspectContext, n node.Node, next func() (map[string]any, error)) (map[string]any, error) {
	if a.shouldCheck != nil && !a.shouldCheck(n) {
		return next()
	}

	reqCtx := ctx.Context()
	if a.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, a.timeout)
		defer cancel()
	}

	req := ApprovalRequest{
		ID:      n.ID(),
		Type:    ApprovalNode,
		Summary: fmt.Sprintf("即将执行节点: %s", n.ID()),
		Details: map[string]any{
			"inputs":  n.Inputs(),
			"outputs": n.Outputs(),
		},
		Timeout: a.timeout,
	}

	resp, err := a.approver.RequestApproval(reqCtx, req)
	if err != nil {
		return nil, fmt.Errorf("hitl: node %s confirmation error: %w", n.ID(), err)
	}
	if !resp.Approved {
		return nil, fmt.Errorf("hitl: node %s rejected by user: %s", n.ID(), resp.Reason)
	}

	return next()
}

// ============================================================================
// 工具调用确认钩子
// ============================================================================

// ToolConfirmationHook 工具调用确认钩子
// 注册到 ToolRegistry 的 BeforeExecuteHook
type ToolConfirmationHook struct {
	approver    Approver
	registry    *tools.ToolRegistry // 用于查询工具真实权限
	shouldCheck func(toolName string, perm tools.ToolPermission) bool
	timeout     time.Duration
}

// NewToolConfirmationHook 创建工具确认钩子
// shouldCheck 返回 true 的工具需要确认，nil = 仅 PermDangerous 需要确认
// registry 用于查询工具的真实权限级别
func NewToolConfirmationHook(approver Approver, registry *tools.ToolRegistry, shouldCheck func(toolName string, perm tools.ToolPermission) bool, timeout time.Duration) *ToolConfirmationHook {
	return &ToolConfirmationHook{
		approver:    approver,
		registry:    registry,
		shouldCheck: shouldCheck,
		timeout:     timeout,
	}
}

// Hook 返回可注册到 ToolRegistry 的钩子函数
func (h *ToolConfirmationHook) Hook() func(ctx context.Context, toolName string, args map[string]any) error {
	return func(ctx context.Context, toolName string, args map[string]any) error {
		// 从 registry 查询工具真实权限
		perm := tools.PermReadOnly
		if h.registry != nil {
			if tool, ok := h.registry.Get(toolName); ok {
				perm = tool.Metadata.Permission
			}
		}

		needConfirm := false
		if h.shouldCheck != nil {
			needConfirm = h.shouldCheck(toolName, perm)
		} else {
			// 默认：仅 PermDangerous 需要确认
			needConfirm = perm >= tools.PermDangerous
		}

		if !needConfirm {
			return nil
		}

		reqCtx := ctx
		if h.timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, h.timeout)
			defer cancel()
		}

		req := ApprovalRequest{
			ID:      toolName,
			Type:    ApprovalTool,
			Summary: fmt.Sprintf("工具调用: %s (权限: %s)", toolName, perm.String()),
			Details: map[string]any{
				"args":       args,
				"permission": perm.String(),
			},
			Timeout: h.timeout,
		}

		resp, err := h.approver.RequestApproval(reqCtx, req)
		if err != nil {
			return fmt.Errorf("hitl: tool %s confirmation error: %w", toolName, err)
		}
		if !resp.Approved {
			return fmt.Errorf("hitl: tool %s rejected by user: %s", toolName, resp.Reason)
		}

		return nil
	}
}

// ============================================================================
// Plan 步骤确认
// ============================================================================

// PlanConfirmation Plan 步骤确认回调
type PlanConfirmation struct {
	approver    Approver
	shouldCheck func(step *agent.ExecStep) bool
	timeout     time.Duration
}

// NewPlanConfirmation 创建计划确认
func NewPlanConfirmation(approver Approver, shouldCheck func(step *agent.ExecStep) bool, timeout time.Duration) *PlanConfirmation {
	return &PlanConfirmation{
		approver:    approver,
		shouldCheck: shouldCheck,
		timeout:     timeout,
	}
}

// StepCallback 返回可传给 PlanExecutor 的步骤回调
// 使用传入的 ctx 作为父 context，而非 context.Background()
func (pc *PlanConfirmation) StepCallback(ctx context.Context) func(step *agent.ExecStep) {
	return func(step *agent.ExecStep) {
		if pc.shouldCheck != nil && !pc.shouldCheck(step) {
			return
		}

		reqCtx := ctx
		if pc.timeout > 0 {
			var cancel context.CancelFunc
			reqCtx, cancel = context.WithTimeout(ctx, pc.timeout)
			defer cancel()
		}

		req := ApprovalRequest{
			ID:      step.PlanStep.ID,
			Type:    ApprovalStep,
			Summary: fmt.Sprintf("计划步骤: %s - %s", step.PlanStep.ID, step.PlanStep.Description),
			Details: map[string]any{
				"step_id":     step.PlanStep.ID,
				"description": step.PlanStep.Description,
				"depends_on":  step.PlanStep.DependsOn,
			},
			Timeout: pc.timeout,
		}

		resp, err := pc.approver.RequestApproval(reqCtx, req)
		if err != nil {
			// 超时或网络错误，标记为跳过
			step.Status = agent.StepSkipped
			step.Error = fmt.Sprintf("approval error: %v", err)
			return
		}
		if !resp.Approved {
			step.Status = agent.StepSkipped
			step.Error = fmt.Sprintf("skipped by user: %s", resp.Reason)
		}
	}
}

// ============================================================================
// 工具注册辅助
// ============================================================================

// RegisterWithConfirmation 注册工具并添加确认钩子
// 需要确认的工具权限级别：PermReadWrite 和 PermDangerous
func RegisterWithConfirmation(registry *tools.ToolRegistry, approver Approver) {
	hook := NewToolConfirmationHook(approver, registry, func(name string, perm tools.ToolPermission) bool {
		return perm >= tools.PermReadWrite
	}, 30*time.Second)

	registry.AddBeforeExecuteHook(hook.Hook())
}

// RegisterWithConfirmationForAll 注册工具并对所有工具添加确认钩子
func RegisterWithConfirmationForAll(registry *tools.ToolRegistry, approver Approver) {
	hook := NewToolConfirmationHook(approver, registry, func(name string, perm tools.ToolPermission) bool {
		return true
	}, 30*time.Second)

	registry.AddBeforeExecuteHook(hook.Hook())
}

// RegisterWithConfirmationForDangerous 仅对危险工具添加确认钩子
func RegisterWithConfirmationForDangerous(registry *tools.ToolRegistry, approver Approver) {
	hook := NewToolConfirmationHook(approver, registry, nil, 30*time.Second)
	registry.AddBeforeExecuteHook(hook.Hook())
}
