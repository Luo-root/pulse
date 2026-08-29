package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PreviewFn 在执行前只读计算权限卡片。nil 表示该工具不提供预览。
// 不得写入磁盘；失败时 HITL 应视为空预览并仍按 Risk 问人。
type PreviewFn func(ctx context.Context, args json.RawMessage) (Preview, error)

const (
	// KindFile 文件创建/修改/删除。
	KindFile = "file"
	// KindCommand 将执行的命令。
	KindCommand = "command"
	// KindNetwork 出网请求。
	KindNetwork = "network"
	// KindOpaque 未知/远端效果（MCP 默认）。
	KindOpaque = "opaque"

	// ActionRead 只读。
	ActionRead = "read"
	// ActionWrite 写文件或等价变更。
	ActionWrite = "write"
	// ActionExecute 跑命令。
	ActionExecute = "execute"
	// ActionNetwork 出网。
	ActionNetwork = "network"
)

// Preview 是执行前给人看的权限卡片（三层：身份 / 主体 / 效果）。
// Markdown 只是 Render 的一种输出，不是事实源。
type Preview struct {
	Tool    string // 模型可见名；LookupPreview 填写
	Source  string
	Risk    Risk
	Action  string // read|write|execute|network
	Subject string // 审批键：path / command / url / mcp.id+upstream
	Kind    string // file|command|network|opaque
	File    *FileChange
	Command *CommandChange
	Network *NetworkChange
	Opaque  *OpaqueChange
}

// FileChange 是 file kind 的效果。不含全文 OldText/NewText。
type FileChange struct {
	Op        string // create|modify|delete|rename
	Path      string
	OldPath   string // rename 时的旧路径
	Added     int
	Removed   int
	Diff      string // unified；超限截断
	Truncated bool
	Binary    bool
}

// CommandChange 是 command kind 的效果。
type CommandChange struct {
	Command string
	Cwd     string
	Timeout string
}

// NetworkChange 是 network kind 的效果。
type NetworkChange struct {
	Method    string
	URL       string
	HostClass string // public|private|metadata
}

// OpaqueChange 是 opaque kind 的效果。
type OpaqueChange struct {
	Summary     string
	ArgsExcerpt string
}

// ActionFromRisk 把 Risk 映到卡片 action；未知时保守为 execute。
func ActionFromRisk(r Risk) string {
	switch r {
	case RiskReadonly:
		return ActionRead
	case RiskReadWrite:
		return ActionWrite
	default:
		return ActionExecute
	}
}

// LookupPreview 返回已登记工具的 PreviewFn。没有或已撤销时 ok=false，不是错误。
func (r *Registry) LookupPreview(name string) (PreviewFn, bool) {
	if r == nil || name == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	if !ok || e.preview == nil {
		return nil, false
	}
	return e.preview, true
}

// Preview 调用 PreviewFn，并填上 Tool/Source/Risk。
// ok=false：未登记或没有 PreviewFn（空预览，HITL 仍应问人）。
// PreviewFn 返回 error：ok=true 但 err!=nil，HITL 不得因此放行。
func (r *Registry) Preview(ctx context.Context, name string, args json.RawMessage) (Preview, bool, error) {
	fn, ok := r.LookupPreview(name)
	if !ok {
		return Preview{}, false, nil
	}
	src, risk, _ := r.LookupMeta(name)
	p, err := fn(ctx, args)
	if err != nil {
		return Preview{}, true, err
	}
	p.Tool = name
	p.Source = src
	p.Risk = risk
	if p.Action == "" {
		p.Action = ActionFromRisk(risk)
	}
	return p, true, nil
}

// Render 把卡片打成给人看的几行文本（demo HITL / 日志）。不是 JSON 事实源。
func (p Preview) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "preview kind=%s action=%s risk=%s source=%s\n", p.Kind, p.Action, p.Risk, p.Source)
	if p.Subject != "" {
		fmt.Fprintf(&b, "subject: %s\n", p.Subject)
	}
	switch p.Kind {
	case KindFile:
		if p.File == nil {
			break
		}
		fmt.Fprintf(&b, "file %s %s  +%d/-%d", p.File.Op, p.File.Path, p.File.Added, p.File.Removed)
		if p.File.Binary {
			b.WriteString("  binary")
		}
		if p.File.Truncated {
			b.WriteString("  truncated")
		}
		b.WriteByte('\n')
		if p.File.Diff != "" {
			b.WriteString(p.File.Diff)
			if !strings.HasSuffix(p.File.Diff, "\n") {
				b.WriteByte('\n')
			}
		}
	case KindCommand:
		if p.Command != nil {
			fmt.Fprintf(&b, "command: %s\n", p.Command.Command)
			if p.Command.Cwd != "" {
				fmt.Fprintf(&b, "cwd: %s\n", p.Command.Cwd)
			}
			if p.Command.Timeout != "" {
				fmt.Fprintf(&b, "timeout: %s\n", p.Command.Timeout)
			}
		}
	case KindNetwork:
		if p.Network != nil {
			fmt.Fprintf(&b, "network: %s %s (%s)\n", p.Network.Method, p.Network.URL, p.Network.HostClass)
		}
	case KindOpaque:
		if p.Opaque != nil {
			if p.Opaque.Summary != "" {
				fmt.Fprintf(&b, "%s\n", p.Opaque.Summary)
			}
			if p.Opaque.ArgsExcerpt != "" {
				fmt.Fprintf(&b, "args: %s\n", p.Opaque.ArgsExcerpt)
			}
		}
	}
	return b.String()
}
