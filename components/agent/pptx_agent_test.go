// components/agent/pptx_agent_test.go
//
// 测试 Agent 能否真实使用 PPTX Skill。
// 调用链路: Agent.Send → LLM → tool_call("pptx") → ToolRegistry 执行 → 指令返回 → LLM 继续
//
// 运行 Mock 测试（不需要 API Key）:
//
//	PPTX_SKILL_PATH=../../skills/pptx go test -v ./components/agent/ -run TestPPTXAgent
//
// 运行真实 LLM 测试:
//
//	PPTX_SKILL_PATH=../../skills/pptx \
//	LLM_BASE_URL=https://api.deepseek.com \
//	LLM_API_KEY=sk-xxx \
//	LLM_MODEL=deepseek-chat \
//	go test -v ./components/agent/ -run TestPPTXAgent_RealLLM -timeout 120s
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/agent"
	"github.com/Luo-root/pulse/components/chatmodel/openai"
	"github.com/Luo-root/pulse/components/sandbox"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/skill"
	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// 测试基础设施
// ============================================================================

// locatePPTXSkill 定位 PPTX Skill 目录
func locatePPTXSkill() string {
	candidates := []string{
		os.Getenv("PPTX_SKILL_PATH"),
		"testdata/pptx",
		"skills/pptx",
		"../../skills/pptx",
		"../../../skills/pptx",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "SKILL.md")); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

// setupPPTXEnv 创建完整测试环境：SkillRegistry + ToolRegistry + PPTX Skill 已加载
func setupPPTXEnv(t *testing.T) (*skill.SkillRegistry, *tools.ToolRegistry, *skill.SkillLoader) {
	t.Helper()

	dir := locatePPTXSkill()
	if dir == "" {
		t.Skip("未找到 PPTX Skill。设置 PPTX_SKILL_PATH 环境变量")
	}

	skillReg := skill.NewSkillRegistry()
	toolReg := tools.NewToolRegistry()
	loader := skill.NewSkillLoader(skillReg, toolReg)

	pptxPath := filepath.Join(dir, "SKILL.md")
	if err := loader.LoadFromFile(pptxPath); err != nil {
		t.Fatalf("加载 PPTX Skill 失败: %v", err)
	}

	// 验证加载成功
	sk, ok := skillReg.Get("pptx")
	if !ok {
		t.Fatal("PPTX Skill 加载后未在 SkillRegistry 中找到")
	}

	t.Logf("环境就绪: skill=%q body=%d字符", sk.Name, len(sk.Body))
	return skillReg, toolReg, loader
}

// ============================================================================
// Mock 模型
// ============================================================================

// mockPPTXAgentModel 模拟一个会使用 pptx 工具的 LLM。
//
// 调用序列：
//
//	第 1 次：返回 tool_call 到 "pptx"（触发工具调用）
//	第 2 次：检查消息中已包含 PPTX 指令，返回最终回复
//
// 同时记录每次调用的输入消息，供测试断言使用。
type mockPPTXAgentModel struct {
	mu        sync.Mutex
	callCount int
	inputs    [][]*schema.Message // 每次 Generate 收到的消息快照
}

func newMockPPTXAgentModel() *mockPPTXAgentModel {
	return &mockPPTXAgentModel{}
}

func (m *mockPPTXAgentModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	m.mu.Lock()
	m.callCount++
	call := m.callCount
	// 快照输入消息
	copied := make([]*schema.Message, len(input))
	for i, msg := range input {
		clone := msg.Clone()
		copied[i] = &clone
	}
	m.inputs = append(m.inputs, copied)
	m.mu.Unlock()

	switch call {
	case 1:
		// 模拟 LLM 决定调用 pptx 工具
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "我需要先获取 PPTX 制作指南。",
			ToolCalls: []schema.ToolCall{
				{
					ID:   "call_pptx_mock_001",
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "pptx",
						Arguments: "{}",
					},
				},
			},
		}, nil

	case 2:
		// 模拟 LLM 看到 PPTX 指令后的回复
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "已获取 PPTX 制作指南。根据 Design Ideas 中的建议，我将使用 Midnight Executive 配色方案（navy #1E2761 + ice blue #CADCFC），采用 pptxgenjs 创建一个 3 页的 AI 趋势演示文稿。",
		}, nil

	default:
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "任务已完成。",
		}, nil
	}
}

