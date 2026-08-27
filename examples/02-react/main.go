package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Luo-root/pulse/examples/internal/demoapp"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// toolHint 展示在审批提示里的说明文字（与 Flags.Prompt 无关——那是旧的一次性入口残留）。
const toolHint = "模拟危险操作，不触达真实文件系统"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "02-react: %v\n", err)
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

	tools := loop.NewMemToolSet()
	if err := tools.Register(llm.ToolDef{
		Name:        "lookup",
		Description: "查找本地知识",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
	}, func(_ context.Context, args json.RawMessage) (string, error) {
		return `{"topic":"pulse","note":"plugin kernel + llm vocabulary + loop"}`, nil
	}); err != nil {
		return err
	}
	if err := tools.Register(llm.ToolDef{
		Name:        "delete_file",
		Description: "删除文件，需要审批（模拟工具）",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}, func(_ context.Context, args json.RawMessage) (string, error) {
		return "deleted", nil
	}); err != nil {
		return err
	}

	// REPL 与审批器共享同一 LineSource：一个行缓冲、同 goroutine 顺序消费，
	// 审批时输入的 y/n/a 不会被 Loop 的预读缓冲抢走。
	stdin := demoapp.NewLineSource(os.Stdin)
	trust, err := demoapp.InstallHITL(host.Ctx, mode, flags.DenyTool, flags.AllowTool, toolHint, stdin, os.Stdout)
	if err != nil {
		return err
	}

	agent, err := loop.NewAgent(host.Model,
		loop.WithToolSet(tools),
		loop.WithSystemPrompt("你是 Pulse 示例助手。需要事实时调用 lookup；删除类操作调用 delete_file。后续轮次必须结合对话历史回答。"),
		loop.WithEventScope(host.Ctx),
	)
	if err != nil {
		return err
	}

	var history []*llm.Message
	fmt.Printf("02-react provider=%s model=%s scripted=%v hitl=%s deny=%q allow=%q\n",
		flags.Provider, flags.Model, flags.Scripted, mode, flags.DenyTool, flags.AllowTool)
	if mode == demoapp.HITLInteractive {
		fmt.Println("interactive 模式：危险调用会暂停等待你在终端批准（y/n/a）")
	}
	return demoapp.Loop(stdin, os.Stdout, func(msg *llm.Message) ([]*llm.Message, error) {
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
		fmt.Printf("stopped_by=%s steps=%d history=%d%s\n", res.StoppedBy, res.Steps, len(history), extra)
		return res.Messages, nil
	}, func() int { return len(history) })
}
