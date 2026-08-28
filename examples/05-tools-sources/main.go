package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/skills"
	"github.com/Luo-root/pulse/toolset"
	mcpsrc "github.com/Luo-root/pulse/toolset/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "05-tools-sources: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	host := kernel.New()
	defer host.Dispose()

	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		return err
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		return fmt.Errorf("pulse.tools not provided")
	}

	// --- 1) toolset 本地注册 ---
	if err := registerLocal(host, reg); err != nil {
		return err
	}

	// --- 2) MCP Source（InMemory，无外部进程）---
	mcpCleanup, err := attachMCP(host, reg)
	if err != nil {
		return err
	}
	defer mcpCleanup()

	// --- 3) Skills Discovery 短表（进 Messages，不进 Tools）---
	loader, metas, err := openSkills()
	if err != nil {
		return err
	}
	if err := registerSkillTools(host, reg, loader); err != nil {
		return err
	}

	defs := reg.AsToolSet().Definitions()
	fmt.Println("=== 05-tools-sources：三路装配对照 ===")
	fmt.Println()
	fmt.Println("[1] toolset 本地注册 → pulse.tools")
	fmt.Println("    lookup, delete_file")
	fmt.Println("[2] MCP Source（InMemory go-sdk）→ 同一 Registry")
	fmt.Println("    mcp_echo  (NamePrefix=mcp)")
	fmt.Println("[3] Skills Loader → Messages 短表 + 只读 list_skills/load_skill")
	for _, m := range metas {
		fmt.Printf("    - %s: %s\n", m.Name, trimDesc(m.Description, 72))
	}
	fmt.Println()
	fmt.Println("当前 Definitions():")
	for _, d := range defs {
		fmt.Printf("  - %s — %s\n", d.Name, d.Description)
	}
	fmt.Println()

	model := llm.NewScripted(
		llm.RespToolCalls(
			llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"topic":"pulse"}`)},
			llm.ToolCall{ID: "c2", Name: "mcp_echo", Arguments: json.RawMessage(`{"msg":"hello-mcp"}`)},
			llm.ToolCall{ID: "c3", Name: "list_skills", Arguments: json.RawMessage(`{}`)},
		),
		llm.RespToolCalls(
			llm.ToolCall{ID: "c4", Name: "load_skill", Arguments: json.RawMessage(`{"name":"frontend-design"}`)},
		),
		llm.Resp("三路演示结束：本地 toolset、MCP Source、Skills 短表/只读工具都已走过。"),
	)

	agent, err := loop.NewAgent(model,
		loop.WithToolSet(reg.AsToolSet()),
		loop.WithSystemPrompt(buildSystem(metas)),
		loop.WithEventScope(host),
	)
	if err != nil {
		return err
	}

	res, err := agent.Run(context.Background(), nil, llm.UserText("请分别演示本地 lookup、MCP echo，并列出 skills 后加载 frontend-design。"))
	if err != nil {
		return err
	}

	fmt.Println("=== 回合结果 ===")
	fmt.Printf("stopped_by=%s steps=%d\n", res.StoppedBy, res.Steps)
	for i, m := range res.Messages {
		if m == nil {
			continue
		}
		switch m.Role {
		case llm.RoleAssistant:
			if text := m.Text(); text != "" {
				fmt.Printf("[%d] assistant: %s\n", i, text)
			}
			for _, tc := range m.ToolCalls() {
				fmt.Printf("[%d] assistant tool_call: %s %s\n", i, tc.Name, string(tc.Arguments))
			}
		case llm.RoleTool:
			for _, p := range m.Parts {
				if p.Kind == llm.PartToolResult && p.ToolResultValue != nil {
					text := partText(p.ToolResultValue.Content)
					if len(text) > 160 {
						text = text[:160] + "…"
					}
					fmt.Printf("[%d] tool_result id=%s err=%v: %s\n",
						i, p.ToolResultValue.ToolCallID, p.ToolResultValue.IsError, text)
				}
			}
		}
	}
	if res.Final != nil {
		fmt.Printf("final: %s\n", res.Final.Text())
	}
	return nil
}

func registerLocal(scope *kernel.Context, reg *toolset.Registry) error {
	if _, err := reg.Register(scope, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "lookup",
			Description: "本地 toolset 注册的只读查询",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		},
		Fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return `{"source":"local.lookup","ok":true}`, nil
		},
		Source: "local.lookup",
		Risk:   toolset.RiskReadonly,
	}); err != nil {
		return err
	}
	_, err := reg.Register(scope, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "delete_file",
			Description: "本地 toolset 注册的危险模拟工具（本示例脚本不会调用）",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
		Fn:     func(_ context.Context, _ json.RawMessage) (string, error) { return "deleted", nil },
		Source: "local.delete_file",
		Risk:   toolset.RiskDangerous,
	})
	return err
}

type echoIn struct {
	Msg string `json:"msg"`
}

func attachMCP(scope *kernel.Context, reg *toolset.Registry) (func(), error) {
	ctx := context.Background()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "pulse-05-mcp", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "echo",
		Description: "MCP InMemory 回声（经 Source 进入 Registry）",
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in echoIn) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "mcp-echo:" + in.Msg}},
		}, nil, nil
	})
	t1, t2 := sdkmcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, t1, nil)
	if err != nil {
		return nil, err
	}
	client, err := mcpsrc.ConnectSDK(ctx, t2)
	if err != nil {
		_ = ss.Close()
		return nil, err
	}
	src, err := mcpsrc.NewSource(reg, mcpsrc.Config{
		ID:          "demo",
		Client:      client,
		NamePrefix:  "mcp",
		DefaultRisk: toolset.RiskReadonly,
	})
	if err != nil {
		_ = client.Close()
		_ = ss.Close()
		return nil, err
	}
	if err := src.Sync(scope, ctx); err != nil {
		_ = client.Close()
		_ = ss.Close()
		return nil, err
	}
	return func() {
		src.Detach()
		_ = client.Close()
		_ = ss.Close()
	}, nil
}

func openSkills() (skills.Loader, []skills.Meta, error) {
	root, err := findSkillsRoot()
	if err != nil {
		return nil, nil, err
	}
	loader, err := skills.Open(root)
	if err != nil {
		return nil, nil, err
	}
	metas, err := loader.List(context.Background())
	if err != nil {
		return nil, nil, err
	}
	return loader, metas, nil
}

func findSkillsRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "examples", "skills")
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("cannot find examples/skills")
}

func registerSkillTools(scope *kernel.Context, reg *toolset.Registry, loader skills.Loader) error {
	if _, err := reg.Register(scope, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "list_skills",
			Description: "列出已发现 Skills（只读工具；Skill 本身不是 Source）",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
		Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
			metas, err := loader.List(ctx)
			if err != nil {
				return "", err
			}
			b, err := json.Marshal(metas)
			return string(b), err
		},
		Source: "skills.list",
		Risk:   toolset.RiskReadonly,
	}); err != nil {
		return err
	}
	_, err := reg.Register(scope, toolset.Registration{
		Def: llm.ToolDef{
			Name:        "load_skill",
			Description: "加载 Skill 正文（只读；回合内激活通道）",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		},
		Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
			var p struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "", err
			}
			return loader.Load(ctx, p.Name)
		},
		Source: "skills.load",
		Risk:   toolset.RiskReadonly,
	})
	return err
}

func buildSystem(metas []skills.Meta) string {
	var b strings.Builder
	b.WriteString("你是 Pulse 05 示例助手。Tools 来自 toolset/MCP；Skills 短表如下（规程包，不是 Tools）：\n")
	for _, m := range metas {
		b.WriteString("- ")
		b.WriteString(m.Name)
		b.WriteString(": ")
		b.WriteString(m.Description)
		b.WriteByte('\n')
	}
	b.WriteString("可用工具：lookup、delete_file、mcp_echo、list_skills、load_skill。")
	return b.String()
}

func trimDesc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func partText(parts []llm.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == llm.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
