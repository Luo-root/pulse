package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/agent"
	"github.com/Luo-root/pulse/components/flow"
	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// Helper Functions
// ============================================================================

func planJSON(tasks []map[string]any) string {
	b, _ := json.Marshal(map[string]any{"tasks": tasks})
	return string(b)
}

func taskJSON(outputs map[string]any) string {
	b, _ := json.Marshal(outputs)
	return string(b)
}

func toNodes(sn []*SimpleNode) []Node {
	r := make([]Node, len(sn))
	for i, n := range sn {
		r[i] = n
	}
	return r
}

// createPlannerMockAgent 创建用于 Planner 的 MockAgent
func createPlannerMockAgent(handler func(ctx context.Context, prompt string) (string, error)) *agent.MockAgent {
	mock := agent.NewMockAgent()

	// 使用 WithOnSend 钩子来根据 prompt 内容返回不同响应
	mock.WithOnSend(func(msg *schema.Message) {
		// 这里不直接设置响应，而是通过 handler 在 Send 时处理
	})

	// MockAgent 的设计是基于文本匹配，我们需要适配一下
	// 由于 planner_test 需要动态根据 prompt 返回，我们采用 fallback 方式
	mock.WithFallback("[MOCK]")

	return mock
}

// mockAgentAdapter 将旧的 handler 风格适配到 MockAgent
type mockAgentAdapter struct {
	handler func(ctx context.Context, prompt string) (string, error)
}

func (m *mockAgentAdapter) SendMessage(ctx context.Context, msg *schema.Message) (*schema.Message, error) {
	if m.handler == nil {
		return &schema.Message{Role: schema.AssistantRole, Content: "{}"}, nil
	}
	content, err := m.handler(ctx, msg.TextContent())
	if err != nil {
		return nil, err
	}
	return &schema.Message{Role: schema.AssistantRole, Content: content}, nil
}

func (m *mockAgentAdapter) SendMessageStream(ctx context.Context, msg *schema.Message, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	resp, err := m.SendMessage(ctx, msg)
	if err != nil {
		return nil, err
	}

	if onChunk != nil && resp.Content != "" {
		for i := 0; i < len(resp.Content); i++ {
			chunk := &schema.Message{
				Role:    schema.AssistantRole,
				Content: string(resp.Content[i]),
			}
			if !onChunk(chunk, false) {
				return nil, errors.New("stream cancelled")
			}
		}
	}

	return resp, nil
}

// Ensure mockAgentAdapter implements AgentInterface
var _ agent.AgentInterface = (*mockAgentAdapter)(nil)

// ============================================================================
// PlannerNode
// ============================================================================

func TestPlannerNode_Basic(t *testing.T) {
	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return planJSON([]map[string]any{
				{"id": "task_1", "description": "Get path", "inputs": []any{}, "outputs": []any{"path"}},
				{"id": "task_2", "description": "List files", "inputs": []any{"path"}, "outputs": []any{"files"}},
			}), nil
		},
	}

	pn := NewPlannerNode("planner", adapter)

	if pn.ID() != "planner" {
		t.Fatalf("ID: want planner, got %s", pn.ID())
	}
	if len(pn.Inputs()) != 1 || pn.Inputs()[0] != "user_goal" {
		t.Fatalf("Inputs: want [user_goal], got %v", pn.Inputs())
	}
	if len(pn.Outputs()) != 1 || pn.Outputs()[0] != "planner_plan" {
		t.Fatalf("Outputs: want [planner_plan], got %v", pn.Outputs())
	}

	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("user_goal", "test goal")

	outputs, err := pn.Run(ctx, map[string]any{"user_goal": "test goal"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	plan, ok := outputs["planner_plan"].(*Plan)
	if !ok {
		t.Fatalf("want *Plan, got %T", outputs["planner_plan"])
	}
	if plan.Goal != "test goal" {
		t.Fatalf("Goal: want 'test goal', got %q", plan.Goal)
	}
	if len(plan.Tasks) != 2 {
		t.Fatalf("Tasks: want 2, got %d", len(plan.Tasks))
	}
	if plan.Tasks[0].ID != "task_1" || plan.Tasks[1].ID != "task_2" {
		t.Fatalf("IDs: want [task_1, task_2], got [%s, %s]", plan.Tasks[0].ID, plan.Tasks[1].ID)
	}
	for _, task := range plan.Tasks {
		if task.State != TaskPending {
			t.Fatalf("Task %s state: want Pending, got %v", task.ID, task.State)
		}
	}
}

func TestPlannerNode_MarkdownWrapped(t *testing.T) {
	raw := planJSON([]map[string]any{
		{"id": "task_1", "description": "test", "inputs": []any{}, "outputs": []any{"out"}},
	})

	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return "Here is the plan:\n```json\n" + raw + "\nDone.", nil
		},
	}

	pn := NewPlannerNode("p", adapter)
	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("user_goal", "test")

	outputs, err := pn.Run(ctx, map[string]any{"user_goal": "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	planVal, ok := outputs["p_plan"]
	if !ok {
		t.Fatal("p_plan not found in outputs")
	}

	plan, ok := planVal.(*Plan)
	if !ok {
		t.Fatalf("want *Plan, got %T (value: %v)", planVal, planVal)
	}

	if len(plan.Tasks) != 1 || plan.Tasks[0].ID != "task_1" {
		t.Fatalf("parsed plan incorrect: %+v", plan.Tasks)
	}
}

func TestPlannerNode_AgentError(t *testing.T) {
	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("LLM unavailable")
		},
	}

	pn := NewPlannerNode("p", adapter)
	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("user_goal", "test")

	_, err := pn.Run(ctx, map[string]any{"user_goal": "test"})
	if err == nil {
		t.Fatal("expected error when agent fails")
	}
}

