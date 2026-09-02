// 03-hitl：人机协同审批——**手写一个 HITL**。
//
// 运行：go run ./examples/03-hitl
// before_tool_call 是 loop 暴露在工具执行前的裁决点（WaterfallLocal 派发，
// 监听必须与 Agent 同 scope）。本课把 demoapp.InstallHITLWithTrust 的实现
// 完整展开：denylist / allowlist / interactive 三策略 + 会话信任表 +
// fail-closed——写一遍你就知道生产审批器的最小集长什么样。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/toolset"
)

// toolHint 展示在审批提示里的说明文字。
const toolHint = "模拟危险操作，不触达真实文件系统"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "03-hitl: %v\n", err)
		os.Exit(1)
	}
}

// ---- 会话信任表：interactive 模式下 `a` 键授予的会话级白名单 ----

// sessionTrust 记录本会话内已被永久放行的工具名。跨轮复用的关键：它由
// REPL 外层持有并逐轮传入 install——reqScope 销毁不影响 trust 对象。
type sessionTrust struct {
	mu     sync.RWMutex
	always map[string]bool
}

func newSessionTrust() *sessionTrust {
	return &sessionTrust{always: make(map[string]bool)}
}

// trusted 报告工具是否已获会话级授权。**nil 安全**：02 课那种不装审批
// 的调用方可以直接传 nil 判断。
func (t *sessionTrust) trusted(name string) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.always[name]
}

func (t *sessionTrust) grant(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.always[name] = true
}

func (t *sessionTrust) names() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.always))
	for name := range t.always {
		out = append(out, name)
	}
	return out
}

// parseMode 解析 PULSE_DEMO_HITL（空值归一 denylist）。
func parseMode(v string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(v)); m {
	case "", "denylist":
		return "denylist", nil
	case "interactive", "allowlist", "off":
		return m, nil
	default:
		return "", fmt.Errorf("03-hitl: 未知 PULSE_DEMO_HITL=%q（可选 denylist|interactive|allowlist|off）", v)
	}
}

