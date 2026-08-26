package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

func TestMain(m *testing.M) {
	loadDotEnv()
	os.Exit(m.Run())
}

// loadDotEnv 从仓库根的 .env 读入尚未设置的环境变量。
// .env 已被 gitignore，专供本机真机冒烟；解析失败静默忽略。
func loadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 6; i++ {
		p := dir + string(os.PathSeparator) + ".env"
		data, err := os.ReadFile(p)
		if err == nil {
			applyDotEnv(data)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func applyDotEnv(data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// ---- 测试基建 ----

// newCompletionsTest 起一个 httptest 服务并返回指向它的 Completions 模型。
func newCompletionsTest(t *testing.T, handler http.HandlerFunc) llm.ChatModel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, err := NewCompletions(llm.Config{
		Provider: ProviderCompletions, Model: "gpt-test",
		APIKey: "test-key", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewCompletions: %v", err)
	}
	return m
}

// newResponsesTest 同上，Responses 变体。
func newResponsesTest(t *testing.T, handler http.HandlerFunc) llm.ChatModel {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	m, err := NewResponses(llm.Config{
		Provider: ProviderResponses, Model: "gpt-test",
		APIKey: "test-key", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("NewResponses: %v", err)
	}
	return m
}

// readJSON 读取请求体并解析为 map，供线格式断言。
func readJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	return body
}

// writeSSE 按行写 SSE 事件并以 [DONE] 收尾。
func writeSSE(t *testing.T, w http.ResponseWriter, events ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("response 不支持 flush")
	}
	for _, ev := range events {
		_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		flusher.Flush()
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// collect 读干事件 channel，返回全部事件。
func collect(t *testing.T, ctx context.Context, ch <-chan llm.StreamEvent) []llm.StreamEvent {
	t.Helper()
	var evs []llm.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
		if ev.Kind == llm.EventDone || ev.Kind == llm.EventError {
			break
		}
	}
	// channel 已关（EventDone/Error 后 close）；确认没有残留事件。
	for range ch {
		t.Fatal("结束事件后仍有残留事件")
	}
	if ctx.Err() != nil {
		t.Fatalf("ctx 意外取消: %v", ctx.Err())
	}
	return evs
}

// mustStream 发起流式请求，失败即 Fatal。
func mustStream(t *testing.T, m llm.ChatModel, req *llm.GenerateRequest) <-chan llm.StreamEvent {
	t.Helper()
	ch, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return ch
}

// wantKind 断言事件序列在 offset 处是期望的 Kind，返回该事件。
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

const chunkPrefix = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[`

func chunk(delta, finish string) string {
	return chunkPrefix + `{"index":0,"delta":{` + delta + `},"finish_reason":` + finish + `}]}`
}

// ---- Chat Completions ----

func TestCompletionsWireFormat(t *testing.T) {
	var gotPath, gotAuth string
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		body := readJSON(t, r)
		// 消息线格式
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatalf("messages 数量 = %d，期望 3", len(msgs))
		}
		sys, _ := msgs[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != "be brief" {
			t.Fatalf("system 消息不符: %v", sys)
		}
		assistant, _ := msgs[1].(map[string]any)
		if assistant["role"] != "assistant" {
			t.Fatalf("第 2 条应为 assistant: %v", assistant)
		}
		calls, _ := assistant["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatalf("assistant tool_calls 数量 = %d", len(calls))
		}
		call, _ := calls[0].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if call["id"] != "call_1" || fn["name"] != "echo" || fn["arguments"] != `{"x":1}` {
			t.Fatalf("工具调用线格式不符: %v", call)
		}
		tool, _ := msgs[2].(map[string]any)
		if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "ok" {
			t.Fatalf("tool 消息不符: %v", tool)
		}
		// 工具声明与选择
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools 数量 = %d", len(tools))
		}
		t0, _ := tools[0].(map[string]any)
		if t0["type"] != "function" {
			t.Fatalf("工具类型不符: %v", t0)
		}
		if _, ok := body["stream_options"]; ok {
			t.Fatalf("Generate 不得带 stream_options: %v", body["stream_options"])
		}
		if _, err := json.Marshal(body["temperature"]); err != nil || body["temperature"] != 0.5 {
			t.Fatalf("temperature 不符: %v", body["temperature"])
		}
		if body["max_completion_tokens"] != float64(128) {
			t.Fatalf("应使用 max_completion_tokens 新字段: %v", body["max_completion_tokens"])
		}
		stop, _ := body["stop"].([]any)
		if len(stop) != 1 || stop[0] != "END" {
			t.Fatalf("stop 不符: %v", body["stop"])
		}
		tc, _ := body["tool_choice"].(map[string]any)
		if tc["type"] != "function" {
			t.Fatalf("tool_choice 应为指定函数: %v", tc)
		}
		// 罐头响应：文本 + 工具调用 + usage
		resp := `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"calling","tool_calls":[{"id":"call_9","type":"function","function":{"name":"echo","arguments":"{\"x\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	req := llm.NewRequest(
		llm.System("be brief"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
			llm.Call(llm.ToolCall{ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}),
		}},
		llm.ToolMessage("call_1", "ok"),
	)
	temp := 0.5
	maxTok := 128
	req.Tools = []llm.ToolDef{{Name: "echo", Description: "echo it", Parameters: json.RawMessage(`{"type":"object"}`)}}
	req.ToolChoice = &llm.ToolChoice{Mode: llm.ToolSpecific, Name: "echo"}
	req.Temperature = &temp
	req.MaxTokens = &maxTok
	req.StopSequences = []string{"END"}

	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	// 响应映射
	if resp.FinishReason != llm.FinishToolCalls {
		t.Fatalf("FinishReason = %s", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.CachedInputTokens != 4 {
		t.Fatalf("Usage 不符: %+v", resp.Usage)
	}
	var tc *llm.ToolCall
	for _, p := range resp.Message.Parts {
		if p.Kind == llm.PartToolCall {
			tc = p.ToolCallValue
		}
	}
	if tc == nil || tc.ID != "call_9" || tc.Name != "echo" || string(tc.Arguments) != `{"x":2}` {
		t.Fatalf("工具调用映射不符: %+v", tc)
	}
}

func TestCompletionsStream(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		if body["stream"] != true {
			t.Fatalf("stream 应为 true")
		}
		so, _ := body["stream_options"].(map[string]any)
		if so["include_usage"] != true {
			t.Fatalf("应请求 include_usage: %v", so)
		}
		writeSSE(t, w,
			chunk(`"role":"assistant","content":"Hel"`, `null`),
			chunk(`"content":"lo"`, `null`),
			chunk(`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]`, `null`),
			chunk(`"tool_calls":[{"index":0,"function":{"arguments":"{\"city\""}}]`, `null`),
			chunk(`"tool_calls":[{"index":0,"function":{"arguments":":\"SF\"}"}}]`, `null`),
			chunk(``, `"tool_calls"`),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
		)
	})
	evs := collect(t, context.Background(), mustStream(t, m, llm.NewRequest(llm.UserText("hi"))))

	wantKind(t, evs, 0, llm.EventTextDelta)
	wantKind(t, evs, 1, llm.EventTextDelta)
	begin := wantKind(t, evs, 2, llm.EventToolCallBegin)
	if begin.Index != 1 || begin.CallID != "call_1" || begin.ToolName != "get_weather" {
		t.Fatalf("begin 不符: %+v", begin)
	}
	d1 := wantKind(t, evs, 3, llm.EventToolCallDelta)
	if d1.Text != `{"city"` {
		t.Fatalf("delta1 = %q", d1.Text)
	}
	wantKind(t, evs, 4, llm.EventToolCallDelta)
	done := wantKind(t, evs, 5, llm.EventDone)
	if done.Response.FinishReason != llm.FinishToolCalls {
		t.Fatalf("done FinishReason = %s", done.Response.FinishReason)
	}
	if done.Response.Usage.InputTokens != 10 || done.Response.Usage.CachedInputTokens != 4 {
		t.Fatalf("done Usage 不符: %+v", done.Response.Usage)
	}
	// 聚合消息：文本 + 完整参数的工具调用
	msg := done.Response.Message
	if msg.Text() != "Hello" {
		t.Fatalf("聚合文本 = %q", msg.Text())
	}
	calls := msg.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"SF"}` {
		t.Fatalf("聚合工具调用不符: %+v", calls)
	}
}

func TestCompletionsReasoningContent(t *testing.T) {
	// 非流式：reasoning_content 兼容字段映射为 PartReasoning。
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		resp := `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_content":"think hard"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	resp, err := m.Generate(context.Background(), llm.NewRequest(llm.UserText("q")))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.ReasoningText() != "think hard" {
		t.Fatalf("reasoning = %q", resp.Message.ReasoningText())
	}
	if resp.Message.Text() != "answer" {
		t.Fatalf("text = %q", resp.Message.Text())
	}

	// 流式：reasoning_content 增量映射为 reasoning_delta。
	ms := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			chunk(`"reasoning_content":"step 1"`, `null`),
			chunk(`"content":"done"`, `null`),
			chunk(``, `"stop"`),
		)
	})
	evs := collect(t, context.Background(), mustStream(t, ms, llm.NewRequest(llm.UserText("q"))))
	rd := wantKind(t, evs, 0, llm.EventReasoningDelta)
	if rd.Text != "step 1" {
		t.Fatalf("reasoning delta = %q", rd.Text)
	}
	wantKind(t, evs, 1, llm.EventTextDelta)
	done := wantKind(t, evs, 2, llm.EventDone)
	if done.Response.Message.ReasoningText() != "step 1" {
		t.Fatalf("聚合 reasoning = %q", done.Response.Message.ReasoningText())
	}
}