func (m *mockPPTXAgentModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	msg, err := m.Generate(ctx, input)
	if err != nil {
		return nil, err
	}
	reader := schema.NewStreamReader()
	go func() {
		reader.Send(msg.Clone())
		reader.Close()
	}()
	return reader, nil
}

func (m *mockPPTXAgentModel) getCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

func (m *mockPPTXAgentModel) getInput(callIndex int) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if callIndex >= len(m.inputs) {
		return nil
	}
	return m.inputs[callIndex]
}

// ============================================================================
// Test 1: 工具注册验证
// ============================================================================

func TestPPTXAgent_ToolRegistered(t *testing.T) {
	_, toolReg, _ := setupPPTXEnv(t)

	// 创建 Agent，验证初始化成功（Agent 持有同一个 ToolRegistry）
	mockModel := newMockPPTXAgentModel()
	ag := agent.NewAgent(mockModel, toolReg)
	if ag == nil {
		t.Fatal("NewAgent 返回 nil")
	}

	// 通过实际调用验证工具可执行：
	// Agent.Send → mock 返回 tool_call("pptx") → Agent 通过 toolReg 执行 → 成功
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := ag.Send(ctx, "列出你可以使用的工具")
	if err != nil {
		t.Fatalf("Agent.Send 失败: %v", err)
	}
	if resp == nil {
		t.Fatal("响应为 nil")
	}

	// mock 模型第 1 次调用返回 tool_call，Agent 应该执行了 pptx 工具
	// 然后第 2 次调用 mock 返回最终文本
	if mockModel.getCallCount() < 2 {
		t.Errorf("期望至少 2 次模型调用（触发工具 + 最终回复），实际 %d", mockModel.getCallCount())
	}

	t.Logf("✓ pptx 工具已注册且可被 Agent 执行")
}

// ============================================================================
// Test 2: Agent 完整工具调用循环（非流式）
// ============================================================================