// installHITL 把指定模式的裁决策略挂到 scope 上，返回 trust 供跨轮复用。
//
// 参数约定：denylist 读 denyTool，allowlist 只读 allowTool——两个变量
// 语义相反（「拒绝谁」vs「只许谁」），禁止互相复用。
//
// 并发边界：interactive 依赖「同一 goroutine 内、loop 回合内工具串行执行」
// 与 REPL 共享同一个 LineSource（demoapp.NewLineSource：单一行缓冲、顺序
// 消费）——审批时的 y/n/a 不会被 REPL 预读缓冲抢走。多 Agent 并发审批
// 需要服务化通道，不属于本课。
func installHITL(scope *kernel.Context, mode, denyTool, allowTool, hint string, lines *demoapp.LineSource, out io.Writer, trust *sessionTrust) (*sessionTrust, error) {
	switch mode {
	case "off":
		// 基线：不装监听 = 全放行。看清「无审批时的框架默认」。
		return trust, nil

	case "denylist":
		// 默认放行 + 黑名单拦截：策略拦截的最小实现。
		deny := strings.TrimSpace(denyTool)
		_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
			func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				if deny != "" && btc.Call.Name == deny {
					btc.Rejected = true
					btc.RejectReason = "denied by demo HITL policy (" + deny + ")"
					return btc
				}
				return next(btc)
			})
		return trust, err

	case "allowlist":
		// default-deny：只放行白名单（空则仅 lookup）。
		allow := map[string]bool{}
		for _, name := range strings.Fields(strings.ReplaceAll(allowTool, ",", " ")) {
			if name != "" {
				allow[name] = true
			}
		}
		if len(allow) == 0 {
			allow["lookup"] = true
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
		return trust, err

	case "interactive":
		// 真人审批：阻塞等待 y/n/a。三语义 = 生产审批器的最小集：
		// ① 裁决在基础设施层（模型「以为已批准」也会被拦）；
		// ② fail-closed（输入不可读按拒绝，绝不静默放行）；
		// ③ n 的拒绝以 IsError 工具结果回传——回合不失败，模型可改道。
		if trust == nil {
			trust = newSessionTrust()
		}
		_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
			func(btc *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				if trust.trusted(btc.Call.Name) {
					fmt.Fprintf(out, "✔ %s 已获本会话授权，自动放行\n", btc.Call.Name)
					return next(btc)
				}
				fmt.Fprintf(out, "\n⚠ 审批请求 tool=%s args=%s\n  说明: %s\n  批准? [y]es / [n]o / [a]lways > ",
					btc.Call.Name, string(btc.Call.Arguments), hint)
				line, readErr := lines.ReadLine()
				ans := strings.ToLower(strings.TrimSpace(line))
				if readErr != nil && ans == "" {
					btc.Rejected = true
					btc.RejectReason = "approval input unavailable; fail-closed"
					fmt.Fprintln(out, "✘ 输入不可读，按 fail-closed 拒绝")
					return btc
				}
				switch ans {
				case "y", "yes":
					fmt.Fprintln(out, "→ 批准本次")
					return next(btc)
				case "a", "always":
					trust.grant(btc.Call.Name)
					fmt.Fprintln(out, "→ 批准并记住（本会话内不再询问）")
					return next(btc)
				default:
					btc.Rejected = true
					btc.RejectReason = "denied by operator"
					fmt.Fprintln(out, "→ 拒绝")
					return btc
				}
			})
		return trust, err

	default:
		return nil, fmt.Errorf("03-hitl: unsupported mode %q", mode)
	}
}

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	mode, err := parseMode(flags.HITL)
	if err != nil {
		return err
	}
	if flags.DenyTool == "" {
		flags.DenyTool = "delete_file"
	}
	scripted := []*llm.Response{
		llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"topic":"pulse"}`)}),
		llm.RespToolCalls(llm.ToolCall{ID: "c2", Name: "delete_file", Arguments: json.RawMessage(`{"path":"/tmp/x"}`)}),
		llm.Resp("演示结束：lookup 与 delete_file 的审批路径都已在 before_tool_call 走完。"),
	}
	host, err := demoapp.Open(flags, scripted...)
	if err != nil {
		return err
	}
	defer host.Close()

	// 只读 + 危险工具同台注册：Risk 元数据是审批策略的输入。
	if _, err := kernel.Use(host.Ctx, toolset.Plugin()); err != nil {
		return err
	}
	reg, ok := kernel.Get(host.Ctx, toolset.ServiceKey)
	if !ok {
		return fmt.Errorf("03-hitl: pulse.tools not provided")
	}
	if _, err := reg.Register(host.Ctx, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "lookup",
			Description: "查找本地知识",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			return `{"topic":"pulse","note":"plugin kernel + llm vocabulary + loop"}`, nil
		},
		Source: "local.lookup",
		Risk:   toolset.RiskReadonly,
	}); err != nil {
		return err
	}
	if _, err := reg.Register(host.Ctx, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "delete_file",
			Description: "删除文件，需要审批（模拟工具）",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
		Fn: func(_ context.Context, args json.RawMessage) (string, error) {
			return "deleted", nil
		},
		Source: "local.delete_file",
		Risk:   toolset.RiskDangerous,
	}); err != nil {
		return err
	}
	tools := reg.AsToolSet()

	// REPL 与审批器共享同一 LineSource：一个行缓冲、同 goroutine 顺序消费。
	stdin := demoapp.NewLineSource(os.Stdin)
	var trust *sessionTrust // interactive 跨轮 always 复用

	var history []*llm.Message
	fmt.Printf("03-hitl provider=%s model=%s scripted=%v hitl=%s deny=%q allow=%q host=%s\n",
		flags.Provider, flags.Model, flags.Scripted, mode, flags.DenyTool, flags.AllowTool, host.HostID())
	if mode == "interactive" {
		fmt.Println("interactive 模式：危险调用会暂停等待你在终端批准（y/n/a）")
	}
	return demoapp.Loop(os.Stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		// 每轮独立 reqScope + Agent + HITL（本课手写版 install）：
		// Local 派发下监听必须挂在与 Agent 相同的 reqScope，否则听不到。
		reqScope, err := host.Ctx.Derive()
		if err != nil {
			return nil, err
		}
		defer reqScope.Dispose()
		// trust 跨轮传入：`a` 授予的白名单在 reqScope 销毁后仍生效。
		trust, err = installHITL(reqScope, mode, flags.DenyTool, flags.AllowTool, toolHint, stdin, os.Stdout, trust)
		if err != nil {
			return nil, err
		}
		agent, err := loop.NewAgent(host.Model,
			loop.WithToolSet(tools),
			loop.WithSystemPrompt("你是 Pulse 示例助手。需要事实时调用 lookup；删除类操作调用 delete_file。后续轮次必须结合对话历史回答。"),
			loop.WithEventScope(reqScope),
		)
		if err != nil {
			return nil, err
		}
		res, err := agent.RunStream(context.Background(), func(delta string) {
			fmt.Print(delta)
		}, history, msg)
		if err != nil {
			return nil, err
		}
		if res.Final != nil && !strings.HasSuffix(res.Final.Text(), "\n") {
			fmt.Println()
		}
		history = append(history, msg)
		history = append(history, res.Messages...)
		extra := ""
		if len(trust.names()) > 0 {
			extra = fmt.Sprintf(" session_trust=%v", trust.names())
		}
		fmt.Printf("stopped_by=%s steps=%d history=%d%s\n",
			res.StoppedBy, res.Steps, len(history), extra)
		return res.Messages, nil
	}, func() int { return len(history) })
}