func TestPlannerNode_BadJSON(t *testing.T) {
	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return "I cannot create a plan in JSON format", nil
		},
	}

	pn := NewPlannerNode("p", adapter)
	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("user_goal", "test")

	_, err := pn.Run(ctx, map[string]any{"user_goal": "test"})
	if err == nil {
		t.Fatal("expected error for unparseable response")
	}
}

// ============================================================================
// TaskNode
// ============================================================================

func TestTaskNode_Success(t *testing.T) {
	task := Task{ID: "task_1", Description: "test", Inputs: []string{}, Outputs: []string{"out"}}
	plan := NewPlan("goal")
	plan.Tasks = []Task{task}
	plan.mu.Lock()
	plan.Tasks[0].State = TaskPending
	plan.mu.Unlock()

	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "任务ID: task_1") {
				t.Errorf("prompt missing task_1 identifier")
			}
			return taskJSON(map[string]any{"out": "hello"}), nil
		},
	}

	tn := NewTaskNode("planner", task, adapter)

	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("planner_plan", plan)

	outputs, err := tn.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["out"] != "hello" {
		t.Fatalf("output: want 'hello', got %v", outputs["out"])
	}

	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.Tasks[0].State != TaskSuccess {
		t.Fatalf("state: want Success, got %v", plan.Tasks[0].State)
	}
	if plan.Tasks[0].Result["out"] != "hello" {
		t.Fatalf("plan result: want 'hello', got %v", plan.Tasks[0].Result)
	}
}

func TestTaskNode_MultipleOutputs(t *testing.T) {
	task := Task{ID: "task_1", Description: "test", Inputs: []string{}, Outputs: []string{"a", "b", "c"}}
	plan := NewPlan("goal")
	plan.Tasks = []Task{task}
	plan.mu.Lock()
	plan.Tasks[0].State = TaskPending
	plan.mu.Unlock()

	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return taskJSON(map[string]any{"a": 1, "b": "two", "c": true}), nil
		},
	}

	tn := NewTaskNode("planner", task, adapter)

	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("planner_plan", plan)

	outputs, err := tn.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["a"] != float64(1) || outputs["b"] != "two" || outputs["c"] != true {
		t.Fatalf("outputs: %v", outputs)
	}
}

