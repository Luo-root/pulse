package host

// 站点《装配指南》（site/guide/assembly.md）的组装配方**逐字编译 + 真跑**兜底。
//
// 仓库既有约定：README / 指南里教的装配必须有人编译（见 host README 的
// TestHostContextBuilderRecipe）——没人编译的片段正是字段名写错还能躺在
// 文档里的原因。本文件与页面的差别**逐条列明**（其余逐行照抄）：
//
//  1. 真实 provider 换成脚本模型 `llm.NewScripted(llm.Resp("…"))`（OpenAI
//     适配器要凭据与网络，测试里不可用）——换法就是 host.Provider 的闭包
//     形态，页面上的 `host.Provider(openai.Register)` 是同一个挂点；相应的
//     Models 声明行换成该 provider 的键与占位模型名；
//  2. 包内测试里 `host.X` 写作 `X`（同一个包），收尾的 `panic` / `Println`
//     换成 `t.Fatal` 与断言；
//  3. 由宿主提供的值改由测试提供：模型声明名（测试宿主声明的是 "stub"）、
//     会话目录（`t.TempDir()`）、技能目录（临时目录里现写一个 SKILL.md）、
//     MCP Client（下面的 guideMCPClient）；
//  4. §五·a 的 Tools 是两段：页面那行 `builtins.Register(c, reg, builtins.Options{Root: …})`
//     逐字编译（Root 换成 `t.TempDir()`），另加一个自建 echo 来源供断言用
//     （要证的是「来源闭包在装配期被调用 + 注册进去的工具真能执行」）；
//  5. §五·a 的冷恢复裁决片段（`Open` → `Recoverable.Pending` → `ResolvePending`
//     → `Flush` → 续跑）由 TestSiteAssemblyGuideRecoveryRecipe 逐字编译并真跑；
//     崩溃现场用「跑到 HITL 等待点后关会话句柄」造出来（与跨包用例同一手法）。
//
// 覆盖：§二 主装配、§五·a 会话（JSONL + RecoverExposePending + 冷恢复裁决）、
// §五·b HITL、§五·e MCP 来源、§五·f 技能。§五·c 由 TestHostContextBuilderRecipe
// 覆盖、§五·d 由 TestHostObservePerRequest / TestHostAttachCollectorBusinessWrite
// 覆盖（本页与 host README 是同一份配方）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/memory"
	"github.com/Luo-root/pulse/memory/session"
	"github.com/Luo-root/pulse/skills"
	"github.com/Luo-root/pulse/toolset"
	"github.com/Luo-root/pulse/toolset/builtins"
	"github.com/Luo-root/pulse/toolset/mcp"
)

// guideScriptedProvider 是页面上 host.Provider(openai.Register) 的测试替身：
// 注册一个总是返回脚本模型的 provider 工厂（签名与 openai.Register 对齐）。
func guideScriptedProvider(model llm.ChatModel) Provider {
	return func(c *kernel.Context, reg *llm.Registry) error {
		_, err := reg.RegisterProvider(c, "guide-scripted", func(llm.Config) (llm.ChatModel, error) {
			return model, nil
		})
		return err
	}
}