func TestCompletionsImageInput(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		user, _ := msgs[0].(map[string]any)
		parts, _ := user["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("图文混合应为内容块列表: %v", user["content"])
		}
		img, _ := parts[1].(map[string]any)
		if img["type"] != "image_url" {
			t.Fatalf("第 2 块应为 image_url: %v", img)
		}
		iu, _ := img["image_url"].(map[string]any)
		if !strings.HasPrefix(iu["url"].(string), "data:image/png;base64,") {
			t.Fatalf("data URI 不符: %v", iu["url"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"seen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Text("what is this"),
		llm.ImageData("image/png", []byte{0x89, 'P', 'N', 'G'}),
	}})
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "seen" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestCompletionsVideoInput(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		user, _ := msgs[0].(map[string]any)
		parts, _ := user["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("文本+视频应为内容块列表: %v", user["content"])
		}
		vid, _ := parts[1].(map[string]any)
		if vid["type"] != "video_url" {
			t.Fatalf("第 2 块应为 video_url: %v", vid)
		}
		vu, _ := vid["video_url"].(map[string]any)
		if vu["url"] != "https://example.com/clip.mp4" {
			t.Fatalf("video url 不符: %v", vu)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"clip seen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Text("what happens in this clip"),
		llm.MediaURL("video/mp4", "https://example.com/clip.mp4"),
	}})
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "clip seen" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestCompletionsErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   llm.ErrKind
		code   int
	}{
		{401, `{"error":{"message":"bad key","type":"invalid_request_error"}}`, llm.ErrAuth, 401},
		{429, `{"error":{"message":"slow down","type":"rate_limit_error"}}`, llm.ErrRateLimit, 429},
		{400, `{"error":{"message":"This model's maximum context length is 4096 tokens","type":"invalid_request_error"}}`, llm.ErrContextLength, 400},
		{400, `{"error":{"message":"invalid x","type":"invalid_request_error","code":"content_filter"}}`, llm.ErrContentFilter, 400},
		{404, `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`, llm.ErrNoModel, 404},
		{500, `{"error":{"message":"boom","type":"server_error"}}`, llm.ErrProvider, 500},
	}
	for _, tc := range cases {
		m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := m.Generate(context.Background(), llm.NewRequest(llm.UserText("q")))
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
		if llm.IsRetryable(err) != (tc.kind == llm.ErrRateLimit || tc.kind == llm.ErrProvider) {
			t.Fatalf("status %d: IsRetryable 判定不符", tc.status)
		}
	}
}

