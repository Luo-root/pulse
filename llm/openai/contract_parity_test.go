package openai

// #246 / #247 的回归用例：跨家语义缺口（工具失败的表达、拒答文本）与两变体
// 不对称（system 非文本块、Message.Name、logprobs 冲突）。基线口径见
// README.md「Tool-result failure marking, Name and refusals」一节。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
)

// toolMsg 构造一条带工具结果的 tool 消息（isError 决定是否失败）。
func toolMsg(id, text string, isError bool) *llm.Message {
	return &llm.Message{Role: llm.RoleTool, Parts: []llm.Part{llm.ResultParts(id, isError, llm.Text(text))}}
}

// completionsOKBody 是最小的非流式 completions 响应体。
const completionsOKBody = `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

// responsesOKBody 是最小的非流式 responses 响应体。
const responsesOKBody = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,` +
	`"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"completed",` +
	`"content":[{"type":"output_text","text":"ok","annotations":[]}]}],` +
	`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`

// writeBody 写一个 JSON 响应体（桩服务用）。
func writeBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// TestCompletionsToolResultIsErrorMarked：#246——OpenAI 两个变体都没有工具结果的
// 错误字段，IsError 只能以统一文本前缀表达（对照 Anthropic 的原生 is_error）。
func TestCompletionsToolResultIsErrorMarked(t *testing.T) {
	var got map[string]any
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		got = readJSON(t, r)
		writeBody(w, completionsOKBody)
	})
	req := llm.NewRequest(llm.UserText("hi"))
	req.Messages = append(req.Messages,
		toolMsg("call_1", "boom: tool failed", true),
		toolMsg("call_2", "all good", false),
		toolMsg("call_3", "", true),
	)
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 条数 = %d，want 4: %v", len(msgs), msgs)
	}
	want := []string{"hi", "[tool error] boom: tool failed", "all good", "[tool error]"}
	for i, w := range want {
		msg, _ := msgs[i].(map[string]any)
		if msg["content"] != w {
			t.Fatalf("messages[%d].content = %v, want %q", i, msg["content"], w)
		}
	}
}

// TestResponsesToolResultIsErrorMarked：同上，Responses 变体的 function_call_output。
func TestResponsesToolResultIsErrorMarked(t *testing.T) {
	var got map[string]any
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		got = readJSON(t, r)
		writeBody(w, responsesOKBody)
	})
	req := llm.NewRequest(llm.UserText("hi"))
	req.Messages = append(req.Messages, toolMsg("call_1", "boom", true), toolMsg("call_2", "fine", false))
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	items, _ := got["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("input 条数 = %d，want 3: %v", len(items), items)
	}
	want := []string{"[tool error] boom", "fine"}
	for i, w := range want {
		item, _ := items[i+1].(map[string]any)
		if item["type"] != "function_call_output" || item["output"] != w {
			t.Fatalf("input[%d] = %v, want function_call_output/%q", i+1, item, w)
		}
	}
}

// TestCompletionsRefusalMapped：#247-1——拒答时 message.refusal 不能丢，否则上层
// 拿到的是「空回复」而不是「拒答文本」（Responses 变体早已映射）。
func TestCompletionsRefusalMapped(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeBody(w, `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[`+
			`{"index":0,"message":{"role":"assistant","content":"","refusal":"I cannot help with that"},"finish_reason":"stop"}]}`)
	})
	resp, err := m.Generate(context.Background(), llm.NewRequest(llm.UserText("q")))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if txt := resp.Message.Text(); txt != "I cannot help with that" {
		t.Fatalf("拒答文本 = %q（修复前是空串）", txt)
	}
}

// TestCompletionsStreamRefusalDelta：#247-1 的流式一面——拒答以 delta.refusal 分片
// 到达，聚合进 done 的文本块并作为 text_delta 下推。
func TestCompletionsStreamRefusalDelta(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			chunk(`"refusal":"I cannot help"`, `null`),
			chunk(`"refusal":" with that"`, `null`),
			chunk(``, `"stop"`),
		)
	})
	evs := collect(t, context.Background(), mustStream(t, m, llm.NewRequest(llm.UserText("q"))))
	if d := wantKind(t, evs, 0, llm.EventTextDelta); d.Text != "I cannot help" {
		t.Fatalf("首个 delta = %q", d.Text)
	}
	wantKind(t, evs, 1, llm.EventTextDelta)
	done := wantKind(t, evs, 2, llm.EventDone)
	if got := done.Response.Message.Text(); got != "I cannot help with that" {
		t.Fatalf("聚合拒答文本 = %q", got)
	}
}