// TestSiteAssemblyGuideRecipe：指南 §二「最快路径」那段 15 行主装配逐字编译，
// 并真跑一回合（断言最终文本、会话落盘、request.header 的模型审计名）。
func TestSiteAssemblyGuideRecipe(t *testing.T) {
	ctx := context.Background()

	// ↓↓↓ 页面「最快路径」代码块（provider 两行换成脚本模型，其余逐字）↓↓↓
	k := kernel.New() // 内核归应用所有：你的插件（UI、审批、任务队列…）Use 同一个根
	defer k.Dispose() // 生命周期归调用方，Host 没有 Close

	h, err := New(Options{
		Kernel:    k,
		Providers: []Provider{guideScriptedProvider(llm.NewScripted(llm.Resp("ok")))},
		Models: []ModelDecl{
			{Name: "main", Config: llm.Config{Provider: "guide-scripted", Model: "test-model"}},
		},
		Session: memory.NewMemorySessionStack(),
	})
	if err != nil {
		t.Fatal(err)
	}

	agent, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "main", System: "You are a concise assistant."})
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(ctx, llm.UserText("你好"))
	if err != nil {
		t.Fatal(err)
	}
	// ↑↑↑ 页面代码块结束（页面收尾是 fmt.Println(res.Final.Text())）↑↑↑

	if res.Final == nil || res.Final.Text() != "ok" {
		t.Fatalf("final = %+v, want the scripted reply", res.Final)
	}

	// 会话真接上了：Surface 折出本轮 user + assistant。
	surface, err := agent.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 2 || surface[0].Role != llm.RoleUser || surface[1].Role != llm.RoleAssistant {
		t.Fatalf("surface = %+v, want [user assistant]", surface)
	}
	if surface[0].Text() != "你好" || surface[1].Text() != "ok" {
		t.Fatalf("surface text = %q / %q", surface[0].Text(), surface[1].Text())
	}

	// request.header 的模型审计名：DefaultAgent 用声明名填 ModelName。
	envs, err := agent.Session().Events(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var audit string
	for _, env := range envs {
		if env.Type != session.EventRequestHeader {
			continue
		}
		var p session.RequestHeaderPayload
		if err := json.Unmarshal(env.Data, &p); err != nil {
			t.Fatal(err)
		}
		audit = p.Model
	}
	if audit != "main" {
		t.Fatalf("request.header model = %q, want the declared name", audit)
	}
}

// TestSiteAssemblyGuideSessionRecipe：指南 §五·a 的会话配方——JSONL store 带
// RecoverExposePending 策略经泛化构造接进 host，跑一回合（含一次工具调用），
// 关句柄后按 SessionID 续跑。
func TestSiteAssemblyGuideSessionRecipe(t *testing.T) {
	ctx := context.Background()

	// ↓↓↓ 页面 §五·a 的存储段（dir 由测试提供，其余逐字）↓↓↓
	store, err := session.NewJSONLStore(t.TempDir(), session.WithRecoverPolicy(session.RecoverExposePending))
	if err != nil {
		t.Fatal(err)
	}
	// ↑↑↑ 页面这里写的是 "data/sessions" ↑↑↑

	h := newTestHost(t,
		llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("first"),
			llm.Resp("second"),
		),
		func(o *Options) {
			// ↓↓↓ 页面 §五·a 的 builtins 闭包逐字（Root 换成测试临时目录）↓↓↓
			root := t.TempDir()
			// ↑↑↑ 页面这里写的是 "workspace" ↑↑↑
			o.Tools = []ToolSource{
				func(c *kernel.Context, reg *toolset.Registry) error {
					_, err := builtins.Register(c, reg, builtins.Options{Root: root})
					return err
				},
				// 页面没有这一段：断言用的 echo 来源（证明来源闭包真被调用、
				// 注册进去的工具真能执行）。
				func(c *kernel.Context, reg *toolset.Registry) error {
					_, err := reg.Register(c, toolset.Registration{
						Def:    llm.ToolDef{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
						Fn:     func(context.Context, json.RawMessage) (string, error) { return "pong", nil },
						Source: "guide.echo",
						Risk:   toolset.RiskReadonly,
					})
					return err
				},
			}
			o.Session = memory.NewSessionStack(store) // 泛化构造：store 带策略，门面只收敛构造
			// ↑↑↑ 页面这里用的是 host.New(host.Options{…})，来源换成测试宿主 ↑↑↑
		})

	a1, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a1.Run(ctx, llm.UserText("你好")); err != nil {
		t.Fatal(err)
	}
	id := a1.Session().Header().SessionID
	if id == "" {
		t.Fatal("session id must be generated")
	}
	// 释放文件锁：同一进程重复 Open 会复用缓存句柄，模拟「重开」要显式 Close。
	if c, ok := a1.Session().(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// 页面 §五·a 的续跑一行。
	a2, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "stub", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := a2.Session().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})

	// 已闭合的日志在 ExposePending 档照常投影：策略只改变「未决现场」的处理。
	surface, err := a2.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	roles := make([]string, 0, len(surface))
	for _, m := range surface {
		roles = append(roles, string(m.Role))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant" {
		t.Fatalf("reopened surface roles = %v", roles)
	}
	if surface[0].Text() != "你好" || surface[3].Text() != "first" {
		t.Fatalf("continued history lost: %q … %q", surface[0].Text(), surface[3].Text())
	}

	// 续跑的后一半：跨两次构造仍然续得上。
	res, err := a2.Run(ctx, llm.UserText("再来一轮"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Final.Text() != "second" {
		t.Fatalf("final = %q, want the second scripted reply", res.Final.Text())
	}
	surface, err = a2.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) != 6 {
		t.Fatalf("surface len = %d after resume round, want 6", len(surface))
	}
}

