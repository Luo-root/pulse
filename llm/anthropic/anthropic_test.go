package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// ---- 测试基建 ----

// newTest 起一个 httptest 服务并返回指向它的 Messages 模型。
func newTest(t *testing.T, handler http.HandlerFunc) llm.ChatModel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, err := New(llm.Config{
		Provider: ProviderAnthropic, Model: "claude-test",
		APIKey: "test-key", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func readJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	return body
}

// writeSSE 按条目写 Anthropic SSE：每条事件 = event 行 + data 行。
// SDK 的流解码器依赖 event 行路由类型；Anthropic 流没有 [DONE] 哨兵，
// message_stop 即最后一条。
func writeSSE(t *testing.T, w http.ResponseWriter, pairs ...[2]string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("response 不支持 flush")
	}
	for _, p := range pairs {
		_, _ = w.Write([]byte("event: " + p[0] + "\ndata: " + p[1] + "\n\n"))
		flusher.Flush()
	}
}

func ev(kind string, data string) [2]string { return [2]string{kind, data} }

func mustStream(t *testing.T, m llm.ChatModel, req *llm.GenerateRequest) <-chan llm.StreamEvent {
	t.Helper()
	ch, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return ch
}

// collect 读干事件 channel；允许 ctx 已取消（取消测例专用）。
func collect(t *testing.T, ctx context.Context, ch <-chan llm.StreamEvent) []llm.StreamEvent {
	t.Helper()
	var evs []llm.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
		if ev.Kind == llm.EventDone || ev.Kind == llm.EventError {
			break
		}
	}
	for range ch {
		t.Fatal("结束事件后仍有残留事件")
	}
	return evs
}

func wantKind(t *testing.T, evs []llm.StreamEvent, i int, kind llm.StreamEventKind) llm.StreamEvent {
	t.Helper()
	if i >= len(evs) {
		t.Fatalf("事件不足：期望第 %d 个是 %s，实际共 %d 个", i, kind, len(evs))
	}
	if evs[i].Kind != kind {
		t.Fatalf("第 %d 个事件 = %s，期望 %s", i, evs[i].Kind, kind)
	}
	return evs[i]
}