// ---- Responses ----

func TestResponsesWireFormat(t *testing.T) {
	var gotPath string
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body := readJSON(t, r)
		if body["instructions"] != "be brief" {
			t.Fatalf("system 应进 instructions: %v", body["instructions"])
		}
		if body["store"] != false {
			t.Fatalf("无状态适配应 store=false")
		}
		items, _ := body["input"].([]any)
		// [user 文本, assistant function_call, function_call_output]
		if len(items) != 3 {
			t.Fatalf("input items = %d，期望 3: %v", len(items), body["input"])
		}
		i0, _ := items[0].(map[string]any)
		if i0["role"] != "user" || i0["content"] != "hi" {
			t.Fatalf("user 项不符: %v", i0)
		}
		i1, _ := items[1].(map[string]any)
		if i1["type"] != "function_call" || i1["call_id"] != "call_1" || i1["name"] != "echo" || i1["arguments"] != `{"x":1}` {
			t.Fatalf("function_call 项不符: %v", i1)
		}
		i2, _ := items[2].(map[string]any)
		if i2["type"] != "function_call_output" || i2["call_id"] != "call_1" || i2["output"] != "ok" {
			t.Fatalf("function_call_output 项不符: %v", i2)
		}
		tools, _ := body["tools"].([]any)
		t0, _ := tools[0].(map[string]any)
		if t0["type"] != "function" || t0["strict"] != false || t0["name"] != "echo" {
			t.Fatalf("工具声明不符: %v", t0)
		}
		resp := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"ponder"}]},{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi there","annotations":[]}]},{"type":"function_call","id":"fc1","call_id":"call_9","name":"echo","arguments":"{\"x\":2}"}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":15},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	req := llm.NewRequest(
		llm.System("be brief"),
		llm.UserText("hi"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
			llm.Call(llm.ToolCall{ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}),
		}},
		llm.ToolMessage("call_1", "ok"),
	)
	req.Tools = []llm.ToolDef{{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}}

	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q", gotPath)
	}
	if resp.FinishReason != llm.FinishToolCalls {
		t.Fatalf("含 function_call 应为 FinishToolCalls，得到 %s", resp.FinishReason)
	}
	if resp.Message.ReasoningText() != "ponder" || resp.Message.Text() != "hi there" {
		t.Fatalf("消息映射不符: reasoning=%q text=%q", resp.Message.ReasoningText(), resp.Message.Text())
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "call_9" || string(calls[0].Arguments) != `{"x":2}` {
		t.Fatalf("工具调用映射不符: %+v", calls)
	}
	if resp.Usage.CachedInputTokens != 3 {
		t.Fatalf("CachedInputTokens = %d", resp.Usage.CachedInputTokens)
	}
}

func TestResponsesStream(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		completed := `{"type":"response.completed","sequence_number":6,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world","annotations":[]}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":15},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}}`
		writeSSE(t, w,
			`{"type":"response.output_text.delta","item_id":"m1","output_index":0,"content_index":0,"delta":"Hello ","sequence_number":1}`,
			`{"type":"response.output_text.delta","item_id":"m1","output_index":0,"content_index":0,"delta":"world","sequence_number":2}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"r1","output_index":2,"summary_index":0,"delta":"thinking","sequence_number":3}`,
			`{"type":"response.output_item.added","output_index":1,"sequence_number":4,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"ci","sequence_number":5}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"ty\":\"SF\"}","sequence_number":6}`,
			completed,
		)
	})
	evs := collect(t, context.Background(), mustStream(t, m, llm.NewRequest(llm.UserText("hi"))))

	wantKind(t, evs, 0, llm.EventTextDelta)
	wantKind(t, evs, 1, llm.EventTextDelta)
	wantKind(t, evs, 2, llm.EventReasoningDelta)
	begin := wantKind(t, evs, 3, llm.EventToolCallBegin)
	if begin.Index != 1 || begin.CallID != "call_1" || begin.ToolName != "get_weather" {
		t.Fatalf("begin 不符: %+v", begin)
	}
	wantKind(t, evs, 4, llm.EventToolCallDelta)
	wantKind(t, evs, 5, llm.EventToolCallDelta)
	done := wantKind(t, evs, 6, llm.EventDone)
	if done.Response.FinishReason != llm.FinishToolCalls {
		t.Fatalf("含 function_call 应为 FinishToolCalls，得到 %s", done.Response.FinishReason)
	}
	msg := done.Response.Message
	if msg.Text() != "Hello world" || msg.ReasoningText() != "thinking" {
		t.Fatalf("聚合消息不符: text=%q reasoning=%q", msg.Text(), msg.ReasoningText())
	}
	calls := msg.ToolCalls()
	if len(calls) != 1 || string(calls[0].Arguments) != `{"city":"SF"}` {
		t.Fatalf("聚合工具调用不符: %+v", calls)
	}
	if done.Response.Usage.OutputTokens != 5 {
		t.Fatalf("Usage 不符: %+v", done.Response.Usage)
	}
}