// TestSiteAssemblyGuideHITLRecipe：指南 §五·b 的审批配方——闸门用
// `h.Tools().Preview` 取执行前权限卡片，`ScopeHook` 的 waterfall 改写调用；
// 批准一次、拒绝一次，断言工具执行次数与执行到的参数（批准的 = 执行的）、
// 以及拒绝理由作为 IsError 结果回到模型。
func TestSiteAssemblyGuideHITLRecipe(t *testing.T) {
	ctx := context.Background()

	var executed []string
	var cards []toolset.Preview
	h := newTestHost(t,
		llm.NewScripted(
			llm.RespToolCalls(
				llm.ToolCall{ID: "c1", Name: "write_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
				llm.ToolCall{ID: "c2", Name: "write_file", Arguments: json.RawMessage(`{"path":"b.txt"}`)},
			),
			llm.Resp("done"),
		),
		func(o *Options) {
			o.Tools = []ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
				_, err := reg.Register(c, toolset.Registration{
					Def: llm.ToolDef{
						Name:        "write_file",
						Description: "写一个文件",
						Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
					},
					Source: "guide.local",
					Risk:   toolset.RiskReadWrite,
					Fn: func(_ context.Context, args json.RawMessage) (string, error) {
						var p struct {
							Path string `json:"path"`
						}
						if err := json.Unmarshal(args, &p); err != nil {
							return "", err
						}
						executed = append(executed, p.Path)
						return "wrote " + p.Path, nil
					},
					PreviewFn: func(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
						var p struct {
							Path string `json:"path"`
						}
						if err := json.Unmarshal(args, &p); err != nil {
							return toolset.Preview{}, err
						}
						return toolset.Preview{
							Action:  toolset.ActionWrite,
							Kind:    toolset.KindFile,
							Subject: "/workspace/" + p.Path,
						}, nil
					},
				})
				return err
			}}
		})

	// askHuman 是宿主自己的裁决来源（审批 UI / 策略表 / 终端提示）：测试里用
	// 闭包顶替——只批准写 a.txt 的那一次；sanitize 顶替宿主的参数脱敏/改写。
	askHuman := func(_ context.Context, card toolset.Preview) bool {
		cards = append(cards, card)
		return strings.HasSuffix(card.Subject, "a.txt")
	}
	sanitize := func(args json.RawMessage) json.RawMessage {
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return args
		}
		return json.RawMessage(`{"path":"safe/` + p.Path + `"}`)
	}

	// ↓↓↓ 页面 §五·b 的 ToolGate / ScopeHook（两个宿主闭包由上面顶替）↓↓↓
	a, err := h.DefaultAgent(ctx, DefaultAgentOptions{
		Name: "main", Model: "stub",
		// (1) 执行前权限卡片：闸门闭包持 h.Tools()，用 toolset 的预览面取卡片。
		ToolGate: func(gctx context.Context, call llm.ToolCall) (bool, string) {
			card, ok, err := h.Tools().Preview(gctx, call.Name, call.Arguments)
			if err != nil || !ok {
				return false, "no preview card" // 没卡片也照问人，绝不自动放行
			}
			return askHuman(gctx, card), "rejected by approval UI"
		},
		ScopeHook: func(scope *kernel.Context) error {
			_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
				func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
					p.Call.Arguments = sanitize(p.Call.Arguments) // 就地改写；只观察也必须委托 next
					return next(p)
				})
			return err
		},
	})
	// ↑↑↑ 页面 §五·b 的 ToolGate / ScopeHook 结束 ↑↑↑
	if err != nil {
		t.Fatal(err)
	}

	res, err := a.Run(ctx, llm.UserText("写两个文件"))
	if err != nil {
		t.Fatal(err)
	}

	// 闸门看到的是**改写后**的最终调用（后序），所以卡片主体是 safe/ 下的路径。
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want one per call", len(cards))
	}
	if cards[0].Subject != "/workspace/safe/a.txt" || cards[0].Action != toolset.ActionWrite {
		t.Fatalf("card[0] = %+v, want the rewritten subject", cards[0])
	}
	if cards[1].Subject != "/workspace/safe/b.txt" {
		t.Fatalf("card[1] = %+v", cards[1])
	}

	// 批准的 = 执行的：只执行了被批准的那一次，且执行的是改写后的参数。
	if len(executed) != 1 || executed[0] != "safe/a.txt" {
		t.Fatalf("executed = %v, want exactly the approved rewritten call", executed)
	}

	// 模型侧：被拒的调用是 IsError 结果且带拒绝理由；被批的是工具真实产出。
	var approved, rejected bool
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if p.Kind != llm.PartToolResult || p.ToolResultValue == nil {
				continue
			}
			var text string
			for _, c := range p.ToolResultValue.Content {
				text += c.Text
			}
			switch p.ToolResultValue.ToolCallID {
			case "c1":
				if p.ToolResultValue.IsError || text != "wrote safe/a.txt" {
					t.Fatalf("approved call result = %q isError=%v", text, p.ToolResultValue.IsError)
				}
				approved = true
			case "c2":
				if !p.ToolResultValue.IsError || !strings.Contains(text, "rejected by approval UI") {
					t.Fatalf("rejected call result = %q isError=%v", text, p.ToolResultValue.IsError)
				}
				rejected = true
			}
		}
	}
	if !approved || !rejected {
		t.Fatalf("model-visible results: approved=%v rejected=%v", approved, rejected)
	}
	if res.Final.Text() != "done" {
		t.Fatalf("final = %q", res.Final.Text())
	}
}

