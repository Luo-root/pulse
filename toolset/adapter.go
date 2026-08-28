package toolset

import (
	"context"
	"fmt"
	"sort"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// registryToolSet 把 Registry 适配为 loop.ToolSet（live 视图）。
type registryToolSet struct {
	r *Registry
}

func (s *registryToolSet) Definitions() []llm.ToolDef {
	if s == nil || s.r == nil {
		return nil
	}
	s.r.mu.RLock()
	defer s.r.mu.RUnlock()
	out := make([]llm.ToolDef, 0, len(s.r.tools))
	for _, e := range s.r.tools {
		out = append(out, e.def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *registryToolSet) Execute(ctx context.Context, call llm.ToolCall) (string, error) {
	if s == nil || s.r == nil {
		return "", fmt.Errorf("toolset: nil registry")
	}
	s.r.mu.RLock()
	e, ok := s.r.tools[call.Name]
	s.r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	return e.fn(ctx, call.Arguments)
}

var _ loop.ToolSet = (*registryToolSet)(nil)
