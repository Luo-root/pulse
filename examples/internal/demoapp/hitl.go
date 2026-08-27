package demoapp

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/loop"
)

// HITLMode 决定 before_tool_call 的裁决策略。
type HITLMode string

const (
	// HITLDenylist 默认放行，仅拒绝 DenyTool 名单（原静态演示行为）。
	HITLDenylist HITLMode = "denylist"
	// HITLInteractive 每次危险调用在终端询问操作者：y 放行一次 / n 拒绝 / a 本会话永久放行该工具。
	HITLInteractive HITLMode = "interactive"
	// HITLAllowlist 默认拒绝，仅 AllowTool 白名单中的工具放行（空则 lookup）。
	HITLAllowlist HITLMode = "allowlist"
	// HITLOff 不安装任何审批监听，全部放行（展示零监听时的基线行为）。
	HITLOff HITLMode = "off"
)

// ParseHITLMode 解析 PULSE_DEMO_HITL，空值归一为 denylist。
func ParseHITLMode(v string) (HITLMode, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return HITLDenylist, nil
	case string(HITLDenylist):
		return HITLDenylist, nil
	case string(HITLInteractive):
		return HITLInteractive, nil
	case string(HITLAllowlist):
		return HITLAllowlist, nil
	case string(HITLOff):
		return HITLOff, nil
	default:
		return "", fmt.Errorf("demo: 未知 PULSE_DEMO_HITL=%q（可选 denylist|interactive|allowlist|off）", v)
	}
}

// InstallHITL 把指定模式的审批策略挂到 scope 上，返回会话信任表（非 interactive 为 nil）。
//
// 参数来源约定：denylist 读 DenyTool（PULSE_DEMO_DENY_TOOL）；allowlist 只读
// AllowTool（PULSE_DEMO_ALLOW_TOOL）——两个变量语义相反，禁止互相复用。
//
// 并发边界：interactive 依赖「同一 goroutine 内、loop 回合工具串行执行」这一事实，
// 与 REPL 共享同一个 LineSource（单一行缓冲，顺序消费），不会互相抢行；
// 多 Agent 并发审批需要服务化通道，不属于本 demo。
func InstallHITL(scope *kernel.Context, mode HITLMode, denyTool, allowTool, hint string, in io.Reader, out io.Writer) (*SessionTrust, error) {
	switch mode {
	case HITLOff:
		return nil, nil
	case HITLDenylist:
		deny := denyTool
		_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
			func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				if deny != "" && btc.Call.Name == deny {
					btc.Rejected = true
					btc.RejectReason = "denied by demo HITL policy (" + deny + ")"
					return btc
				}
				return next(btc)
			})
		return nil, err
	case HITLAllowlist:
		allow := parseNameList(allowTool)
		if len(allow) == 0 {
			allow["lookup"] = true // 默认只放行只读工具
		}
		_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
			func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				if !allow[btc.Call.Name] {
					btc.Rejected = true
					btc.RejectReason = "default-deny: not in allowlist"
					return btc
				}
				return next(btc)
			})
		return nil, err
	case HITLInteractive:
		trust := newSessionTrust()
		listener := newConsoleApprover(hint, NewLineSource(in), out, trust)
		_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall, listener.approve)
		return trust, err
	default:
		return nil, fmt.Errorf("demo: unsupported hitl mode %q", mode)
	}
}

// parseNameList 把逗号/空白分隔的工具名列表转成集合。
func parseNameList(v string) map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Fields(strings.ReplaceAll(v, ",", " ")) {
		if name != "" {
			out[name] = true
		}
	}
	return out
}

// SessionTrust 记录 interactive 模式下操作者用 a 授予的会话级白名单。
type SessionTrust struct {
	mu     sync.RWMutex
	always map[string]bool
}

func newSessionTrust() *SessionTrust {
	return &SessionTrust{always: make(map[string]bool)}
}

// Trusted 报告工具是否已获得会话级授权。
func (t *SessionTrust) Trusted(name string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.always[name]
}

// Names 返回已获会话授权的工具名列表（无序）。
func (t *SessionTrust) Names() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.always))
	for name := range t.always {
		out = append(out, name)
	}
	return out
}

// Grant 显式授予会话级信任（测试与预置白名单用）。
func (t *SessionTrust) Grant(name string) { t.always[name] = true }

const approveUsage = "[y]es / [n]o / [a]lways"

// consoleApprover 是阻塞式终端审批器。它和 REPL 必须共享同一个 LineSource，
// 保证两个组件对 stdin 的读取串行且共用一份行缓冲。
type consoleApprover struct {
	hint  string
	lines *LineSource
	out   io.Writer
	trust *SessionTrust
}

func newConsoleApprover(hint string, lines *LineSource, out io.Writer, trust *SessionTrust) *consoleApprover {
	return &consoleApprover{hint: hint, lines: lines, out: out, trust: trust}
}

// approve 实现 before_tool_call 的 around 契约：
//   - 会话已信任的工具直接放行；
//   - 其余阻塞等待操作者 y/n/a；
//   - 输入流关闭或不可读时按安全侧失败处理（拒绝而非放行）。
func (a *consoleApprover) approve(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
	if a.trust.Trusted(btc.Call.Name) {
		fmt.Fprintf(a.out, "✔ %s 已获得本会话授权，自动放行\n", btc.Call.Name)
		return next(btc)
	}
	fmt.Fprintf(a.out, "\n⚠ 审批请求 tool=%s args=%s\n  说明: %s\n  批准? %s > ",
		btc.Call.Name, string(btc.Call.Arguments), a.hint, approveUsage)
	line, err := a.lines.ReadLine()
	ans := strings.ToLower(strings.TrimSpace(line))
	if err != nil && ans == "" {
		btc.Rejected = true
		btc.RejectReason = "approval input unavailable; fail-closed"
		fmt.Fprintln(a.out, "✘ 输入不可读，按 fail-closed 拒绝")
		return btc
	}
	switch ans {
	case "y", "yes":
		fmt.Fprintf(a.out, "→ 批准本次\n")
		return next(btc)
	case "a", "always":
		a.trust.Grant(btc.Call.Name)
		fmt.Fprintf(a.out, "→ 批准并记住 %s（本会话内不再询问）\n", btc.Call.Name)
		return next(btc)
	default:
		btc.Rejected = true
		btc.RejectReason = "denied by operator"
		fmt.Fprintf(a.out, "→ 拒绝\n")
		return btc
	}
}