// guideMCPClient 是 mcp.Client 的最小实现（装配指南的 MCP 配方里由官方
// go-sdk / mcp-go / 自建适配器顶替）。
type guideMCPClient struct{ calls int }

func (c *guideMCPClient) ListTools(context.Context) ([]mcp.Tool, error) {
	return []mcp.Tool{{
		Name:        "read_file",
		Description: "读一个文件",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}, nil
}

func (c *guideMCPClient) CallTool(_ context.Context, name string, _ json.RawMessage) (string, error) {
	c.calls++
	return "content of " + name, nil
}

func (c *guideMCPClient) Close() error { return nil }

// TestSiteAssemblyGuideToolSourceRecipe：指南 §五·e（MCP 来源）与 §五·f（技能）
// 两个 ToolSource 配方逐字编译并可用——模型可见名带前缀、来源元数据可查、
// 上游 Client 真的被调用、技能工具真的读得到目录。
func TestSiteAssemblyGuideToolSourceRecipe(t *testing.T) {
	ctx := context.Background()

	client := &guideMCPClient{}

	// §五·f 的一行：目录下每个含 SKILL.md 的子目录是一个 skill。
	root := t.TempDir()
	skillDir := filepath.Join(root, "guide-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: guide-skill\ndescription: 装配指南示例技能\n---\n# 示例\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	// ↓↓↓ 页面 §五·f（root 由测试提供）↓↓↓
	loader, err := skills.Open(root) // 目录下每个含 SKILL.md 的子目录是一个 skill
	if err != nil {
		t.Fatal(err)
	}
	// ↑↑↑ 页面这里写的是 skills.Open("skills") ↑↑↑

	h := newTestHost(t,
		llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "m1", Name: "fs_read_file", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("done"),
		),
		func(o *Options) {
			o.Tools = []ToolSource{
				// ↓↓↓ 页面 §五·e 的 MCP ToolSource（client 由测试提供）↓↓↓
				func(c *kernel.Context, reg *toolset.Registry) error {
					src, err := mcp.NewSource(reg, mcp.Config{
						ID:          "filesystem",          // Source 元数据固定为 "mcp." + ID
						Client:      client,                // mcp.Client：官方 go-sdk / mcp-go / 自建适配
						NamePrefix:  "fs",                  // 非空时模型可见名 = fs_<上游名>
						DefaultRisk: toolset.RiskReadWrite, // 必填；Unspecified 被拒绝
					})
					if err != nil {
						return err
					}
					return src.Sync(c, context.Background())
				},
				// ↓↓↓ 页面 §五·f 的 Tools 一行 ↓↓↓
				SkillTools(loader), // 注册 list_skills / load_skill 两个只读工具
				// ↑↑↑ 页面这里写作 host.SkillTools(loader) ↑↑↑
			}
		})

	// 注册面：上游名按前缀定名，来源元数据是 "mcp." + ID（不是名字前缀协议）。
	src, risk, ok := h.Tools().LookupMeta("fs_read_file")
	if !ok || src != "mcp.filesystem" || risk != toolset.RiskReadWrite {
		t.Fatalf("mcp tool meta = (%q, %v, %v)", src, risk, ok)
	}
	// 技能工具是只读的「读取规程」工具，不是可执行件。
	if src, risk, ok := h.Tools().LookupMeta("load_skill"); !ok || src != "skills.load" || risk != toolset.RiskReadonly {
		t.Fatalf("skill tool meta = (%q, %v, %v)", src, risk, ok)
	}

	// MCP 工具真能用：模型调一次，上游 Client.CallTool 被调用一次。
	agent, err := h.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(ctx, llm.UserText("读一下"))
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("upstream CallTool calls = %d, want 1", client.calls)
	}
	if res.Final.Text() != "done" {
		t.Fatalf("final = %q", res.Final.Text())
	}
	var toolText string
	for _, m := range res.Messages {
		for _, p := range m.Parts {
			if p.Kind == llm.PartToolResult && p.ToolResultValue != nil {
				for _, c := range p.ToolResultValue.Content {
					toolText += c.Text
				}
			}
		}
	}
	if !strings.Contains(toolText, "content of read_file") {
		t.Fatalf("tool result = %q, want the upstream payload", toolText)
	}

	// 技能工具真能读目录：list_skills 返回 name + description 的目录。
	out, err := h.Tools().AsToolSet().Execute(ctx, llm.ToolCall{Name: "list_skills", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "guide-skill") || !strings.Contains(out, "装配指南示例技能") {
		t.Fatalf("list_skills = %q", out)
	}
}

// TestSiteAssemblyGuideRecoveryRecipe：指南 §五·a 的**冷恢复裁决**片段逐字编译
// 并真跑——先造一个真崩溃现场（跑到 HITL 等待点后关会话句柄），再用
// RecoverExposePending 打开、裁决、续跑。顺带把这段最容易写错的 API 语义钉住：
// **`Open` 恒成功**，哨兵 `ErrPendingEvents` 出现在 `Surface()` / `Run`（不是 `Open`）。
func TestSiteAssemblyGuideRecoveryRecipe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	echo := func(c *kernel.Context, reg *toolset.Registry) error {
		_, err := reg.Register(c, toolset.Registration{
			Def:    llm.ToolDef{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)},
			Fn:     func(context.Context, json.RawMessage) (string, error) { return "pong", nil },
			Source: "guide.echo",
			Risk:   toolset.RiskReadonly,
		})
		return err
	}

	// ---- 第一段生命周期：跑到 HITL 等待点后「猝死」（关句柄）----
	stack1, err := memory.NewJSONLSessionStack(dir)
	if err != nil {
		t.Fatal(err)
	}
	h1 := newTestHost(t,
		llm.NewScripted(
			llm.RespToolCalls(llm.ToolCall{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{}`)}),
			llm.Resp("never reached"),
		),
		func(o *Options) {
			o.Session = stack1
			o.Tools = []ToolSource{echo}
		})
	gateEntered := make(chan struct{}, 1)
	gateRelease := make(chan struct{})
	gate := func(_ context.Context, call llm.ToolCall) (bool, string) {
		gateEntered <- struct{}{}
		<-gateRelease
		return true, ""
	}
	a1, err := h1.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "stub", ToolGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	id := a1.Session().Header().SessionID
	roundErr := make(chan error, 1)
	go func() {
		_, err := a1.Run(ctx, llm.UserText("请调用工具"))
		roundErr <- err
	}()
	<-gateEntered // 已到等待点：assistant(tool_call) 落盘并经 HITL 检查点 Flush
	if c, ok := a1.Session().(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	close(gateRelease) // 放行闸门，让回合走完（预期因落盘失败而报错）
	if err := <-roundErr; err == nil {
		t.Fatal("the round must fail after the persistence sink is gone (fail closed)")
	}

	// ---- 第二段生命周期：ExposePending 打开 → 裁决 → 续跑 ----
	store, err := session.NewJSONLStore(dir, session.WithRecoverPolicy(session.RecoverExposePending))
	if err != nil {
		t.Fatal(err)
	}
	h2 := newTestHost(t, llm.NewScripted(llm.Resp("resumed")), func(o *Options) {
		o.Session = memory.NewSessionStack(store)
		o.Tools = []ToolSource{echo}
	})

	// ↓↓↓ 页面 §五·a 的冷恢复裁决片段（h2 / id 由上面提供）↓↓↓
	sess, err := h2.SessionStack().Open(ctx, id) // 未决现场不在这里报错
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := sess.(session.Recoverable) // 裁决面只在 RecoverExposePending 档存在
	if !ok {
		t.Fatal("this policy must expose the adjudication surface")
	}
	p := rec.Pending() // 现场快照：缺 result 的调用（带当时的载荷）+ 悬空 step/turn
	if len(p.Calls) != 1 || p.Calls[0].ToolCallID != "c1" || !p.HasOpenStep || !p.HasOpenTurn {
		t.Fatalf("pending = %+v, want one call plus an open step and turn", p)
	}
	// 「未裁决期间拒绝投影」是 Surface / Run 的行为，不是 Open 的。
	if _, err := sess.Surface(ctx); !errors.Is(err, session.ErrPendingEvents) {
		t.Fatalf("Surface before adjudication = %v, want ErrPendingEvents", err)
	}
	blocked, err := h2.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "stub", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Run(ctx, llm.UserText("未裁决就想跑")); !errors.Is(err, session.ErrPendingEvents) {
		t.Fatalf("Run before adjudication = %v, want ErrPendingEvents", err)
	}
	if len(p.Calls) > 0 {
		// 补**真实**结果（回到等待点，而不是作废）；要重发就把载荷再跑一遍再填这里
		if err := rec.ResolvePending(ctx, session.ResolvePendingOption{
			Result: &session.ToolResultPayload{ToolCallID: p.Calls[0].ToolCallID, Text: "已人工裁决：批准"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 悬空的 step / turn 逐层闭合（Interrupted = 默认合成闭环）
	if p.HasOpenStep {
		if err := rec.ResolvePending(ctx, session.ResolvePendingOption{Interrupted: true}); err != nil {
			t.Fatal(err)
		}
	}
	if p.HasOpenTurn {
		if err := rec.ResolvePending(ctx, session.ResolvePendingOption{Interrupted: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Flush(ctx); err != nil { // 裁决本身也是写日志：收尾刷一次
		t.Fatal(err)
	}
	// ↑↑↑ 页面片段结束（页面收尾是「裁决完就能续跑」那两行）↑↑↑

	if left := rec.Pending(); len(left.Calls) != 0 || left.HasOpenStep || left.HasOpenTurn {
		t.Fatalf("pending after adjudication = %+v, want it cleared", left)
	}
	agent, err := h2.DefaultAgent(ctx, DefaultAgentOptions{Name: "main", Model: "stub", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := agent.Session().(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	res, err := agent.Run(ctx, llm.UserText("继续"))
	if err != nil {
		t.Fatalf("resumed round: %v", err)
	}
	if res.Final.Text() != "resumed" {
		t.Fatalf("resumed final = %q", res.Final.Text())
	}
	// 裁决补的是**真实**结果：它进了 surface，不是被当成没发生。
	surface, err := agent.Session().Surface(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range surface {
		for _, part := range m.Parts {
			if part.ToolResultValue == nil {
				continue
			}
			for _, c := range part.ToolResultValue.Content {
				if strings.Contains(c.Text, "已人工裁决：批准") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("the adjudicated result must be part of the resumed history")
	}
}
