package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Luo-root/pulse/llm"
)

// ToolSet 是 loop 对工具集合的 provider 中立抽象（与 llm.ChatModel
// 同构：消费方只见接口，实现可替换）。
//
// 契约：
//   - Definitions 的返回顺序稳定（同一次 Run 内多次调用结果一致），
//     供组装 GenerateRequest.Tools；
//   - Execute 返回非 nil error 视为工具执行失败——loop 会把错误文本
//     作为 IsError 结果回传给模型，模型可据此自我修正，回合不中断；
//   - Execute 必须尊重 ctx 取消；panic 由 loop 恢复为失败结果，
//     单个工具崩溃不拖死整个回合。
type ToolSet interface {
	Definitions() []llm.ToolDef
	Execute(ctx context.Context, call llm.ToolCall) (string, error)
}

// ToolFunc 是单个工具的执行函数。args 是模型给出的参数 JSON，
// 由工具自行解析与校验。
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// MemToolSet 是内存工具注册表：最简单的 ToolSet 实现，覆盖测试
// 与小型装配场景。并发安全。
type MemToolSet struct {
	mu    sync.RWMutex
	tools map[string]*memTool
}

type memTool struct {
	def llm.ToolDef
	fn  ToolFunc
}

// NewMemToolSet 创建空注册表。
func NewMemToolSet() *MemToolSet {
	return &MemToolSet{tools: make(map[string]*memTool)}
}

// Register 登记一个工具。同名重复登记返回错误——装配期冲突应当
// 尽早暴露，而不是静默覆盖。
func (s *MemToolSet) Register(def llm.ToolDef, fn ToolFunc) error {
	if def.Name == "" {
		return fmt.Errorf("loop: tool name is required")
	}
	if fn == nil {
		return fmt.Errorf("loop: tool %q: nil handler", def.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.tools[def.Name]; dup {
		return fmt.Errorf("loop: tool %q already registered", def.Name)
	}
	s.tools[def.Name] = &memTool{def: def, fn: fn}
	return nil
}

// Definitions 实现 ToolSet：按工具名排序返回，保证输出稳定。
func (s *MemToolSet) Definitions() []llm.ToolDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]llm.ToolDef, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Execute 实现 ToolSet：未知工具名返回错误；工具 panic 被恢复为
// 错误（双保险——loop 侧也有一层恢复）。
func (s *MemToolSet) Execute(ctx context.Context, call llm.ToolCall) (out string, err error) {
	s.mu.RLock()
	t, ok := s.tools[call.Name]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	defer func() {
		if r := recover(); r != nil {
			out = ""
			err = fmt.Errorf("tool %q panicked: %v", call.Name, r)
		}
	}()
	return t.fn(ctx, call.Arguments)
}

var _ ToolSet = (*MemToolSet)(nil)