func TestPPTXAgent_MockRoundTrip(t *testing.T) {
	skillReg, toolReg, _ := setupPPTXEnv(t)

	// 记录 Skill 元信息
	sk, _ := skillReg.Get("pptx")
	t.Logf("Skill 元信息: name=%q desc_len=%d body=%d", sk.Name, len(sk.Description), len(sk.Body))

	mockModel := newMockPPTXAgentModel()
	ag := agent.NewAgent(mockModel, toolReg,
		agent.WithSessionID("test-pptx-roundtrip"),
		agent.WithMaxToolRounds(5),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	userMsg := "帮我创建一个关于 AI 趋势的 3 页 PPT 演示文稿"
	resp, err := ag.Send(ctx, userMsg)
	if err != nil {
		t.Fatalf("Agent.Send 失败: %v", err)
	}

	// ---- 验证 1: 调用次数 ----
	callCount := mockModel.getCallCount()
	if callCount < 2 {
		t.Fatalf("期望 ≥2 次模型调用，实际 %d", callCount)
	}
	t.Logf("  模型调用次数: %d", callCount)

	// ---- 验证 2: 第 1 次调用包含用户消息 ----
	firstInput := mockModel.getInput(0)
	if firstInput == nil {
		t.Fatal("第 1 次调用输入为 nil")
	}
	hasUserMsg := false
	for _, msg := range firstInput {
		if msg.Role == schema.UserRole && strings.Contains(msg.Content, "PPT") {
			hasUserMsg = true
			break
		}
	}
	if !hasUserMsg {
		t.Error("第 1 次调用缺少用户消息")
	} else {
		t.Logf("  第 1 次调用: %d 条消息，包含用户请求", len(firstInput))
	}

	// ---- 验证 3: 第 2 次调用包含工具结果 ----
	secondInput := mockModel.getInput(1)
	if secondInput == nil {
		t.Fatal("第 2 次调用输入为 nil")
	}

	hasToolResult := false
	hasPPTXContent := false
	hasAssistantToolCall := false
	var toolResultContent string

	for _, msg := range secondInput {
		// assistant 消息应包含 tool_calls
		if msg.Role == schema.AssistantRole && len(msg.ToolCalls) > 0 {
			hasAssistantToolCall = true
			for _, tc := range msg.ToolCalls {
				t.Logf("  assistant tool_call: %s(id=%s)", tc.Function.Name, tc.ID)
			}
		}
		// tool 消息应包含 pptx 指令
		if msg.Role == schema.ToolRole {
			hasToolResult = true
			toolResultContent = msg.Content
			// PPTX 指令正文应包含这些关键词
			keywords := []string{"PPTX Skill", "Quick Reference", "pptxgenjs", "Design Ideas"}
			for _, kw := range keywords {
				if strings.Contains(msg.Content, kw) {
					hasPPTXContent = true
					break
				}
			}
			t.Logf("  tool result: %d 字符, tool_call_id=%s", len(msg.Content), msg.ToolCallID)
		}
	}

	if !hasAssistantToolCall {
		t.Error("第 2 次调用中 assistant 消息缺少 tool_calls")
	}
	if !hasToolResult {
		t.Error("第 2 次调用缺少 tool 角色的消息（工具执行结果）")
	}
	if !hasPPTXContent {
		t.Errorf("工具结果中未包含 PPTX 指令关键词。内容前 200 字符: %.200s", toolResultContent)
	}

	// ---- 验证 4: 最终响应 ----
	if resp == nil {
		t.Fatal("最终响应为 nil")
	}
	if resp.Role != schema.AssistantRole {
		t.Errorf("响应角色 = %q, 期望 assistant", resp.Role)
	}
	if resp.Content == "" {
		t.Error("最终响应内容为空")
	}

	t.Logf("✓ Agent 完整工具调用循环通过")
	t.Logf("  用户: %s", userMsg)
	t.Logf("  Agent: %s", resp.Content)
}

// ============================================================================
// Test 3: 流式模式工具调用循环
// ============================================================================

func TestPPTXAgent_MockStreamRoundTrip(t *testing.T) {
	_, toolReg, _ := setupPPTXEnv(t)

	mockModel := newMockPPTXAgentModel()
	ag := agent.NewAgent(mockModel, toolReg,
		agent.WithSessionID("test-pptx-stream"),
		agent.WithMaxToolRounds(5),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var receivedChunks []string
	var toolCallSeen bool

	resp, err := ag.SendStream(ctx, "做一个关于大模型的 PPT", func(msg *schema.Message, isToolCall bool) bool {
		if isToolCall {
			toolCallSeen = true
			t.Logf("  [chunk] 工具调用: %d 个 tool_calls", len(msg.ToolCalls))
		}
		if msg.Content != "" {
			receivedChunks = append(receivedChunks, msg.Content)
		}
		return true // 继续接收
	})
	if err != nil {
		t.Fatalf("Agent.SendStream 失败: %v", err)
	}
	if resp == nil {
		t.Fatal("流式最终响应为 nil")
	}

	t.Logf("✓ 流式模式完成:")
	t.Logf("  chunks=%d, toolCallSeen=%v, 调用次数=%d",
		len(receivedChunks), toolCallSeen, mockModel.getCallCount())
	t.Logf("  最终响应: %.200s", resp.Content)
}

// ============================================================================
// Test 4: 真实 LLM 端到端
// ============================================================================

func TestPPTXAgent_RealLLM(t *testing.T) {
	// 环境变量检查
	baseURL := os.Getenv("LLM_BASE_URL")
	apiKey := os.Getenv("LLM_API_KEY")
	modelName := os.Getenv("LLM_MODEL")

	if baseURL == "" || apiKey == "" || modelName == "" {
		t.Skip("未设置 LLM_BASE_URL / LLM_API_KEY / LLM_MODEL，跳过真实 LLM 测试")
	}

	skillReg, toolReg, _ := setupPPTXEnv(t)
	sb := sandbox.NewProcessSandbox(sandbox.ProcessConfig{})
	sandbox.RegisterSandboxTools(toolReg, sb)

	// 获取 PPTX Skill 的工具定义，传给 LLM
	sk, _ := skillReg.Get("pptx")
	meta := sk.ToToolMetadata()

	t.Logf("向 LLM 注册工具: %q", meta.Name)

	t.Logf("LLM 可使用的工具%v", toolReg.GetEnabledTools())
	// ---- 方式1: 直接 HTTP 调用（避免依赖 openai 包）----
	// 验证 LLM 能否看到 pptx 工具并决定调用它
	t.Run("LLM_CanSeeTool", func(t *testing.T) {
		testLLMCanSeeTool(t, baseURL, apiKey, modelName, meta)
	})

	// ---- 方式2: 通过 Agent 完整调用 ----
	// 如果 openai 包可用，取消下面注释并使用 Agent

	t.Run("Agent_FullLoop", func(t *testing.T) {
		ctx := context.Background()
		chatModel, err := openai.NewChatModel(&openai.ChatModelConfig{
			BaseURL:             baseURL,
			APIKey:              apiKey,
			Model:               modelName,
			MaxCompletionTokens: 4096,
			Temperature:         0.7,
			TimeOut:             120 * time.Second,
			Thinking: openai.Thinking{
				Type: openai.Disabled,
				Keep: openai.Null,
			},
			Tools: toolReg.GetEnabledTools(),
		})
		if err != nil {
			t.Fatalf("创建 ChatModel 失败: %v", err)
		}

		ag := agent.NewAgent(chatModel, toolReg,
			agent.WithSessionID("test-pptx-real"),
			agent.WithMaxToolRounds(100),
		)

		resp, err := ag.Send(ctx, "帮我创建一个关于 AI 趋势的 3 页 PPT 演示文稿，使用深色主题")
		if err != nil {
			t.Fatalf("Agent.Send 失败: %v", err)
		}

		t.Logf("Agent 最终响应:\n%s", resp.Content)
	})

}

// testLLMCanSeeTool 验证 LLM 能否看到 pptx 工具并决定调用它
func testLLMCanSeeTool(t *testing.T, baseURL, apiKey, modelName string, meta tools.ToolMetadata) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 构造请求体
	paramsJSON, _ := json.Marshal(meta.Parameters)
	descJSON, _ := json.Marshal(meta.Description)

	toolsJSON := fmt.Sprintf(`[{
		"type": "function",
		"function": {
			"name": %q,
			"description": %s,
			"parameters": %s
		}
	}]`, meta.Name, string(descJSON), string(paramsJSON))

	modelJSON, _ := json.Marshal(modelName)

	reqBody := fmt.Sprintf(`{
		"model": %s,
		"messages": [
			{"role": "system", "content": "你是一个专业的演示文稿制作助手。当用户需要制作 PPT、slides 或 presentation 时，使用 pptx 工具获取制作指南。"},
			{"role": "user", "content": "帮我创建一个关于 AI 趋势的 PPT 演示文稿"}
		],
		"tools": %s,
		"max_completion_tokens": 2048,
		"temperature": 0.3
	}`, string(modelJSON), toolsJSON)

	apiURL := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("创建 HTTP 请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp.Body)
		t.Fatalf("API 返回 %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	// 解析响应
	var result struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if len(result.Choices) == 0 {
		t.Fatal("API 返回空 choices")
	}

	msg := result.Choices[0].Message

	t.Logf("LLM 响应:")
	t.Logf("  role: %s", msg.Role)
	t.Logf("  finish_reason: %s", result.Choices[0].FinishReason)
	t.Logf("  content: %s", truncate(msg.Content, 300))
	t.Logf("  usage: prompt=%d completion=%d total=%d",
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)

	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			t.Logf("  tool_call: %s(id=%s, args=%s)", tc.Function.Name, tc.ID, tc.Function.Arguments)
		}

		// 验证是否调用了 pptx 工具
		calledPPTX := false
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name == "pptx" {
				calledPPTX = true
				break
			}
		}

		if calledPPTX {
			t.Logf("✓ LLM 正确调用了 pptx 工具")

			// ---- 完整往返：将工具结果发回 LLM ----
			testFullRoundTrip(t, baseURL, apiKey, modelName, meta, msg)
		} else {
			t.Logf("⚠️  LLM 调用了其他工具，未调用 pptx（名称: %s）", msg.ToolCalls[0].Function.Name)
		}
	} else {
		t.Logf("⚠️  LLM 未返回工具调用，直接回复了文本")
		t.Logf("   这可能是因为模型倾向于直接回答而非调用工具")
	}
}