// TestSystemNonTextPartRejected：#247-2——两变体的系统提示都是字符串字段，非文本块
// 必须显式报错（原来被 JoinText 静默削掉），且请求不发出。
func TestSystemNonTextPartRejected(t *testing.T) {
	adapters := []struct {
		name  string
		build func(t *testing.T) llm.ChatModel
	}{
		{"completions", func(t *testing.T) llm.ChatModel {
			return newCompletionsTest(t, func(http.ResponseWriter, *http.Request) {
				t.Fatal("被拒的请求不应发出")
			})
		}},
		{"responses", func(t *testing.T) llm.ChatModel {
			return newResponsesTest(t, func(http.ResponseWriter, *http.Request) {
				t.Fatal("被拒的请求不应发出")
			})
		}},
	}
	for _, ad := range adapters {
		t.Run(ad.name, func(t *testing.T) {
			sys := &llm.Message{Role: llm.RoleSystem, Parts: []llm.Part{
				llm.Text("be nice"),
				llm.ImageURL("https://example.com/a.png", "image/png"),
			}}
			_, err := ad.build(t).Generate(context.Background(), llm.NewRequest(sys, llm.UserText("hi")))
			if llm.KindOf(err) != llm.ErrBadRequest {
				t.Fatalf("kind = %v, want ErrBadRequest (err=%v)", llm.KindOf(err), err)
			}
			if !strings.Contains(err.Error(), "system") || !strings.Contains(err.Error(), "image") {
				t.Fatalf("err = %v, want 角色与块类型都点到", err)
			}
		})
	}
}

// TestCompletionsMessageNameMapped：#247-3——OpenAI 的 system / user / assistant
// 消息有 name 字段（tool 消息没有），Message.Name 应落下去而不是被忽略。
func TestCompletionsMessageNameMapped(t *testing.T) {
	var got map[string]any
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		got = readJSON(t, r)
		writeBody(w, completionsOKBody)
	})
	tm := toolMsg("call_1", "done", false)
	tm.Name = "callee" // 线格式的 tool 消息没有 name 字段，这一档不下发
	req := llm.NewRequest(
		&llm.Message{Role: llm.RoleSystem, Parts: []llm.Part{llm.Text("be nice")}, Name: "sys"},
		&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("hi")}, Name: "alice"},
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("hello")}, Name: "bob"},
		&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("plain")}},
		tm,
	)
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	want := []any{"sys", "alice", "bob", nil, nil}
	if len(msgs) != len(want) {
		t.Fatalf("messages 条数 = %d，want %d: %v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		msg, _ := msgs[i].(map[string]any)
		if msg["name"] != w {
			t.Fatalf("messages[%d].name = %v, want %v", i, msg["name"], w)
		}
	}
}

// TestResponsesMessageNameIgnored：#247-3 的另一档——Responses 线格式没有 name
// 字段，口径是「忽略、不报错」（与 Message.Name godoc 一致），请求体不应带它。
func TestResponsesMessageNameIgnored(t *testing.T) {
	var raw string
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		writeBody(w, responsesOKBody)
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("hi")}, Name: "alice"})
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(raw, `"name"`) {
		t.Fatalf("Responses 线格式没有 name 字段，不应下发: %s", raw)
	}
}

// TestCompletionsLogprobsConflictRejected：#247-4——completions 原来把显式
// Logprobs=false 静默改写成 true，与 Responses 变体的 ErrBadRequest 不对称。
func TestCompletionsLogprobsConflictRejected(t *testing.T) {
	m := newCompletionsTest(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("不应发出请求")
	})
	no := false
	top := 5
	req := llm.NewRequest(llm.UserText("hi"))
	req.Output = &llm.OutputOptions{Logprobs: &no, TopLogprobs: &top}
	if _, err := m.Generate(context.Background(), req); llm.KindOf(err) != llm.ErrBadRequest {
		t.Fatalf("Logprobs=false + TopLogprobs 应 bad_request，得到 %v", err)
	}
}
