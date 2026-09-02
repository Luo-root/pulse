// 05-memory-session：会话真相——事件日志是唯一事实源，Surface 只是投影。
//
// 运行：go run ./examples/05-memory-session
// 四段演示，全部离线（Scripted 摘要模型，无需 API Key）：
//  1. 内存 session：Append 事件 → Surface() fold 投影
//  2. JSONL 持久化：Flush/Close → 重新 Open，Surface 一致
//  3. 崩溃恢复：半截 tool call 重新 Open，合成闭合事件真实写回日志
//  4. compaction：压力检测 → 八步事务压缩 → Surface 换摘要、raw log 不动
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/compaction"
	"github.com/Luo-root/pulse/memory/session"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "05-memory-session: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// ---- ① 内存 session：事件进，投影出 ----
	memStore := session.NewMemoryStore()
	sess, err := memStore.Create(ctx, session.SessionHeader{})
	if err != nil {
		return err
	}
	for _, d := range seedTurn() {
		if _, err := sess.Append(ctx, d); err != nil {
			return err
		}
	}
	surface, err := sess.Surface(ctx)
	if err != nil {
		return err
	}
	events, err := sess.Events(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Println("== ① 内存 session：Append 四条事件（user → assistant(call) → tool.result → assistant）==")
	fmt.Printf("raw events=%d  surface=%d 条消息\n", len(events), len(surface))
	for i, m := range surface {
		fmt.Printf("  [%d] %s: %s\n", i, m.Role, preview(msgLabel(m)))
	}

	// ---- ②+③ JSONL：持久化与崩溃恢复 ----
	root, err := os.MkdirTemp("", "pulse-05-jsonl-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	if err := demoPersistAndRecover(ctx, filepath.Join(root, "persist")); err != nil {
		return err
	}
	if err := demoCrashRecovery(ctx, filepath.Join(root, "crash")); err != nil {
		return err
	}

	// ---- ④ compaction：Surface 换摘要，raw log 不动 ----
	if err := demoCompaction(ctx, filepath.Join(root, "compact")); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("课程要点：model-visible means logged——给模型看的（Surface）必须真实落日志；")
	fmt.Println("压缩只追加 + surface replace，不删原始证据；恢复合成的闭合事件也写回日志。")
	return nil
}

// seedTurn 一个完整 tool turn 的四条事件：user → assistant(call) → result → assistant。
func seedTurn() []session.EventDraft {
	return []session.EventDraft{
		draftUser("find the config"),
		draftAssistant(llm.Call(llm.ToolCall{ID: "c1", Name: "lookup"})),
		draftToolResult("c1", "config found at /etc/app.yaml"),
		draftAssistant(llm.Text("done, config loaded")),
	}
}