// testFullRoundTrip 完成一次完整的工具调用往返
// LLM 调用 pptx → 执行工具 → 将结果发回 LLM → LLM 给出最终回复
func testFullRoundTrip(t *testing.T, baseURL, apiKey, modelName string, meta tools.ToolMetadata, firstMsg struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}) {
	t.Helper()

	// 执行 pptx 工具（模拟 Agent 的 handleToolCalls）
	// 使用 SkillLoader 注册的 handler
	skillReg := skill.NewSkillRegistry()
	toolReg := tools.NewToolRegistry()
	loader := skill.NewSkillLoader(skillReg, toolReg)

	dir := locatePPTXSkill()
	if dir == "" {
		t.Skip("无法定位 PPTX Skill")
	}
	loader.LoadFromFile(filepath.Join(dir, "SKILL.md"))

	// 通过 ToolRegistry.ExecuteBatch 执行工具调用
	var toolCallsForBatch []schema.ToolCall
	for _, tc := range firstMsg.ToolCalls {
		toolCallsForBatch = append(toolCallsForBatch, schema.ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: schema.FunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	results := toolReg.ExecuteBatch(context.Background(), toolCallsForBatch)
	toolResultMsgs := schema.ToolResultsMessage(results)

	// 构造第二轮请求的消息
	firstAssistantJSON, _ := json.Marshal(struct {
		Role      string            `json:"role"`
		Content   string            `json:"content"`
		ToolCalls []schema.ToolCall `json:"tool_calls"`
	}{
		Role:      "assistant",
		Content:   firstMsg.Content,
		ToolCalls: toolCallsForBatch,
	})

	var messagesBuilder strings.Builder
	messagesBuilder.WriteString(`[
		{"role": "system", "content": "你是专业的演示文稿制作助手。使用工具获取制作指南后，按指南要求完成任务。"},
		{"role": "user", "content": "帮我创建一个关于 AI 趋势的 PPT 演示文稿"},
		` + string(firstAssistantJSON) + `,`)

	// 添加工具结果消息
	for i, tr := range toolResultMsgs {
		trJSON, _ := json.Marshal(struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		}{
			Role:       string(tr.Role),
			Content:    tr.Content,
			ToolCallID: tr.ToolCallID,
		})
		if i > 0 {
			messagesBuilder.WriteString(",")
		}
		messagesBuilder.WriteString(string(trJSON))
	}
	messagesBuilder.WriteString(`]`)

	modelJSON, _ := json.Marshal(modelName)
	paramsJSON, _ := json.Marshal(meta.Parameters)
	descJSON, _ := json.Marshal(meta.Description)

	toolsJSON := fmt.Sprintf(`[{
		"type": "function",
		"function": {
			"name": %q,
			"description": %s,
			"parameters": %s
		}
	}]`, meta.Name, string(descJSON), string(paramsJSON))

	reqBody := fmt.Sprintf(`{
		"model": %s,
		"messages": %s,
		"tools": %s,
		"max_completion_tokens": 4096,
		"temperature": 0.3
	}`, string(modelJSON), messagesBuilder.String(), toolsJSON)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	apiURL := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, _ := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("第二轮请求失败: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}

	json.NewDecoder(resp.Body).Decode(&result)

	if len(result.Choices) > 0 {
		finalMsg := result.Choices[0].Message
		t.Logf("第二轮 LLM 响应:")
		t.Logf("  content (%d字符): %s", len(finalMsg.Content), truncate(finalMsg.Content, 500))
		t.Logf("  tool_calls: %d", len(finalMsg.ToolCalls))
		t.Logf("  usage: total=%d", result.Usage.TotalTokens)

		if len(finalMsg.ToolCalls) > 0 {
			for _, tc := range finalMsg.ToolCalls {
				t.Logf("  后续工具调用: %s", tc.Function.Name)
			}
		}

		if len(finalMsg.Content) > 0 {
			t.Logf("✓ 完整工具调用往返成功: LLM 获取到 PPTX 指令后给出了具体方案")
		}
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, err
		}
	}
}