func TestResponsesImageOnly(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		items, _ := body["input"].([]any)
		if len(items) != 1 {
			t.Fatalf("仅图应发 1 条 input，得到 %d: %v", len(items), body["input"])
		}
		item, _ := items[0].(map[string]any)
		parts, _ := item["content"].([]any)
		if len(parts) != 1 {
			t.Fatalf("content 块数 = %d，期望 1（纯图）: %v", len(parts), item["content"])
		}
		p0, _ := parts[0].(map[string]any)
		if p0["type"] != "input_image" {
			t.Fatalf("应为 input_image: %v", p0)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"seen","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.ImageURL("https://example.com/cat.png", "image/png"),
	}})
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "seen" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestResponsesTextThenImage(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		items, _ := body["input"].([]any)
		if len(items) != 1 {
			t.Fatalf("文+图应合成 1 条: %d %v", len(items), body["input"])
		}
		item, _ := items[0].(map[string]any)
		parts, _ := item["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("content 块数 = %d: %v", len(parts), item["content"])
		}
		p0, _ := parts[0].(map[string]any)
		p1, _ := parts[1].(map[string]any)
		if p0["type"] != "input_text" || p0["text"] != "look" {
			t.Fatalf("第 1 块应为文本 look: %v", p0)
		}
		if p1["type"] != "input_image" {
			t.Fatalf("第 2 块应为 input_image: %v", p1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Text("look"),
		llm.ImageURL("https://example.com/cat.png", "image/png"),
	}})
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestResponsesVideoAndPDF(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		items, _ := body["input"].([]any)
		item, _ := items[0].(map[string]any)
		parts, _ := item["content"].([]any)
		if len(parts) != 3 {
			t.Fatalf("文本+视频+pdf 应为 3 块: %v", item["content"])
		}
		p1, _ := parts[1].(map[string]any)
		p2, _ := parts[2].(map[string]any)
		if p1["type"] != "input_video" {
			t.Fatalf("第 2 块应为 input_video: %v", p1)
		}
		if p2["type"] != "input_file" {
			t.Fatalf("第 3 块应为 input_file: %v", p2)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Text("inspect"),
		llm.MediaURL("video/mp4", "https://example.com/clip.mp4"),
		llm.Media("application/pdf", []byte("%PDF")),
	}})
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestCompletionsAudioAndPDF(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		msgs, _ := body["messages"].([]any)
		user, _ := msgs[0].(map[string]any)
		parts, _ := user["content"].([]any)
		if len(parts) != 3 {
			t.Fatalf("文本+音频+pdf 应为 3 块: %v", user["content"])
		}
		p1, _ := parts[1].(map[string]any)
		p2, _ := parts[2].(map[string]any)
		if p1["type"] != "input_audio" {
			t.Fatalf("第 2 块应为 input_audio: %v", p1)
		}
		if p2["type"] != "file" {
			t.Fatalf("第 3 块应为 file: %v", p2)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Text("listen"),
		llm.Media("audio/wav", []byte("RIFF")),
		llm.Media("application/pdf", []byte("%PDF")),
	}})
	if _, err := m.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestStreamCancelSendsEventError(t *testing.T) {
	started := make(chan struct{})
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+chunk(`"content":"x"`, `null`)+"\n\n")
		flusher.Flush()
		time.Sleep(2 * time.Second) // 等测试侧 cancel
	})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Stream(ctx, llm.NewRequest(llm.UserText("hi")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	<-started
	cancel()
	var evs []llm.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	if len(evs) == 0 {
		t.Fatal("取消后应至少有 EventError")
	}
	last := evs[len(evs)-1]
	if last.Kind != llm.EventError {
		t.Fatalf("最后事件 = %s，期望 EventError", last.Kind)
	}
	if llm.KindOf(last.Err) != llm.ErrCanceled {
		t.Fatalf("kind = %s，期望 canceled（err=%v）", llm.KindOf(last.Err), last.Err)
	}
}

func TestResponsesCancelSendsEventError(t *testing.T) {
	started := make(chan struct{})
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","item_id":"m1","output_index":0,"content_index":0,"delta":"x","sequence_number":1}`+"\n\n")
		flusher.Flush()
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Stream(ctx, llm.NewRequest(llm.UserText("hi")))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	<-started
	cancel()
	var evs []llm.StreamEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	if len(evs) == 0 {
		t.Fatal("取消后应至少有 EventError")
	}
	last := evs[len(evs)-1]
	if last.Kind != llm.EventError || llm.KindOf(last.Err) != llm.ErrCanceled {
		t.Fatalf("最后事件 = %s（err=%v），期望 EventError(canceled)", last.Kind, last.Err)
	}
}

func TestUnknownMIMERejected(t *testing.T) {
	// 未知 application/* 与空 MediaType 都必须显式 bad_request 且不发请求。
	reject := func(t *testing.T, m llm.ChatModel, mediaType string, data []byte) {
		t.Helper()
		req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
			llm.Media(mediaType, data),
		}})
		if _, err := m.Generate(context.Background(), req); llm.KindOf(err) != llm.ErrBadRequest {
			t.Fatalf("mediaType %q: kind = %v，期望 bad_request", mediaType, llm.KindOf(err))
		}
	}
	mc := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("不应发出请求") })
	reject(t, mc, "application/zip", []byte("PK"))
	reject(t, mc, "", []byte("???"))
	mr := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("不应发出请求") })
	reject(t, mr, "application/zip", []byte("PK"))
	reject(t, mr, "", []byte("???"))
}