func TestTaskNode_AgentError_SoftFailure(t *testing.T) {
	task := Task{ID: "task_1", Description: "test", Inputs: []string{}, Outputs: []string{"out"}}
	plan := NewPlan("goal")
	plan.Tasks = []Task{task}
	plan.mu.Lock()
	plan.Tasks[0].State = TaskPending
	plan.mu.Unlock()

	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("agent down")
		},
	}

	tn := NewTaskNode("planner", task, adapter)

	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("planner_plan", plan)

	// 软失败：不返回 error，输出为 nil
	outputs, err := tn.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("soft failure should not return error, got: %v", err)
	}
	if outputs["out"] != nil {
		t.Fatalf("output should be nil, got %v", outputs["out"])
	}

	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.Tasks[0].State != TaskFailed {
		t.Fatalf("state: want Failed, got %v", plan.Tasks[0].State)
	}
	if plan.Tasks[0].Error == "" {
		t.Fatal("error message should be recorded in plan")
	}
}

func TestTaskNode_BadJSON_SoftFailure(t *testing.T) {
	task := Task{ID: "task_1", Description: "test", Inputs: []string{}, Outputs: []string{"out"}}
	plan := NewPlan("goal")
	plan.Tasks = []Task{task}
	plan.mu.Lock()
	plan.Tasks[0].State = TaskPending
	plan.mu.Unlock()

	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return "this is definitely not json", nil
		},
	}

	tn := NewTaskNode("planner", task, adapter)

	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("planner_plan", plan)

	outputs, err := tn.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("soft failure should not return error, got: %v", err)
	}
	if outputs["out"] != nil {
		t.Fatalf("output should be nil, got %v", outputs["out"])
	}

	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.Tasks[0].State != TaskFailed {
		t.Fatalf("state: want Failed, got %v", plan.Tasks[0].State)
	}
}

func TestTaskNode_StateTransitions(t *testing.T) {
	task := Task{ID: "task_1", Description: "test", Inputs: []string{}, Outputs: []string{"out"}}
	plan := NewPlan("goal")
	plan.Tasks = []Task{task}
	plan.mu.Lock()
	plan.Tasks[0].State = TaskPending
	plan.mu.Unlock()

	// 记录 agent 被调用时的状态（应该是 Running）
	var stateWhenAgentCalled TaskState
	var mu sync.Mutex

	adapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			plan.mu.Lock()
			stateWhenAgentCalled = plan.Tasks[0].State
			plan.mu.Unlock()
			return taskJSON(map[string]any{"out": "ok"}), nil
		},
	}

	tn := NewTaskNode("planner", task, adapter)

	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("planner_plan", plan)

	_, err := tn.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// agent 被调用时应该是 Running 状态
	if stateWhenAgentCalled != TaskRunning {
		t.Fatalf("state when agent called: want Running, got %v", stateWhenAgentCalled)
	}

	// 最终应该是 Success 状态
	plan.mu.Lock()
	finalState := plan.Tasks[0].State
	plan.mu.Unlock()
	if finalState != TaskSuccess {
		t.Fatalf("final state: want Success, got %v", finalState)
	}
}

// ============================================================================
// BatchNewTaskNode
// ============================================================================

func TestBatchNewTaskNode(t *testing.T) {
	plan := NewPlan("goal")
	plan.Tasks = []Task{
		{ID: "task_1", Inputs: []string{}, Outputs: []string{"a"}},
		{ID: "task_2", Inputs: []string{"a"}, Outputs: []string{"b"}},
	}
	plan.mu.Lock()
	for i := range plan.Tasks {
		plan.Tasks[i].State = TaskPending
	}
	plan.mu.Unlock()

	adapter := &mockAgentAdapter{handler: func(_ context.Context, _ string) (string, error) { return "{}", nil }}
	nodes := BatchNewTaskNode("planner", plan, adapter)

	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
	if nodes[0].ID() != "task_1" || nodes[1].ID() != "task_2" {
		t.Fatalf("IDs: want [task_1, task_2], got [%s, %s]", nodes[0].ID(), nodes[1].ID())
	}

	// 每个节点都应依赖 planner_plan
	for _, n := range nodes {
		hasPlanInput := false
		for _, in := range n.Inputs() {
			if in == "planner_plan" {
				hasPlanInput = true
			}
		}
		if !hasPlanInput {
			t.Fatalf("node %s should depend on planner_plan, inputs: %v", n.ID(), n.Inputs())
		}
	}
}

// ============================================================================
// TopologicalNode（纯节点，不需要 agent）
// ============================================================================