const okResp = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}`

// ---- 线格式 ----

func TestWireFormat(t *testing.T) {
	var gotPath, gotAuth, gotVersion string
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		body := readJSON(t, r)
		// 必填字段
		if body["model"] != "claude-test" {
			t.Fatalf("model = %v", body["model"])
		}
		if body["max_tokens"] != float64(256) {
			t.Fatalf("max_tokens = %v", body["max_tokens"])
		}
		// system 进顶层参数，不在 messages 里
		sys, _ := body["system"].([]any)
		if len(sys) != 1 {
			t.Fatalf("system 应为单块数组: %v", body["system"])
		}
		s0, _ := sys[0].(map[string]any)
		if s0["type"] != "text" || s0["text"] != "be brief" {
			t.Fatalf("system 不符: %v", s0)
		}
		// 消息：user 文本 / assistant 工具调用 / user 工具结果
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("messages 数量 = %d，期望 3: %v", len(msgs), body["messages"])
		}
		m0, _ := msgs[0].(map[string]any)
		if m0["role"] != "user" {
			t.Fatalf("第 1 条应为 user: %v", m0)
		}
		m1, _ := msgs[1].(map[string]any)
		if m1["role"] != "assistant" {
			t.Fatalf("第 2 条应为 assistant: %v", m1)
		}
		ac, _ := m1["content"].([]any)
		tu, _ := ac[0].(map[string]any)
		input, _ := tu["input"].(map[string]any)
		if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "echo" ||
			input == nil || input["x"] != float64(1) {
			t.Fatalf("assistant tool_use 不符: %v", tu)
		}
		m2, _ := msgs[2].(map[string]any)
		if m2["role"] != "user" {
			t.Fatalf("第 3 条应为 user(tool_result): %v", m2)
		}
		tc, _ := m2["content"].([]any)
		tr, _ := tc[0].(map[string]any)
		if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["is_error"] != true {
			t.Fatalf("tool_result 不符: %v", tr)
		}
		trc, _ := tr["content"].([]any)
		trt, _ := trc[0].(map[string]any)
		if trt["text"] != "boom happened" {
			t.Fatalf("tool_result 内容不符: %v", trt)
		}
		// 工具声明：schema 扁平映射——properties/required 各归其位
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools 数量 = %d", len(tools))
		}
		t0, _ := tools[0].(map[string]any)
		if t0["name"] != "echo" {
			t.Fatalf("工具名不符: %v", t0)
		}
		schema, _ := t0["input_schema"].(map[string]any)
		if schema["type"] != "object" {
			t.Fatalf("input_schema.type = %v", schema["type"])
		}
		props, _ := schema["properties"].(map[string]any)
		if props == nil || props["city"] == nil {
			t.Fatalf("input_schema.properties 应为字段表: %v", schema)
		}
		if _, nested := props["properties"]; nested {
			t.Fatal("整份 schema 被塞进了 properties（套层 bug）")
		}
		required, _ := schema["required"].([]any)
		if len(required) != 1 || required[0] != "city" {
			t.Fatalf("input_schema.required = %v", schema["required"])
		}
		// tool_choice 线上值
		tcm, _ := body["tool_choice"].(map[string]any)
		if tcm["type"] != "auto" {
			t.Fatalf("tool_choice = %v", body["tool_choice"])
		}
		// 采样参数与 stop
		if body["temperature"] != 0.5 {
			t.Fatalf("temperature = %v", body["temperature"])
		}
		ss, _ := body["stop_sequences"].([]any)
		if len(ss) != 1 || ss[0] != "END" {
			t.Fatalf("stop_sequences = %v", body["stop_sequences"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	})

	temp := 0.5
	maxTok := 256
	req := llm.NewRequest(
		llm.System("be brief"),
		llm.UserText("hi"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
			llm.Call(llm.ToolCall{ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}),
		}},
		&llm.Message{Role: llm.RoleTool, Parts: []llm.Part{
			llm.ResultParts("call_1", true, llm.Text("boom happened")),
		}},
	)
	req.Tools = []llm.ToolDef{{
		Name:        "echo",
		Description: "echo it",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
	req.ToolChoice = &llm.ToolChoice{Mode: llm.ToolAuto}
	req.Temperature = &temp
	req.MaxTokens = &maxTok
	req.StopSequences = []string{"END"}

	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "test-key" {
		t.Fatalf("x-api-key = %q", gotAuth)
	}
	if gotVersion == "" {
		t.Fatal("anthropic-version 头缺失")
	}
	if resp.FinishReason != llm.FinishStop {
		t.Fatalf("FinishReason = %s", resp.FinishReason)
	}
	if resp.Message.Text() != "hi there" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.CachedInputTokens != 3 {
		t.Fatalf("Usage 不符: %+v", resp.Usage)
	}
}

func TestMaxTokensRequired(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("不应发出请求")
	})
	req := llm.NewRequest(llm.UserText("hi"))
	_, err := m.Generate(context.Background(), req)
	if llm.KindOf(err) != llm.ErrBadRequest {
		t.Fatalf("缺 MaxTokens 应显式 bad_request，得到 %v", err)
	}
}

func TestImageAndDocument(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		user, _ := msgs[0].(map[string]any)
		parts, _ := user["content"].([]any)
		if len(parts) != 3 {
			t.Fatalf("文本+图+PDF 应为 3 块: %v", user["content"])
		}
		p1, _ := parts[1].(map[string]any)
		if p1["type"] != "image" {
			t.Fatalf("第 2 块应为 image: %v", p1)
		}
		src, _ := p1["source"].(map[string]any)
		if src["type"] != "url" || src["url"] != "https://example.com/cat.png" {
			t.Fatalf("image source 不符: %v", src)
		}
		p2, _ := parts[2].(map[string]any)
		if p2["type"] != "document" {
			t.Fatalf("第 3 块应为 document: %v", p2)
		}
		dsrc, _ := p2["source"].(map[string]any)
		if dsrc["type"] != "base64" || dsrc["media_type"] != "application/pdf" {
			t.Fatalf("document source 不符: %v", dsrc)
		}
		want := base64.StdEncoding.EncodeToString([]byte("%PDF"))
		if dsrc["data"] != want {
			t.Fatalf("PDF data 不符: %v", dsrc["data"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Text("看这两份材料"),
		llm.ImageURL("https://example.com/cat.png", "image/png"),
		llm.Media("application/pdf", []byte("%PDF")),
	}})
	maxTok := 64
	req.MaxTokens = &maxTok
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "hi there" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestUnsupportedModalitiesRejected(t *testing.T) {
	reject := func(t *testing.T, m llm.ChatModel, mediaType string) {
		t.Helper()
		req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
			llm.Media(mediaType, []byte("xx")),
		}})
		maxTok := 32
		req.MaxTokens = &maxTok
		if _, err := m.Generate(context.Background(), req); llm.KindOf(err) != llm.ErrBadRequest {
			t.Fatalf("mediaType %q: kind = %v，期望 bad_request", mediaType, llm.KindOf(err))
		}
	}
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("不应发出请求") })
	reject(t, m, "audio/wav")
	reject(t, m, "video/mp4")
	reject(t, m, "application/zip")

	// Audio 输出同样显式拒绝。
	req := llm.NewRequest(llm.UserText("tts"))
	req.Audio = &llm.AudioOutput{Voice: "v"}
	maxTok := 32
	req.MaxTokens = &maxTok
	if _, err := m.Generate(context.Background(), req); llm.KindOf(err) != llm.ErrBadRequest {
		t.Fatalf("Audio 输出应 bad_request，得到 %v", err)
	}
}

func TestResponseFormatRejected(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("不应发出请求")
	})
	req := llm.NewRequest(llm.UserText("hi"))
	rf := llm.FormatJSONSchema
	req.ResponseFormat = &llm.ResponseFormat{Type: rf}
	maxTok := 32
	req.MaxTokens = &maxTok
	if _, err := m.Generate(context.Background(), req); llm.KindOf(err) != llm.ErrBadRequest {
		t.Fatalf("ResponseFormat 应 bad_request，得到 %v", err)
	}
}

// ---- 流式 ----

func TestStream(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		if body["stream"] != true {
			t.Fatalf("stream 应为 true")
		}
		writeSSE(t, w,
			ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":2}}}`),
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
			ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"get_weather"}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"ci"}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"ty\":\"SF\"}"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":10,"output_tokens":9,"cache_read_input_tokens":2}}`),
			ev("message_stop", `{"type":"message_stop"}`),
		)
	})
	req := llm.NewRequest(llm.UserText("hi"))
	maxTok := 128
	req.MaxTokens = &maxTok

	evs := collect(t, context.Background(), mustStream(t, m, req))
	done := wantKind(t, evs, len(evs)-1, llm.EventDone)

	// 事件序列：2 文本增量 + begin + 2 参数增量
	if evs[0].Kind != llm.EventTextDelta || evs[1].Kind != llm.EventTextDelta {
		t.Fatalf("前两个事件应为文本增量: %+v", evs[:2])
	}
	begin := wantKind(t, evs, 2, llm.EventToolCallBegin)
	if begin.Index != 1 || begin.CallID != "call_1" || begin.ToolName != "get_weather" {
		t.Fatalf("begin 不符: %+v", begin)
	}
	wantKind(t, evs, 3, llm.EventToolCallDelta)
	wantKind(t, evs, 4, llm.EventToolCallDelta)

	if done.Response.FinishReason != llm.FinishToolCalls {
		t.Fatalf("FinishReason = %s", done.Response.FinishReason)
	}
	msg := done.Response.Message
	if msg.Text() != "Hello world" {
		t.Fatalf("聚合文本 = %q", msg.Text())
	}
	calls := msg.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"SF"}` {
		t.Fatalf("聚合工具调用不符: %+v", calls)
	}
	if done.Response.Usage.InputTokens != 10 || done.Response.Usage.OutputTokens != 9 || done.Response.Usage.CachedInputTokens != 2 {
		t.Fatalf("Usage 不符: %+v", done.Response.Usage)
	}
}

func TestStreamReasoning(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
			ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`),
			ev("message_stop", `{"type":"message_stop"}`),
		)
	})
	req := llm.NewRequest(llm.UserText("hi"))
	maxTok := 128
	req.MaxTokens = &maxTok
	evs := collect(t, context.Background(), mustStream(t, m, req))
	rd := wantKind(t, evs, 0, llm.EventReasoningDelta)
	if rd.Text != "pondering" {
		t.Fatalf("reasoning delta = %q", rd.Text)
	}
	wantKind(t, evs, 1, llm.EventTextDelta)
	done := wantKind(t, evs, len(evs)-1, llm.EventDone)
	if done.Response.Message.ReasoningText() != "pondering" {
		t.Fatalf("聚合 reasoning = %q", done.Response.Message.ReasoningText())
	}
}