func TestResponsesImageBeforeText(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		items, _ := body["input"].([]any)
		item, _ := items[0].(map[string]any)
		parts, _ := item["content"].([]any)
		if len(parts) != 2 {
			t.Fatalf("图+文应为 2 块: %v", item["content"])
		}
		p0, _ := parts[0].(map[string]any)
		p1, _ := parts[1].(map[string]any)
		if p0["type"] != "input_image" {
			t.Fatalf("图在前：第 1 块应为 input_image: %v", p0)
		}
		if p1["type"] != "input_text" || p1["text"] != "what is this" {
			t.Fatalf("图在前：第 2 块应为文本: %v", p1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"cat","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}`))
	})
	req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.ImageURL("https://example.com/cat.png", "image/png"),
		llm.Text("what is this"),
	}})
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "cat" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestCompletionsToolChoiceAndFormat(t *testing.T) {
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		switch body["model"] {
		case "auto-test":
			tc, _ := body["tool_choice"].(string)
			if tc != "auto" {
				t.Fatalf("auto 应为字符串 tool_choice: %v", body["tool_choice"])
			}
		case "none-test":
			tc, _ := body["tool_choice"].(string)
			if tc != "none" {
				t.Fatalf("none 应为字符串 tool_choice: %v", body["tool_choice"])
			}
		case "format-test":
			rf, _ := body["response_format"].(map[string]any)
			if rf["type"] != "json_object" {
				t.Fatalf("response_format.type = %v", rf["type"])
			}
		case "schema-test":
			rf, _ := body["response_format"].(map[string]any)
			if rf["type"] != "json_schema" {
				t.Fatalf("response_format.type = %v", rf["type"])
			}
			js, _ := rf["json_schema"].(map[string]any)
			if js["name"] != "answer" {
				t.Fatalf("json_schema.name = %v", js["name"])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	for _, tc := range []struct {
		model string
		mode  llm.ToolChoiceMode
		rf    *llm.ResponseFormat
	}{
		{"auto-test", llm.ToolAuto, nil},
		{"none-test", llm.ToolNone, nil},
		{"format-test", "", &llm.ResponseFormat{Type: llm.FormatJSONObject}},
		{"schema-test", "", &llm.ResponseFormat{Type: llm.FormatJSONSchema, Name: "answer", Schema: json.RawMessage(`{"type":"object"}`)}},
	} {
		req := llm.NewRequest(llm.UserText("q"))
		if tc.mode != "" {
			req.ToolChoice = &llm.ToolChoice{Mode: tc.mode}
		}
		if tc.rf != nil {
			req.ResponseFormat = tc.rf
		}
		if _, err := m.Generate(context.Background(), req); err != nil {
			t.Fatalf("%s: Generate: %v", tc.model, err)
		}
	}
}

func TestCompletionsAudioOutput(t *testing.T) {
	// TTS 线格式：请求带 modalities + 官方 audio 模态参数；
	// 响应 message.audio.data 解码为 PartCustom(audio/*)，
	// transcript 非空时映射为文本块。
	audioB64 := base64.StdEncoding.EncodeToString([]byte("RIFFxxxx"))
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		body := readJSON(t, r)
		mods, _ := body["modalities"].([]any)
		if len(mods) != 2 || mods[0] != "text" || mods[1] != "audio" {
			t.Fatalf("modalities 应为 [text audio]: %v", body["modalities"])
		}
		au, _ := body["audio"].(map[string]any)
		if au == nil {
			t.Fatalf("请求应带 audio 模态: %v", body)
		}
		if au["voice"] != "mimo_default" || au["format"] != "wav" {
			t.Fatalf("audio 参数不符: %v", au)
		}
		resp := `{"id":"c1","object":"chat.completion","created":1,"model":"tts","choices":[{"index":0,"message":{"role":"assistant","content":"","audio":{"id":"a1","data":"` + audioB64 + `","expires_at":0,"transcript":"你好，世界。"}},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	req := llm.NewRequest(
		llm.UserText("用轻快的语气"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("你好，世界。")}},
	)
	req.Audio = &llm.AudioOutput{Voice: "mimo_default", Format: "wav"}
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var found *llm.MediaContent
	for _, p := range resp.Message.Parts {
		if p.Kind == llm.PartCustom && p.Media != nil && p.Media.MediaType == "audio/wav" {
			found = p.Media
		}
	}
	if found == nil || string(found.Data) != "RIFFxxxx" {
		t.Fatalf("音频块映射不符: %+v", resp.Message.Parts)
	}
	if resp.Message.Text() != "你好，世界。" {
		t.Fatalf("transcript 应映射为文本块: %q", resp.Message.Text())
	}
}

func TestCompletionsAudioStreamFragments(t *testing.T) {
	// 流式 audio：每片是独立 base64 编码块，必须逐片解码后拼字节——
	// 拼字符串再整体解码会因 padding 错位失败或产出坏字节。
	frag1 := base64.StdEncoding.EncodeToString([]byte("RIFF")) // 独立编码，含 padding
	frag2 := base64.StdEncoding.EncodeToString([]byte("xxxxWAVE"))
	m := newCompletionsTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			chunk(`"audio":{"data":"`+frag1+`"}`, `null`),
			chunk(`"audio":{"data":"`+frag2+`"}`, `null`),
			chunk(``, `"stop"`),
		)
	})
	req := llm.NewRequest(llm.UserText("tts"))
	req.Audio = &llm.AudioOutput{Voice: "v", Format: "wav"}
	evs := collect(t, context.Background(), mustStream(t, m, req))
	done := wantKind(t, evs, len(evs)-1, llm.EventDone)
	var audio *llm.MediaContent
	for _, p := range done.Response.Message.Parts {
		if p.Kind == llm.PartCustom && p.Media != nil {
			audio = p.Media
		}
	}
	if audio == nil || string(audio.Data) != "RIFFxxxxWAVE" {
		t.Fatalf("分片聚合不符: %+v", done.Response.Message.Parts)
	}
}

func TestResponsesAudioRejected(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("不应发出请求")
	})
	req := llm.NewRequest(llm.UserText("hi"))
	req.Audio = &llm.AudioOutput{Voice: "v", Format: "wav"}
	_, err := m.Generate(context.Background(), req)
	if llm.KindOf(err) != llm.ErrBadRequest {
		t.Fatalf("kind = %s，期望 bad_request（err=%v）", llm.KindOf(err), err)
	}
}

func TestRegistryOpenResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"roundtrip","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}`))
	}))
	defer srv.Close()

	ctx := kernel.New()
	reg := llm.NewRegistry(ctx)
	if err := Register(ctx, reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Declare("r", llm.Config{
		Provider: ProviderResponses, Model: "gpt-test", APIKey: "k", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	model, err := reg.Open("r")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	resp, err := model.Generate(context.Background(), llm.NewRequest(llm.UserText("q")))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "roundtrip" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

func TestResponsesIncomplete(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			`{"type":"response.output_text.delta","item_id":"m1","output_index":0,"content_index":0,"delta":"partial","sequence_number":1}`,
			`{"type":"response.incomplete","sequence_number":2,"response":{"id":"resp_1","object":"response","created_at":1,"status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"},"model":"gpt-test","output":[{"type":"message","id":"m1","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"partial","annotations":[]}]}],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":15},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}}`,
		)
	})
	evs := collect(t, context.Background(), mustStream(t, m, llm.NewRequest(llm.UserText("hi"))))
	done := wantKind(t, evs, 1, llm.EventDone)
	if done.Response.FinishReason != llm.FinishLength {
		t.Fatalf("incomplete 应映射 FinishLength，得到 %s", done.Response.FinishReason)
	}
}

