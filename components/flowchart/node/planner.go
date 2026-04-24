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
type Plan struct {
	Goal  string `json:"goal"`
	Tasks []Task `json:"tasks"`
	mu    *sync.Mutex
}

func NewPlan() *Plan {
	return &Plan{
		mu: &sync.Mutex{},
	}
}
