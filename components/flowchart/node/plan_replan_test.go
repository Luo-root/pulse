package node

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/agent"
	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================
// RePlan 测试
// ============================================================

func TestRePlan_WithValidJSON(t *testing.T) {
	mockModel := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse(`{"tasks":[
			{"id":"task_1","description":"fixed task","inputs":[],"outputs":["result"]}
		]}`),
	)

	reg := tools.NewToolRegistry()
	testAgent := agent.NewAgent(mockModel, reg)

	originalPlan := NewPlan("test goal")
	originalPlan.Tasks = []Task{
		{ID: "task_1", Description: "original", State: TaskFailed, Error: "something failed"},
	}

	failedTask := &originalPlan.Tasks[0]
	newPlan, err := RePlan(context.Background(), originalPlan, failedTask, testAgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if newPlan.Goal != "test goal" {
		t.Fatalf("expected goal 'test goal', got %q", newPlan.Goal)
	}

	found := false
	for _, task := range newPlan.Tasks {
		if task.ID == "task_1" {
			found = true
			if task.State != TaskPending {
				t.Fatalf("expected task_1 to be pending, got %v", task.State)
			}
			if task.Error != "" {
				t.Fatalf("expected empty error, got %q", task.Error)
			}
		}
	}
	if !found {
		t.Fatal("expected task_1 in new plan")
	}
}

func TestRePlan_WithInvalidJSON_FallbackReset(t *testing.T) {
	mockModel := chatmodel.NewMockModelWithResponses(
		chatmodel.MockTextResponse("not valid json at all"),
	)

	reg := tools.NewToolRegistry()
	testAgent := agent.NewAgent(mockModel, reg)

	originalPlan := NewPlan("test goal")
	originalPlan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess, Result: map[string]any{"out": "val"}},
		{ID: "task_2", State: TaskFailed, Error: "bad stuff"},
	}

	newPlan, err := RePlan(context.Background(), originalPlan, &originalPlan.Tasks[1], testAgent)
	if err != nil {
		t.Logf("RePlan returned error (extractPlan fallback): %v", err)
	}

	if newPlan == nil {
		t.Fatal("expected non-nil plan from fallback")
	}

	for _, task := range newPlan.Tasks {
		if task.ID == "task_1" {
			if task.State != TaskSuccess {
				t.Fatalf("task_1 should remain success, got %v", task.State)
			}
		}
		if task.ID == "task_2" {
			if task.State != TaskPending {
				t.Fatalf("task_2 should be reset to pending, got %v", task.State)
			}
		}
	}
}

// ============================================================
// Plan JSON 序列化测试
// ============================================================

func TestPlanJSON_MarshalUnmarshal(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{
			ID:          "task_1",
			Description: "do something",
			Inputs:      []string{},
			Outputs:     []string{"result"},
			State:       TaskPending,
		},
		{
			ID:          "task_2",
			Description: "do something else",
			Inputs:      []string{"result"},
			Outputs:     []string{"final"},
			State:       TaskPending,
		},
	}

	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded Plan
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Goal != plan.Goal {
		t.Fatalf("goal mismatch: %q vs %q", decoded.Goal, plan.Goal)
	}
	if len(decoded.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(decoded.Tasks))
	}
	if decoded.Tasks[0].ID != "task_1" {
		t.Fatalf("expected task_1, got %s", decoded.Tasks[0].ID)
	}
}

func TestPlanJSON_UnmarshalFromPlanningResponse(t *testing.T) {
	// 模拟 LLM 返回的 planning JSON
	responseJSON := `{
		"tasks": [
			{
				"id": "task_1",
				"description": "Get current working directory. outputs: {\"current_path\": \"path\"}",
				"inputs": [],
				"outputs": ["current_path"]
			},
			{
				"id": "task_2",
				"description": "List files. inputs: {\"current_path\": \"path\"}, outputs: {\"file_list\": \"list\"}",
				"inputs": ["current_path"],
				"outputs": ["file_list"]
			}
		]
	}`

	plan := NewPlan("test")
	err := json.Unmarshal([]byte(responseJSON), plan)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(plan.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(plan.Tasks))
	}

	// 验证依赖关系
	if len(plan.Tasks[0].Inputs) != 0 {
		t.Fatal("task_1 should have no inputs")
	}
	if len(plan.Tasks[1].Inputs) != 1 || plan.Tasks[1].Inputs[0] != "current_path" {
		t.Fatalf("task_2 should depend on current_path, got %v", plan.Tasks[1].Inputs)
	}
}

// ============================================================
// Plan 并发安全测试
// ============================================================

func TestPlan_ConcurrentStateModification(t *testing.T) {
	plan := NewPlan("concurrent test")
	for i := 0; i < 10; i++ {
		plan.Tasks = append(plan.Tasks, Task{
			ID:    fmt.Sprintf("task_%d", i+1),
			State: TaskPending,
		})
	}

	var wg sync.WaitGroup

	// 模拟多个 task node 并发修改状态
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task_%d", idx+1)

			taskStateModifyRunning(plan, taskID)
			time.Sleep(time.Duration(idx*5) * time.Millisecond)
			taskStateModifySuccess(plan, taskID)
		}(i)
	}

	wg.Wait()

	// 验证所有任务都成功完成
	for _, task := range plan.Tasks {
		if task.State != TaskSuccess {
			t.Fatalf("task %s should be success, got %v", task.ID, task.State)
		}
	}
}

func TestPlan_ConcurrentSnapshotAndModify(t *testing.T) {
	plan := NewPlan("snapshot test")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskPending},
	}

	var wg sync.WaitGroup
	const iterations = 100

	// 并发快照和修改
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			snap := plan.Snapshot()
			_ = snap
		}()
		go func(state TaskState) {
			defer wg.Done()
			plan.mu.Lock()
			plan.Tasks[0].State = state
			plan.notifyStateChanged()
			plan.mu.Unlock()
		}(TaskState(fmt.Sprintf("state_%d", i)))
	}
	wg.Wait()
}