func TestResponsesStopSequencesRejected(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("不应发出请求")
	})
	req := llm.NewRequest(llm.UserText("hi"))
	req.StopSequences = []string{"END"}
	_, err := m.Generate(context.Background(), req)
	if llm.KindOf(err) != llm.ErrBadRequest {
		t.Fatalf("kind = %s，期望 bad_request（err=%v）", llm.KindOf(err), err)
	}
}

func TestResponsesFailedEvent(t *testing.T) {
	m := newResponsesTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w,
			`{"type":"response.failed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"status":"failed","error":{"code":"server_error","message":"boom"},"incomplete_details":null,"model":"gpt-test","output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0},"metadata":{},"tool_choice":"auto","tools":[],"parallel_tool_calls":true}}`,
		)
	})
	evs := collect(t, context.Background(), mustStream(t, m, llm.NewRequest(llm.UserText("hi"))))
	last := evs[len(evs)-1]
	if last.Kind != llm.EventError {
		t.Fatalf("期望 EventError，得到 %s", last.Kind)
	}
	if llm.KindOf(last.Err) != llm.ErrProvider {
		t.Fatalf("kind = %s", llm.KindOf(last.Err))
	}
}

// ---- 配置与装配 ----

func TestConfigValidation(t *testing.T) {
	_, err := NewCompletions(llm.Config{Provider: ProviderCompletions, Model: "m"})
	if llm.KindOf(err) != llm.ErrAuth {
		t.Fatalf("缺 APIKey 应报 auth，得到 %v", err)
	}
}

func TestOptionsHeaders(t *testing.T) {
	var gotOrg, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = r.Header.Get("OpenAI-Organization")
		gotCustom = r.Header.Get("X-Trace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	m, err := NewCompletions(llm.Config{
		Provider: ProviderCompletions, Model: "gpt-test", APIKey: "k", BaseURL: srv.URL,
		Options: map[string]any{"organization": "org-1", "headers": map[string]any{"X-Trace": "t-1"}},
	})
	if err != nil {
		t.Fatalf("NewCompletions: %v", err)
	}
	if _, err := m.Generate(context.Background(), llm.NewRequest(llm.UserText("q"))); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotOrg != "org-1" || gotCustom != "t-1" {
		t.Fatalf("headers 不符: org=%q trace=%q", gotOrg, gotCustom)
	}
}