func TestTopologicalNode_Chain(t *testing.T) {
	nodes := []Node{
		NewNode("a", nil, []string{"x"}, func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return map[string]any{"x": 10}, nil
		}),
		NewNode("b", []string{"x"}, []string{"y"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"y": in["x"].(int) * 2}, nil
		}),
		NewNode("c", []string{"y"}, []string{"z"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"z": in["y"].(int) + 1}, nil
		}),
	}

	tn, err := NewTopologicalNode("topo", nodes, []string{"z"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	order := tn.GetExecutionOrder()
	if order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("order: want [a,b,c], got %v", order)
	}
	if len(tn.GetLayers()) != 3 {
		t.Fatalf("layers: want 3, got %d", len(tn.GetLayers()))
	}

	ctx := flow.NewFlowContext(context.Background())
	outputs, err := tn.Run(ctx, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["z"] != 21 { // (10*2)+1
		t.Fatalf("want z=21, got %v", outputs["z"])
	}
}

func TestTopologicalNode_Diamond(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(id string) { mu.Lock(); order = append(order, id); mu.Unlock() }

	nodes := []Node{
		NewNode("a", nil, []string{"x"}, func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			record("a")
			return map[string]any{"x": 1}, nil
		}),
		NewNode("b", []string{"x"}, []string{"y"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			record("b")
			time.Sleep(50 * time.Millisecond)
			return map[string]any{"y": in["x"].(int) + 10}, nil
		}),
		NewNode("c", []string{"x"}, []string{"z"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			record("c")
			time.Sleep(50 * time.Millisecond)
			return map[string]any{"z": in["x"].(int) + 20}, nil
		}),
		NewNode("d", []string{"y", "z"}, []string{"result"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			record("d")
			return map[string]any{"result": in["y"].(int) + in["z"].(int)}, nil
		}),
	}

	tn, err := NewTopologicalNode("topo", nodes, []string{"result"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	layers := tn.GetLayers()
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d: %v", len(layers), layers)
	}
	if len(layers[1]) != 2 {
		t.Fatalf("layer 1 want 2 nodes [b,c], got %d: %v", len(layers[1]), layers[1])
	}

	ctx := flow.NewFlowContext(context.Background())
	outputs, err := tn.Run(ctx, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["result"] != 32 {
		t.Fatalf("want 32, got %v", outputs["result"])
	}

	mu.Lock()
	defer mu.Unlock()
	if order[0] != "a" || order[len(order)-1] != "d" {
		t.Fatalf("order: want a...d, got %v", order)
	}
}

func TestTopologicalNode_WithExternalInput(t *testing.T) {
	nodes := []Node{
		NewNode("a", []string{"seed"}, []string{"x"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"x": in["seed"].(int) * 3}, nil
		}),
		NewNode("b", []string{"x"}, []string{"y"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"y": in["x"].(int) + 1}, nil
		}),
	}

	tn, err := NewTopologicalNode("topo", nodes, []string{"y"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	externals := tn.Inputs()
	if len(externals) != 1 || externals[0] != "seed" {
		t.Fatalf("external inputs: want [seed], got %v", externals)
	}

	ctx := flow.NewFlowContext(context.Background())
	outputs, err := tn.Run(ctx, map[string]any{"seed": 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["y"] != 16 { // 5*3+1
		t.Fatalf("want 16, got %v", outputs["y"])
	}
}

func TestTopologicalNode_CycleDetection(t *testing.T) {
	nodes := []Node{
		NewNode("a", []string{"y"}, []string{"x"}, nil),
		NewNode("b", []string{"x"}, []string{"y"}, nil),
	}

	_, err := NewTopologicalNode("topo", nodes, []string{"x"})
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got: %v", err)
	}
}

func TestTopologicalNode_DuplicateOutputs(t *testing.T) {
	nodes := []Node{
		NewNode("a", nil, []string{"x"}, nil),
		NewNode("b", nil, []string{"x"}, nil),
	}

	_, err := NewTopologicalNode("topo", nodes, []string{"x"})
	if err == nil {
		t.Fatal("expected duplicate output error")
	}
}

func TestTopologicalNode_ErrorPropagation(t *testing.T) {
	nodes := []Node{
		NewNode("a", nil, []string{"x"}, func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return nil, errors.New("a failed")
		}),
		NewNode("b", []string{"x"}, []string{"y"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"y": in["x"].(int) + 1}, nil
		}),
	}

	tn, err := NewTopologicalNode("topo", nodes, []string{"y"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := flow.NewFlowContext(context.Background())
	_, err = tn.Run(ctx, nil)
	if err == nil {
		t.Fatal("expected error from node a")
	}
}

func TestTopologicalNode_ComplexDAG_NoDeadlock(t *testing.T) {
	// 5 节点复杂 DAG，如果死锁 context 超时会触发
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nodes := []Node{
		NewNode("a", nil, []string{"x"}, func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return map[string]any{"x": 1}, nil
		}),
		NewNode("b", nil, []string{"y"}, func(_ *flow.FlowContext, _ map[string]any) (map[string]any, error) {
			return map[string]any{"y": 2}, nil
		}),
		NewNode("c", []string{"x", "y"}, []string{"z"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"z": in["x"].(int) + in["y"].(int)}, nil
		}),
		NewNode("d", []string{"x"}, []string{"w"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"w": in["x"].(int) * 10}, nil
		}),
		NewNode("e", []string{"z", "w"}, []string{"result"}, func(_ *flow.FlowContext, in map[string]any) (map[string]any, error) {
			return map[string]any{"result": in["z"].(int) + in["w"].(int)}, nil
		}),
	}

	tn, err := NewTopologicalNode("topo", nodes, []string{"result"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fc := flow.NewFlowContext(ctx)
	outputs, err := tn.Run(fc, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// x=1, y=2, z=3, w=10, result=13
	if outputs["result"] != 13 {
		t.Fatalf("want 13, got %v", outputs["result"])
	}
}

// ============================================================================
// 集成测试：Planner + TaskNode + TopologicalNode
// ============================================================================

func TestIntegration_PlannerToTopological_Chain(t *testing.T) {
	// 完整链路：Planner 出计划 → TaskNodes 从计划创建 → TopologicalNode 编排执行
	planTasks := []map[string]any{
		{"id": "task_1", "description": "Get path", "inputs": []any{}, "outputs": []any{"path"}},
		{"id": "task_2", "description": "List files", "inputs": []any{"path"}, "outputs": []any{"files"}},
		{"id": "task_3", "description": "Count", "inputs": []any{"files"}, "outputs": []any{"count"}},
	}

	planAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return planJSON(planTasks), nil
		},
	}

	taskAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, prompt string) (string, error) {
			switch {
			case strings.Contains(prompt, "任务ID: task_1"):
				return taskJSON(map[string]any{"path": "/tmp"}), nil
			case strings.Contains(prompt, "任务ID: task_2"):
				return taskJSON(map[string]any{"files": "a.txt,b.txt,c.txt"}), nil
			case strings.Contains(prompt, "任务ID: task_3"):
				return taskJSON(map[string]any{"count": 3}), nil
			default:
				return "", fmt.Errorf("unexpected prompt")
			}
		},
	}

	// Phase 1: Planner
	pn := NewPlannerNode("planner", planAdapter)
	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("user_goal", "count files")

	planOut, err := pn.Run(ctx, map[string]any{"user_goal": "count files"})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	plan := planOut["planner_plan"].(*Plan)
	t.Logf("Plan: %d tasks created", len(plan.Tasks))

	// Phase 2: TaskNodes → TopologicalNode
	taskNodes := BatchNewTaskNode("planner", plan, taskAdapter)
	topo, err := NewTopologicalNode("exec", toNodes(taskNodes), []string{"count"})
	if err != nil {
		t.Fatalf("TopologicalNode: %v", err)
	}

	layers := topo.GetLayers()
	t.Logf("Layers: %v, Order: %v", layers, topo.GetExecutionOrder())
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d", len(layers))
	}

	// Phase 3: Run
	ctx.SetOrUpdate("planner_plan", plan)
	outputs, err := topo.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["count"] != float64(3) {
		t.Fatalf("want count=3, got %v", outputs["count"])
	}

	// Verify all tasks succeeded
	plan.mu.Lock()
	defer plan.mu.Unlock()
	for _, task := range plan.Tasks {
		if task.State != TaskSuccess {
			t.Errorf("task %s: want Success, got %v", task.ID, task.State)
		}
		t.Logf("task %s: state=%v, result=%v", task.ID, task.State, task.Result)
	}
}