func draftUser(text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageUser,
		Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text(text)}}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftAssistant(parts ...llm.Part) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventMessageAssistant,
		Data:    mustJSON(session.MessagePayload{Parts: parts}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func draftToolResult(callID, text string) session.EventDraft {
	return session.EventDraft{
		Type:    session.EventToolResult,
		Data:    mustJSON(session.ToolResultPayload{ToolCallID: callID, Text: text}),
		Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// demoPersistAndRecover：JSONL 落盘 → Close → 重新 Open，Surface 一致。
func demoPersistAndRecover(ctx context.Context, root string) error {
	store, err := session.NewJSONLStore(root)
	if err != nil {
		return err
	}
	sess, err := store.Create(ctx, session.SessionHeader{})
	if err != nil {
		return err
	}
	for _, d := range seedTurn() {
		if _, err := sess.Append(ctx, d); err != nil {
			return err
		}
	}
	if err := sess.Flush(ctx); err != nil { // JSONL 版 fsync：崩溃只保证 Flush 点之前
		return err
	}
	before, err := sess.Surface(ctx)
	if err != nil {
		return err
	}
	id := sess.Header().SessionID
	if c, ok := sess.(interface{ Close() error }); ok {
		_ = c.Close() // 释放文件锁与句柄
	}

	// 重新打开：Open 即冷恢复入口。
	sess2, err := store.Open(ctx, id)
	if err != nil {
		return err
	}
	after, err := sess2.Surface(ctx)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("== ② JSONL 持久化：Flush → Close → Open ==")
	fmt.Printf("重开前后 surface 一致：%v（%d 条消息）\n", renderSurface(before) == renderSurface(after), len(after))
	headers, _, err := store.List(ctx, session.SessionFilter{})
	if err != nil {
		return err
	}
	fmt.Printf("store.List = %d 个会话\n", len(headers))
	return nil
}

// demoCrashRecovery：半截 tool call（assistant 发起、无 result）后重开——
// Open 即冷恢复：合成闭合事件**真实写回日志**，fold 出合法 surface。
func demoCrashRecovery(ctx context.Context, root string) error {
	store, err := session.NewJSONLStore(root)
	if err != nil {
		return err
	}
	sess, err := store.Create(ctx, session.SessionHeader{})
	if err != nil {
		return err
	}
	id := sess.Header().SessionID
	if _, err := sess.Append(ctx, draftUser("delete the file")); err != nil {
		return err
	}
	// 崩溃现场：assistant 已发起 tool call，进程在 result 落盘前死亡。
	if _, err := sess.Append(ctx, draftAssistant(llm.Call(llm.ToolCall{ID: "c9", Name: "delete_file"}))); err != nil {
		return err
	}
	if err := sess.Flush(ctx); err != nil {
		return err
	}
	if c, ok := sess.(interface{ Close() error }); ok {
		_ = c.Close()
	}

	// 重开：恢复产物必须通过 tool-pairing 校验。
	sess2, err := store.Open(ctx, id)
	if err != nil {
		return err
	}
	surface, err := sess2.Surface(ctx)
	if err != nil {
		return err
	}
	events, err := sess2.Events(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("== ③ 崩溃恢复：半截 tool call → 重新 Open ==")
	fmt.Printf("surface=%d 条消息（call 已有配对 result，合法续跑）：\n", len(surface))
	for i, m := range surface {
		fmt.Printf("  [%d] %s: %s\n", i, m.Role, preview(msgLabel(m)))
	}
	// 合成事件写回日志：model-visible means logged。
	synthetic := 0
	for _, ev := range events {
		if ev.Type == session.EventToolResult && strings.Contains(string(ev.Data), "interrupted") {
			synthetic++
		}
	}
	fmt.Printf("raw events=%d（含合成闭合 result %d 条——它真实写进了日志）\n", len(events), synthetic)
	return nil
}

// demoCompaction：长会话触发压力 → Compact → Surface 换成 checkpoint
// 摘要；原始事件完整保留（「压缩是事务不是删除」）。
func demoCompaction(ctx context.Context, root string) error {
	store, err := session.NewJSONLStore(root)
	if err != nil {
		return err
	}
	sess, err := store.Create(ctx, session.SessionHeader{})
	if err != nil {
		return err
	}
	// 造一个超预算的长 surface（5 个满长 turn）。
	for i := 0; i < 5; i++ {
		if _, err := sess.Append(ctx, draftUser(fmt.Sprintf("question %d: %s", i, strings.Repeat("detail ", 120)))); err != nil {
			return err
		}
		if _, err := sess.Append(ctx, draftAssistant(llm.Text(fmt.Sprintf("answer %d: %s", i, strings.Repeat("analysis ", 120))))); err != nil {
			return err
		}
	}
	surface, err := sess.Surface(ctx)
	if err != nil {
		return err
	}

	meter := compaction.CharMeter{} // rune/4 估算 token；精确计数由宿主提供
	if !compaction.Pressure(meter, surface, 1000) {
		return fmt.Errorf("demo: expected pressure over 1000 tokens")
	}

	// 八步压缩事务（Engine 可失败可取消；checkpoint 是 SurfaceReplace，
	// 不得伪装 message.user）。
	report, err := compaction.Compact(ctx, sess, compaction.Options{
		Engine:    &compaction.LLMSummarizer{Model: llm.NewScripted(llm.Resp("（摘要）前五轮 Q&A：用户逐步追问 detail/analysis，无未决工具调用。")), ModelName: "scripted"},
		Meter:     meter,
		ModelName: "scripted",
	})
	if err != nil {
		return err
	}
	after, err := sess.Surface(ctx)
	if err != nil {
		return err
	}
	raw, err := sess.Events(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("== ④ compaction：Pressure → Compact（§9.1 事务）==")
	fmt.Printf("压缩前 surface=%d 条（超 1000 token 预算）\n", len(surface))
	fmt.Printf("压缩后 surface=%d 条：\n", len(after))
	for i, m := range after {
		fmt.Printf("  [%d] %s: %s\n", i, m.Role, preview(m.Text()))
	}
	fmt.Printf("raw log events=%d（原始事件一个都没删）\n", len(raw))
	fmt.Printf("checkpoint seq=%d，Replaced 窗口=%d 条 source refs 可溯源\n",
		report.CheckpointSeq, len(report.Replaced))

	// 持久化验证：checkpoint 写入抬 FormatVersion=2，重开仍是摘要形态。
	id := sess.Header().SessionID
	if err := sess.Flush(ctx); err != nil {
		return err
	}
	if c, ok := sess.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	sess2, err := store.Open(ctx, id)
	if err != nil {
		return err
	}
	reopened, err := sess2.Surface(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("重开后 surface=%d 条（压缩形态持久化，旧 reader 拒开 FormatVersion=2）\n", len(reopened))
	return nil
}

// renderSurface 把 surface 转成可比对的文本形态。
func renderSurface(msgs []*llm.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s|%s\n", m.Role, m.Text())
	}
	return b.String()
}

// msgLabel 取消息的可读标签：text part 直取；tool call/result 从结构化
// 字段组装（Message.Text() 只聚合 text part，不含 tool 结构）。
func msgLabel(m *llm.Message) string {
	if s := strings.TrimSpace(m.Text()); s != "" {
		return s
	}
	for _, p := range m.Parts {
		switch p.Kind {
		case llm.PartToolCall:
			if p.ToolCallValue != nil {
				return fmt.Sprintf("call %s(%s)", p.ToolCallValue.Name, string(p.ToolCallValue.Arguments))
			}
		case llm.PartToolResult:
			if p.ToolResultValue != nil {
				for _, c := range p.ToolResultValue.Content {
					if c.Text != "" {
						return c.Text
					}
				}
			}
		}
	}
	return "(structured)"
}

func preview(s string) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 48 {
		return string([]rune(s)[:48]) + "…"
	}
	return s
}