// TestRegistryIntegration 走真实装配路径：Register → Declare → Open。
func TestRegistryIntegration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"roundtrip"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	ctx := kernel.New()
	reg := llm.NewRegistry(ctx)
	if err := Register(ctx, reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Declare("main", llm.Config{
		Provider: ProviderCompletions, Model: "gpt-test", APIKey: "k", BaseURL: srv.URL,
	}); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	model, err := reg.Open("main")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	resp, err := model.Generate(context.Background(), llm.NewRequest(llm.UserText("q")))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "roundtrip" {
		t.Fatalf("text = %q", resp.Message.Text())
	}
}

// ---- 真机冒烟（环境变量门控，凭据绝不入库）----
//
//	PULSE_OPENAI_API_KEY     必填才跑
//	PULSE_OPENAI_BASE_URL    可选，覆盖端点（OpenAI 兼容网关）
//	PULSE_OPENAI_MODEL       可选，默认 gpt-4o-mini
//	PULSE_OPENAI_SKIP_RESPONSES=1  跳过 Responses 变体（兼容网关常不实现）

func liveCfg(t *testing.T) llm.Config {
	t.Helper()
	key := os.Getenv("PULSE_OPENAI_API_KEY")
	if key == "" {
		t.Skip("PULSE_OPENAI_API_KEY 未设置，跳过真机冒烟")
	}
	model := os.Getenv("PULSE_OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return llm.Config{
		Provider: ProviderCompletions,
		Model:    model,
		APIKey:   key,
		BaseURL:  os.Getenv("PULSE_OPENAI_BASE_URL"),
	}
}

func TestLiveCompletions(t *testing.T) {
	cfg := liveCfg(t)
	model, err := NewCompletions(cfg)
	if err != nil {
		t.Fatalf("NewCompletions: %v", err)
	}

	t.Run("text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		resp, err := model.Generate(ctx, llm.NewRequest(llm.UserText("用一句话介绍 Go 语言")))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if resp.Message.Text() == "" {
			t.Fatalf("空文本；reasoning=%q finish=%s", resp.Message.ReasoningText(), resp.FinishReason)
		}
		t.Logf("text=%q tokens in/out=%d/%d", truncate(resp.Message.Text(), 80),
			resp.Usage.InputTokens, resp.Usage.OutputTokens)
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ch, err := model.Stream(ctx, llm.NewRequest(llm.UserText("数到三，只输出数字")))
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		evs := collect(t, ctx, ch)
		var sb strings.Builder
		for _, ev := range evs {
			if ev.Kind == llm.EventTextDelta {
				sb.WriteString(ev.Text)
			}
		}
		if sb.Len() == 0 {
			last := evs[len(evs)-1]
			t.Fatalf("流式未收到文本增量 last=%s err=%v", last.Kind, last.Err)
		}
		t.Logf("stream=%q events=%d", truncate(sb.String(), 80), len(evs))
	})

	t.Run("tools", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req := llm.NewRequest(llm.UserText("北京现在天气如何？请调用工具查询，不要直接回答"))
		req.Tools = []llm.ToolDef{{
			Name:        "get_weather",
			Description: "查询城市天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}
		req.ToolChoice = &llm.ToolChoice{Mode: llm.ToolAny}
		resp, err := model.Generate(ctx, req)
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
			llm.ImageURL("https://filecdn.minimax.chat/public/4ab63cda-da2a-4c77-b1c7-900d2562073f.png", "image/png"),
		}})
		resp, err := model.Generate(ctx, req)
		if err != nil {
			t.Fatalf("Generate(image): %v", err)
		}
		if resp.Message.Text() == "" {
			t.Fatal("图像输入返回空文本")
		}
		t.Logf("image=%q", truncate(resp.Message.Text(), 80))
	})

	t.Run("video", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		req := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
			llm.Text("这段视频里发生了什么？用一句话回答"),
			llm.MediaURL("video/mp4", "https://interactive-examples.mdn.mozilla.net/media/cc0-videos/flower.mp4"),
		}})
		resp, err := model.Generate(ctx, req)
		if err != nil {
			t.Fatalf("Generate(video): %v", err)
		}
		if resp.Message.Text() == "" {
			t.Fatal("视频输入返回空文本")
		}
		t.Logf("video=%q", truncate(resp.Message.Text(), 80))
	})
}

