package node

import (
	"sync"
)

// TaskState 任务状态
type TaskState string

const (
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskSuccess   TaskState = "success"
	TaskFailed    TaskState = "failed"
	TaskCancelled TaskState = "cancelled"
)

// Task 规划任务
type Task struct {
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Inputs      []string       `json:"inputs"`
	Outputs     []string       `json:"outputs"`
	State       TaskState      `json:"state"`
	Result      map[string]any `json:"result"`
	Error       string         `json:"error"`
}

// Plan 执行计划
// 新增状态变更通知机制：支持 channel 通知和条件变量等待，替代轮询
type Plan struct {
	Goal  string `json:"goal"`
	Tasks []Task `json:"tasks"`
	mu    *sync.Mutex
	// 状态变更通知 channel（缓冲1，非阻塞发送）
	// 任何任务状态变更时都会向此 channel 发送信号
	stateChanged chan struct{}
	// 条件变量，用于支持 WaitForStateChange 阻塞等待
	cond *sync.Cond
}

func NewPlan() *Plan {
	mu := &sync.Mutex{}
	return &Plan{
		mu:           mu,
		stateChanged: make(chan struct{}, 1),
		cond:         sync.NewCond(mu),
	}
}

// notifyStateChanged 通知状态变更（内部使用）
func (p *Plan) notifyStateChanged() {
	// 非阻塞发送通知
	select {
	case p.stateChanged <- struct{}{}:
	default:
	}
	// 同时广播条件变量
	p.cond.Broadcast()
}

// WaitForStateChange 阻塞等待状态变更（替代轮询）
// 返回当前计划的一个快照，调用方可检查任务状态
func (p *Plan) WaitForStateChange() {
	p.mu.Lock()
	p.cond.Wait()
	p.mu.Unlock()
}

// GetStateChannel 获取状态变更通知 channel
// 调用方可通过 select 监听此 channel，实现非轮询的状态监控
func (p *Plan) GetStateChannel() <-chan struct{} {
	return p.stateChanged
}

// Snapshot 获取计划当前状态的快照（线程安全）
func (p *Plan) Snapshot() Plan {
	p.mu.Lock()
	defer p.mu.Unlock()

	tasksCopy := make([]Task, len(p.Tasks))
	copy(tasksCopy, p.Tasks)

	return Plan{
		Goal:  p.Goal,
		Tasks: tasksCopy,
		// 不复制内部同步原语
	}
}

// FindFailedTask 查找第一个失败的任务（线程安全）
func (p *Plan) FindFailedTask() *Task {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := range p.Tasks {
		if p.Tasks[i].State == TaskFailed {
			return &p.Tasks[i]
		}
	}
	return nil
}

// IsAllCompleted 检查是否全部完成（线程安全）
func (p *Plan) IsAllCompleted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, task := range p.Tasks {
		if task.State != TaskSuccess {
			return false
		}
	}
	return true
}

// IsAnyFailed 检查是否有失败任务（线程安全）
func (p *Plan) IsAnyFailed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, task := range p.Tasks {
		if task.State == TaskFailed {
			return true
		}
	}
	return false
}