func TestConsecutiveToolResultsMerged(t *testing.T) {
	// 一次模型返回多个 tool_call 时 loop 会 append 多条 RoleTool；
	// Anthropic 要求 user/assistant 严格交替，相邻 tool_result 必须
	// 合并进同一条 user 轮。
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("相邻 RoleTool 应合并：messages = %d，期望 3（user/assistant/user）: %v",
				len(msgs), body["messages"])
		}
		last, _ := msgs[2].(map[string]any)
		if last["role"] != "user" {
			t.Fatalf("第 3 条应为 user: %v", last)
		}
		content, _ := last["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("合并后 tool_result 数 = %d，期望 2: %v", len(content), last["content"])
		}
		r0, _ := content[0].(map[string]any)
		r1, _ := content[1].(map[string]any)
		if r0["tool_use_id"] != "call_1" || r1["tool_use_id"] != "call_2" {
			t.Fatalf("tool_result 顺序不符: %v %v", r0, r1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	})
	req := llm.NewRequest(
		llm.UserText("hi"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
			llm.Call(llm.ToolCall{ID: "call_1", Name: "a", Arguments: json.RawMessage(`{}`)}),
			llm.Call(llm.ToolCall{ID: "call_2", Name: "b", Arguments: json.RawMessage(`{}`)}),
		}},
		llm.ToolMessage("call_1", "r1"),
		llm.ToolMessage("call_2", "r2"),
	)
	maxTok := 64
	req.MaxTokens = &maxTok
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestGenerateToolUseAndThinking(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		resp := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"let me check","signature":"sig"},{"type":"tool_use","id":"call_9","name":"get_weather","input":{"city":"SF"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":4}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	resp, err := m.Generate(context.Background(), funcReq())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.FinishReason != llm.FinishToolCalls {
		t.Fatalf("stop_reason=tool_use 应映射 FinishToolCalls，得到 %s", resp.FinishReason)
	}
	if resp.Message.ReasoningText() != "let me check" {
		t.Fatalf("thinking 应映射为 reasoning: %q", resp.Message.ReasoningText())
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"SF"}` {
		t.Fatalf("tool_use 映射不符: %+v", calls)
	}
}

func TestCustomImagePart(t *testing.T) {
	// PartCustom(image/*) 与 PartImage 同路：都映射官方 image 块。
	png := []byte{0x89, 'P', 'N', 'G'}
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		user, _ := msgs[0].(map[string]any)
		parts, _ := user["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("应有两个 image 块: %v", user["content"])
		}
		for i, anyPart := range parts {
			p, _ := anyPart.(map[string]any)
			if p["type"] != "image" {
				t.Fatalf("第 %d 块应为 image: %v", i+1, p)
			}
			src, _ := p["source"].(map[string]any)
			if src["type"] != "base64" || src["media_type"] != "image/png" {
				t.Fatalf("image source 不符: %v", src)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.ImageData("image/png", png),
		llm.Media("image/png", png),
	}})
	maxTok := 64
	req.MaxTokens = &maxTok
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestMediaSources(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		user, _ := msgs[0].(map[string]any)
		parts, _ := user["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("应为图 base64 + PDF URL 两块: %v", user["content"])
		}
		p0, _ := parts[0].(map[string]any)
		src0, _ := p0["source"].(map[string]any)
		want := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
		if src0["type"] != "base64" || src0["data"] != want {
			t.Fatalf("图 base64 source 不符: %v", src0)
		}
		p1, _ := parts[1].(map[string]any)
		if p1["type"] != "document" {
			t.Fatalf("第 2 块应为 document: %v", p1)
		}
		src1, _ := p1["source"].(map[string]any)
		if src1["type"] != "url" || src1["url"] != "https://example.com/doc.pdf" {
			t.Fatalf("PDF URL source 不符: %v", src1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.ImageData("image/png", []byte{0x89, 'P', 'N', 'G'}),
		llm.MediaURL("application/pdf", "https://example.com/doc.pdf"),
	}})
	maxTok := 64
	req.MaxTokens = &maxTok
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestHeadersOption(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	}))
	defer srv.Close()
	m, err := New(llm.Config{
		Provider: ProviderAnthropic, Model: "claude-test", APIKey: "k", BaseURL: srv.URL,
		Options: map[string]any{"headers": map[string]any{"X-Trace": "t-1"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Generate(context.Background(), funcReq()); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "t-1" {
		t.Fatalf("headers 选项未透传: %q", got)
	}
}

func TestStreamEarlyEnd(t *testing.T) {
	// 流在 message_stop 前静默结束：必须以 EventError(provider) 收尾，
	// 不能当成正常完成。
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			ev("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		)
	})
	evs := collect(t, context.Background(), mustStream(t, m, funcReq()))
	if len(evs) == 0 || evs[len(evs)-1].Kind != llm.EventError {
		t.Fatalf("期望 EventError 收尾: %+v", evs)
	}
	if llm.KindOf(evs[len(evs)-1].Err) != llm.ErrProvider {
		t.Fatalf("kind = %s，期望 provider", llm.KindOf(evs[len(evs)-1].Err))
	}
}

func TestStreamErrorEvent(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	})
	evs := collect(t, context.Background(), mustStream(t, m, funcReq()))
	if len(evs) == 0 || evs[len(evs)-1].Kind != llm.EventError {
		t.Fatalf("期望 EventError 收尾: %+v", evs)
	}
	if llm.KindOf(evs[len(evs)-1].Err) != llm.ErrProvider {
		t.Fatalf("overloaded 应映射 provider，得到 %v", llm.KindOf(evs[len(evs)-1].Err))
	}
}

func TestStreamCancel(t *testing.T) {
	started := make(chan struct{})
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"c","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n"))
		flusher.Flush()
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Stream(ctx, funcReq())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	<-started
	cancel()
	var evs []llm.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	if len(evs) == 0 || evs[len(evs)-1].Kind != llm.EventError {
		t.Fatalf("取消后最后事件应为 EventError: %+v", evs)
	}
	if llm.KindOf(evs[len(evs)-1].Err) != llm.ErrCanceled {
		t.Fatalf("kind = %s，期望 canceled", llm.KindOf(evs[len(evs)-1].Err))
	}
}

func funcReq() *llm.GenerateRequest {
	req := llm.NewRequest(llm.UserText("hi"))
	maxTok := 64
	req.MaxTokens = &maxTok
	return req
}

// ---- 错误分类 ----

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   llm.ErrKind
		code   int
	}{
		{401, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, llm.ErrAuth, 401},
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, llm.ErrRateLimit, 429},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 200000 tokens > 180000 maximum"}}`, llm.ErrContextLength, 400},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: field required"}}`, llm.ErrBadRequest, 400},
		{404, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, llm.ErrNoModel, 404},
		{500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, llm.ErrProvider, 500},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, llm.ErrProvider, 529},
	}
	for _, tc := range cases {
		m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := m.Generate(context.Background(), funcReq())
		if err == nil {
			t.Fatalf("status %d: 期望错误", tc.status)
		}
		if kind := llm.KindOf(err); kind != tc.kind {
			t.Fatalf("status %d: kind = %s，期望 %s（err=%v）", tc.status, kind, tc.kind, err)
		}
		var le *llm.Error
		if !errors.As(err, &le) || le.StatusCode != tc.code {
			t.Fatalf("status %d: StatusCode 未透传: %+v", tc.status, le)
		}
	}
}