func TestLiveResponses(t *testing.T) {
	if os.Getenv("PULSE_OPENAI_SKIP_RESPONSES") == "1" {
		t.Skip("PULSE_OPENAI_SKIP_RESPONSES=1")
	}
	cfg := liveCfg(t)
	cfg.Provider = ProviderResponses
	model, err := NewResponses(cfg)
	if err != nil {
		t.Fatalf("NewResponses: %v", err)
	}

	t.Run("text", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		resp, err := model.Generate(ctx, llm.NewRequest(llm.UserText("用一句话介绍 Go 语言")))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if resp.Message.Text() == "" {
			t.Fatalf("空文本；reasoning=%q finish=%s", resp.Message.ReasoningText(), resp.FinishReason)
		}
		t.Logf("text=%q tokens in/out=%d/%d", truncate(resp.Message.Text(), 80),
			resp.Usage.InputTokens, resp.Usage.OutputTokens)
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ch, err := model.Stream(ctx, llm.NewRequest(llm.UserText("数到三，只输出数字")))
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		evs := collect(t, ctx, ch)
		var sb strings.Builder
		for _, ev := range evs {
			if ev.Kind == llm.EventTextDelta {
				sb.WriteString(ev.Text)
			}
		}
		if sb.Len() == 0 {
			last := evs[len(evs)-1]
			t.Fatalf("流式未收到文本增量 last=%s err=%v", last.Kind, last.Err)
		}
		t.Logf("stream=%q events=%d", truncate(sb.String(), 80), len(evs))
	})

	t.Run("tools", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req := llm.NewRequest(llm.UserText("北京现在天气如何？请调用工具查询，不要直接回答"))
		req.Tools = []llm.ToolDef{{
			Name:        "get_weather",
			Description: "查询城市天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}}
		req.ToolChoice = &llm.ToolChoice{Mode: llm.ToolAny}
		resp, err := model.Generate(ctx, req)
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
			llm.ImageURL("https://filecdn.minimax.chat/public/4ab63cda-da2a-4c77-b1c7-900d2562073f.png", "image/png"),
		}})
		resp, err := model.Generate(ctx, req)
		if err != nil {
			t.Fatalf("Generate(image): %v", err)
		}
		if resp.Message.Text() == "" {
			t.Fatal("图像输入返回空文本")
		}
		t.Logf("image=%q", truncate(resp.Message.Text(), 80))
	})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- MiMo 真机（ASR/TTS 走对话线格式，环境变量门控）----
//
//	PULSE_MIMO_API_KEY / PULSE_MIMO_BASE_URL 存在才跑。
//	闭环设计：TTS 合成 wav → 字节直接喂 ASR → 校验转写文本。

func mimoCfg(t *testing.T, model string) llm.Config {
	t.Helper()
	key := os.Getenv("PULSE_MIMO_API_KEY")
	if key == "" {
		t.Skip("PULSE_MIMO_API_KEY 未设置，跳过 MiMo 真机")
	}
	return llm.Config{
		Provider: ProviderCompletions,
		Model:    model,
		APIKey:   key,
		BaseURL:  os.Getenv("PULSE_MIMO_BASE_URL"),
	}
}

func TestLiveMimoTTS(t *testing.T) {
	m, err := NewCompletions(mimoCfg(t, "mimo-v2.5-tts"))
	if err != nil {
		t.Fatalf("NewCompletions: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req := llm.NewRequest(
		llm.UserText("用平静的语速朗读"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text("你好，这是 Pulse 适配层的真机语音合成测试。")}},
	)
	req.Audio = &llm.AudioOutput{Voice: "mimo_default", Format: "wav"}
	resp, err := m.Generate(ctx, req)
	if err != nil {
		t.Fatalf("TTS Generate: %v", err)
	}
	var audio *llm.MediaContent
	for _, p := range resp.Message.Parts {
		if p.Kind == llm.PartCustom && p.Media != nil && strings.HasPrefix(p.Media.MediaType, "audio/") {
			audio = p.Media
		}
	}
	if audio == nil || len(audio.Data) < 44 {
		t.Fatalf("TTS 未返回有效音频: parts=%d", len(resp.Message.Parts))
	}
	if string(audio.Data[:4]) != "RIFF" {
		t.Fatalf("wav 头不符: %x", audio.Data[:4])
	}
	t.Logf("TTS OK: %d bytes wav", len(audio.Data))
}

func TestLiveMimoASR(t *testing.T) {
	// 第一步：TTS 合成一段已知文本的 wav。
	ttsM, err := NewCompletions(mimoCfg(t, "mimo-v2.5-tts"))
	if err != nil {
		t.Fatalf("NewCompletions(tts): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	target := "今天天气不错。"
	ttsReq := llm.NewRequest(
		llm.UserText("朗读"),
		&llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text(target)}},
	)
	ttsReq.Audio = &llm.AudioOutput{Voice: "mimo_default", Format: "wav"}
	ttsResp, err := ttsM.Generate(ctx, ttsReq)
	if err != nil {
		t.Fatalf("TTS: %v", err)
	}
	var wav []byte
	for _, p := range ttsResp.Message.Parts {
		if p.Kind == llm.PartCustom && p.Media != nil && strings.HasPrefix(p.Media.MediaType, "audio/") {
			wav = p.Media.Data
		}
	}
	if len(wav) == 0 {
		t.Fatal("TTS 无音频产物")
	}

	// 第二步：同一音频喂 ASR，校验转写。
	asrM, err := NewCompletions(mimoCfg(t, "mimo-v2.5-asr"))
	if err != nil {
		t.Fatalf("NewCompletions(asr): %v", err)
	}
	asrReq := llm.NewRequest(&llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		llm.Media("audio/wav", wav),
	}})
	asrResp, err := asrM.Generate(ctx, asrReq)
	if err != nil {
		t.Fatalf("ASR Generate: %v", err)
	}
	text := asrResp.Message.Text()
	if strings.TrimSpace(text) == "" {
		t.Fatal("ASR 返回空转写")
	}
	t.Logf("ASR OK: %q (期望含 %q)", truncate(text, 60), target)
}