func TestIntegration_PlannerToTopological_Diamond(t *testing.T) {
	// 菱形依赖：task_1(→x) → task_2(x→y), task_3(x→z) → task_4(y,z→result)
	planTasks := []map[string]any{
		{"id": "task_1", "description": "Gen x", "inputs": []any{}, "outputs": []any{"x"}},
		{"id": "task_2", "description": "x→y", "inputs": []any{"x"}, "outputs": []any{"y"}},
		{"id": "task_3", "description": "x→z", "inputs": []any{"x"}, "outputs": []any{"z"}},
		{"id": "task_4", "description": "y+z→result", "inputs": []any{"y", "z"}, "outputs": []any{"result"}},
	}

	planAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return planJSON(planTasks), nil
		},
	}

	taskAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, prompt string) (string, error) {
			switch {
			case strings.Contains(prompt, "任务ID: task_1"):
				return taskJSON(map[string]any{"x": 10}), nil
			case strings.Contains(prompt, "任务ID: task_2"):
				return taskJSON(map[string]any{"y": 20}), nil
			case strings.Contains(prompt, "任务ID: task_3"):
				return taskJSON(map[string]any{"z": 30}), nil
			case strings.Contains(prompt, "任务ID: task_4"):
				return taskJSON(map[string]any{"result": 50}), nil
			default:
				return "", fmt.Errorf("unexpected prompt")
			}
		},
	}

	pn := NewPlannerNode("planner", planAdapter)
	ctx := flow.NewFlowContext(context.Background())
	ctx.Set("user_goal", "diamond test")

	planOut, err := pn.Run(ctx, map[string]any{"user_goal": "diamond test"})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	plan := planOut["planner_plan"].(*Plan)

	taskNodes := BatchNewTaskNode("planner", plan, taskAdapter)
	topo, err := NewTopologicalNode("exec", toNodes(taskNodes), []string{"result"})
	if err != nil {
		t.Fatalf("TopologicalNode: %v", err)
	}

	layers := topo.GetLayers()
	t.Logf("Layers: %v", layers)

	// 验证菱形结构：3 层，中间层有 2 个并行节点
	if len(layers) != 3 {
		t.Fatalf("want 3 layers, got %d", len(layers))
	}
	if len(layers[1]) != 2 {
		t.Fatalf("layer 1 want 2 parallel nodes, got %d: %v", len(layers[1]), layers[1])
	}

	ctx.SetOrUpdate("planner_plan", plan)
	outputs, err := topo.Run(ctx, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outputs["result"] != float64(50) {
		t.Fatalf("want 50, got %v", outputs["result"])
	}

	plan.mu.Lock()
	defer plan.mu.Unlock()
	for _, task := range plan.Tasks {
		if task.State != TaskSuccess {
			t.Errorf("task %s: want Success, got %v", task.ID, task.State)
		}
	}
}