// ---- 装配 ----

func TestSamplingAndThinking(t *testing.T) {
	m := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		if body["top_k"] != float64(40) {
			t.Fatalf("top_k = %v", body["top_k"])
		}
		thinking, _ := body["thinking"].(map[string]any)
		if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(2048) {
			t.Fatalf("thinking 不符: %v", body["thinking"])
		}
		if body["service_tier"] != "standard_only" {
			t.Fatalf("service_tier = %v", body["service_tier"])
		}
		// 带 thinking 的响应：thinking 块映射为 reasoning。
		resp := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	req := llm.NewRequest(llm.UserText("hi"))
	topK := 40
	req.TopK = &topK
	maxTok := 512
	req.MaxTokens = &maxTok                                                   // anthropic 必填；thinking budget 须 < max_tokens
	req.Reasoning = &llm.ReasoningOptions{BudgetTokens: 2048, Effort: "high"} // Effort 在 anthropic 被忽略
	req.Options = map[string]any{"service_tier": "standard_only"}
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.ReasoningText() != "hmm" {
		t.Fatalf("reasoning = %q", resp.Message.ReasoningText())
	}
}

func TestRegistryIntegration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okResp))
	}))
	defer srv.Close()

	ctx := kernel.New()
	reg := llm.NewRegistry(ctx)
	if err := Register(ctx, reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Declare("main", llm.Config{
		Provider: ProviderAnthropic, Model: "claude-test", APIKey: "k", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	model, err := reg.Open("main")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	resp, err := model.Generate(context.Background(), funcReq())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "hi there" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestConfigValidation(t *testing.T) {
	_, err := New(llm.Config{Provider: ProviderAnthropic, Model: "m"})
	if llm.KindOf(err) != llm.ErrAuth {
		t.Fatalf("缺 APIKey 应报 auth，得到 %v", err)
	}
}

// ---- 真机冒烟（环境变量门控，凭据不入库）----

func TestLive(t *testing.T) {
	key := os.Getenv("PULSE_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("PULSE_ANTHROPIC_API_KEY 未设置，跳过真机冒烟")
	}
	baseURL := os.Getenv("PULSE_ANTHROPIC_BASE_URL")
	model := os.Getenv("PULSE_ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-sonnet-4-5"
	}
	m, err := New(llm.Config{
		Provider: ProviderAnthropic, Model: model, APIKey: key, BaseURL: baseURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Run("text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		resp, err := m.Generate(ctx, funcReqWith("用一句话介绍 Go 语言"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if strings.TrimSpace(resp.Message.Text()) == "" {
			t.Fatal("空文本")
		}
		t.Logf("text=%q tokens=%d/%d", truncate(resp.Message.Text(), 60),
			resp.Usage.InputTokens, resp.Usage.OutputTokens)
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		evs := collect(t, ctx, mustStream(t, m, funcReqWith("数到三，只输出数字")))
		var sb strings.Builder
		for _, ev := range evs {
			if ev.Kind == llm.EventTextDelta {
				sb.WriteString(ev.Text)
			}
		}
		if sb.Len() == 0 {
			t.Fatal("流式未收到文本增量")
		}
		t.Logf("stream=%q", truncate(sb.String(), 60))
	})

	t.Run("tools", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req := funcReqWith("北京现在天气如何？请调用工具查询，不要直接回答")
		req.Tools = []llm.ToolDef{{
			Name:        "get_weather",
			Description: "查询城市天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}
		req.ToolChoice = &llm.ToolChoice{Mode: llm.ToolAny}
		resp, err := m.Generate(ctx, req)
		if err != nil {
			t.Fatalf("Generate(tool): %v", err)
		}
		if len(resp.Message.ToolCalls()) == 0 {
			t.Fatalf("期望工具调用，得到: %s", resp.Message.Text())
		}
		t.Logf("tool=%s args=%s", resp.Message.ToolCalls()[0].Name, resp.Message.ToolCalls()[0].Arguments)
	})

	t.Run("image", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
			llm.Text("这张图里有什么？用一句话回答"),
			llm.ImageURL("https://upload.wikimedia.org/wikipedia/commons/thumb/3/3a/Cat03.jpg/240px-Cat03.jpg", "image/jpeg"),
		}})
		maxTok := 256
		req.MaxTokens = &maxTok
		resp, err := m.Generate(ctx, req)
		if err != nil {
			t.Fatalf("Generate(image): %v", err)
		}
		if strings.TrimSpace(resp.Message.Text()) == "" {
			t.Fatal("图像输入返回空文本")
		}
		t.Logf("image=%q", truncate(resp.Message.Text(), 60))
	})
}

func funcReqWith(text string) *llm.GenerateRequest {
	req := llm.NewRequest(llm.UserText(text))
	maxTok := 1024
	req.MaxTokens = &maxTok
	return req
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
