package node

import (
	"testing"
	"time"
)

// TestPlanStateNotification 测试计划状态变更通知
func TestPlanStateNotification(t *testing.T) {
	plan := NewPlan("test goal")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskPending},
		{ID: "task_2", State: TaskPending},
	}

	// 获取状态变更 channel
	ch := plan.GetStateChannel()

	// 在 goroutine 中等待状态变更
	notified := make(chan bool, 1)
	go func() {
		select {
		case <-ch:
			notified <- true
		case <-time.After(500 * time.Millisecond):
			notified <- false
		}
	}()

	// 稍微等待确保 goroutine 已启动
	time.Sleep(10 * time.Millisecond)

	// 修改任务状态
	plan.mu.Lock()
	plan.Tasks[0].State = TaskRunning
	plan.mu.Unlock()
	plan.notifyStateChanged()

	// 验证收到通知
	if !<-notified {
		t.Fatal("expected state change notification, got timeout")
	}

	t.Logf("✅ Plan state notification test passed")
}

// TestPlanFindFailedTask 测试查找失败任务
func TestPlanFindFailedTask(t *testing.T) {
	plan := NewPlan("")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
		{ID: "task_2", State: TaskFailed, Error: "something wrong"},
		{ID: "task_3", State: TaskPending},
	}

	failed := plan.FindFailedTask()
	if failed == nil {
		t.Fatal("expected to find failed task")
	}
	if failed.ID != "task_2" {
		t.Fatalf("expected task_2, got %s", failed.ID)
	}

	t.Logf("✅ FindFailedTask test passed: %s", failed.ID)
}

// TestPlanIsAllCompleted 测试全部完成检查
func TestPlanIsAllCompleted(t *testing.T) {
	plan := NewPlan("")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
		{ID: "task_2", State: TaskSuccess},
	}

	if !plan.IsAllCompleted() {
		t.Fatal("expected all completed")
	}

	plan.Tasks[1].State = TaskFailed
	if plan.IsAllCompleted() {
		t.Fatal("expected not all completed")
	}

	t.Logf("✅ IsAllCompleted test passed")
}

// TestPlanIsAnyFailed 测试失败检查
func TestPlanIsAnyFailed(t *testing.T) {
	plan := NewPlan("")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskSuccess},
		{ID: "task_2", State: TaskPending},
	}

	if plan.IsAnyFailed() {
		t.Fatal("expected no failed task")
	}

	plan.Tasks[1].State = TaskFailed
	if !plan.IsAnyFailed() {
		t.Fatal("expected failed task")
	}

	t.Logf("✅ IsAnyFailed test passed")
}

// TestPlanSnapshot 测试快照功能
func TestPlanSnapshot(t *testing.T) {
	plan := NewPlan("")
	plan.Goal = "test"
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskRunning},
	}

	snapshot := plan.Snapshot()

	// 修改原 plan
	plan.mu.Lock()
	plan.Tasks[0].State = TaskFailed
	plan.mu.Unlock()

	// 快照应该不受影响
	if snapshot.Tasks[0].State != TaskRunning {
		t.Fatalf("expected snapshot unchanged, got %s", snapshot.Tasks[0].State)
	}

	t.Logf("✅ Snapshot test passed")
}

// TestPlanWaitForStateChange 测试阻塞等待状态变更
func TestPlanWaitForStateChange(t *testing.T) {
	plan := NewPlan("")
	plan.Tasks = []Task{
		{ID: "task_1", State: TaskPending},
	}

	// 在 goroutine 中等待状态变更
	done := make(chan bool, 1)
	go func() {
		plan.WaitForStateChange()
		done <- true
	}()

	// 稍微等待确保 goroutine 已启动
	time.Sleep(10 * time.Millisecond)

	// 修改状态并通知
	plan.mu.Lock()
	plan.Tasks[0].State = TaskRunning
	plan.mu.Unlock()
	plan.notifyStateChanged()

	select {
	case <-done:
		t.Logf("✅ WaitForStateChange test passed")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for state change")
	}
}
