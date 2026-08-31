// 03-hitl：人机协同审批（HITL）。
//
// 运行：go run ./examples/03-hitl
// before_tool_call 是 loop 暴露在工具调用前的裁决点：演示四策略
//（denylist / interactive / allowlist / off）与会话信任表（a = 永久放行）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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

func run() error {
	flags := demoapp.LoadFlagsFromEnv()
	mode, err := demoapp.ParseHITLMode(flags.HITL)
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

	// 只读工具 + 危险工具同台注册：Risk 元数据是审批策略的输入。
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
	var trust *demoapp.SessionTrust // interactive 跨轮 always 复用

	var history []*llm.Message
	fmt.Printf("03-hitl provider=%s model=%s scripted=%v hitl=%s deny=%q allow=%q host=%s\n",
		flags.Provider, flags.Model, flags.Scripted, mode, flags.DenyTool, flags.AllowTool, host.HostID())
	if mode == demoapp.HITLInteractive {
		fmt.Println("interactive 模式：危险调用会暂停等待你在终端批准（y/n/a）")
	}
	return demoapp.Loop(stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
		// 每轮独立 reqScope + Bridge + Agent + HITL：
		// Local 派发下 HITL 必须挂在与 Agent 相同的 reqScope，否则听不到。
		reqScope, err := host.Ctx.Derive()
		if err != nil {
			return nil, err
		}
		defer reqScope.Dispose()
		bridge, err := host.NewBridge(reqScope)
		if err != nil {
			return nil, err
		}
		// trust 跨轮传入：interactive 下操作者按 a 授予的会话级白名单
		// 在后续轮次仍然生效（reqScope 销毁不影响 trust 对象）。
		trust, err = demoapp.InstallHITLWithTrust(reqScope, mode, flags.DenyTool, flags.AllowTool, toolHint, stdin, os.Stdout, trust, reg)
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
		if trust != nil && len(trust.Names()) > 0 {
			extra = fmt.Sprintf(" session_trust=%v", trust.Names())
		}
		fmt.Printf("stopped_by=%s steps=%d history=%d trace=%s%s\n",
			res.StoppedBy, res.Steps, len(history), bridge.TraceID, extra)
		return res.Messages, nil
	}, func() int { return len(history) })
}