func TestIntegration_NoDeadlock(t *testing.T) {
	// 关键测试：完整流水线不死锁
	// context 超时作为死锁检测器
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	planTasks := []map[string]any{
		{"id": "task_1", "description": "Step 1", "inputs": []any{}, "outputs": []any{"a"}},
		{"id": "task_2", "description": "Step 2", "inputs": []any{"a"}, "outputs": []any{"b"}},
		{"id": "task_3", "description": "Step 3", "inputs": []any{"a", "b"}, "outputs": []any{"c"}},
	}

	planAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return planJSON(planTasks), nil
		},
	}

	taskAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, prompt string) (string, error) {
			switch {
			case strings.Contains(prompt, "任务ID: task_1"):
				return taskJSON(map[string]any{"a": "val_a"}), nil
			case strings.Contains(prompt, "任务ID: task_2"):
				return taskJSON(map[string]any{"b": "val_b"}), nil
			case strings.Contains(prompt, "任务ID: task_3"):
				return taskJSON(map[string]any{"c": "val_c"}), nil
			default:
				return "", fmt.Errorf("unexpected prompt")
			}
		},
	}

	// Plan
	pn := NewPlannerNode("planner", planAdapter)
	fc := flow.NewFlowContext(ctx)
	fc.Set("user_goal", "test")

	planOut, err := pn.Run(fc, map[string]any{"user_goal": "test"})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	plan := planOut["planner_plan"].(*Plan)
	t.Logf("Plan: %d tasks", len(plan.Tasks))

	// TaskNodes → TopologicalNode
	taskNodes := BatchNewTaskNode("planner", plan, taskAdapter)
	topo, err := NewTopologicalNode("exec", toNodes(taskNodes), []string{"c"})
	if err != nil {
		t.Fatalf("TopologicalNode: %v", err)
	}

	t.Logf("Layers: %v", topo.GetLayers())
	t.Logf("External inputs: %v", topo.Inputs())
	t.Logf("Execution order: %v", topo.GetExecutionOrder())

	// Run — 死锁的话 context 超时会触发
	fc.SetOrUpdate("planner_plan", plan)
	outputs, err := topo.Run(fc, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if outputs["c"] != "val_c" {
		t.Fatalf("want val_c, got %v", outputs["c"])
	}

	plan.mu.Lock()
	defer plan.mu.Unlock()
	for _, task := range plan.Tasks {
		t.Logf("Task %s: state=%v result=%v", task.ID, task.State, task.Result)
		if task.State != TaskSuccess {
			t.Errorf("task %s: want Success, got %v", task.ID, task.State)
		}
	}
}

func TestIntegration_TaskFailure_DoesNotDeadlock(t *testing.T) {
	// task_2 软失败 → 输出 nil → task_3 仍能读到 a → 不死锁
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	planTasks := []map[string]any{
		{"id": "task_1", "description": "OK", "inputs": []any{}, "outputs": []any{"a"}},
		{"id": "task_2", "description": "Will fail", "inputs": []any{"a"}, "outputs": []any{"b"}},
		{"id": "task_3", "description": "Only needs a", "inputs": []any{"a"}, "outputs": []any{"c"}},
	}

	planAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, _ string) (string, error) {
			return planJSON(planTasks), nil
		},
	}

	taskAdapter := &mockAgentAdapter{
		handler: func(_ context.Context, prompt string) (string, error) {
			switch {
			case strings.Contains(prompt, "任务ID: task_1"):
				return taskJSON(map[string]any{"a": "ok"}), nil
			case strings.Contains(prompt, "任务ID: task_2"):
				return "", errors.New("task_2 failed")
			case strings.Contains(prompt, "任务ID: task_3"):
				return taskJSON(map[string]any{"c": "ok"}), nil
			default:
				return "", fmt.Errorf("unexpected")
			}
		},
	}

	pn := NewPlannerNode("planner", planAdapter)
	fc := flow.NewFlowContext(ctx)
	fc.Set("user_goal", "test")

	planOut, err := pn.Run(fc, map[string]any{"user_goal": "test"})
	if err != nil {
		t.Fatalf("Planner: %v", err)
	}
	plan := planOut["planner_plan"].(*Plan)

	taskNodes := BatchNewTaskNode("planner", plan, taskAdapter)
	topo, err := NewTopologicalNode("exec", toNodes(taskNodes), []string{"b", "c"})
	if err != nil {
		t.Fatalf("TopologicalNode: %v", err)
	}

	fc.SetOrUpdate("planner_plan", plan)
	outputs, err := topo.Run(fc, map[string]any{"planner_plan": plan})
	if err != nil {
		t.Fatalf("Run: %v", err) // 不应死锁，不应报错
	}

	// task_2 软失败 → b 为 nil
	if outputs["b"] != nil {
		t.Fatalf("want b=nil (failed), got %v", outputs["b"])
	}
	// task_3 成功
	if outputs["c"] != "ok" {
		t.Fatalf("want c=ok, got %v", outputs["c"])
	}

	// 验证 Plan 状态
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.Tasks[0].State != TaskSuccess {
		t.Errorf("task_1: want Success, got %v", plan.Tasks[0].State)
	}
	if plan.Tasks[1].State != TaskFailed {
		t.Errorf("task_2: want Failed, got %v", plan.Tasks[1].State)
	}
	if plan.Tasks[2].State != TaskSuccess {
		t.Errorf("task_3: want Success, got %v", plan.Tasks[2].State)
	}
}
