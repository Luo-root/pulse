package gorm

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// mockEmbedding 模拟嵌入函数
func mockEmbedding(ctx context.Context, text string) ([]float32, error) {
	// 简单的模拟：根据文本长度生成向量
	vec := make([]float32, 384)
	for i := 0; i < len(text) && i < 384; i++ {
		vec[i] = float32(text[i]) / 255.0
	}
	return vec, nil
}

// mockEmbedderBagOfChars 字符频率嵌入器（确定性，相同文本=相同向量）
type mockEmbedderBagOfChars struct {
	dim int
}

func (e *mockEmbedderBagOfChars) Embed(ctx context.Context, text string) ([]float32, error) {
	vec := make([]float32, e.dim)
	runes := []rune(text)
	for _, r := range runes {
		idx := int(uint32(r)) % e.dim
		vec[idx] += 1.0
	}
	allZero := true
	for _, v := range vec {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		vec[0] = 1.0
	}
	var normSq float64
	for _, v := range vec {
		normSq += float64(v) * float64(v)
	}
	if normSq > 0 {
		norm := math.Sqrt(normSq)
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}
	return vec, nil
}

func (e *mockEmbedderBagOfChars) Embedder() EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		return e.Embed(ctx, text)
	}
}

func TestGormStoreBasic(t *testing.T) {
	dbPath := "./test_gorm.db"
	defer os.Remove(dbPath)

	config := &Config{
		DBPath:              dbPath,
		MaxOpenConns:        5,
		MaxIdleConns:        2,
		ConnMaxLifetime:     time.Hour,
		DisableVectorSearch: true,
	}

	store, err := NewGORMStore(config, nil)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := "test-session"

	// 保存消息
	msgs := []*schema.Message{
		schema.UserMessage("Hello"),
		schema.AssistantMessage("Hi there!", ""),
	}

	if err := store.Save(ctx, sessionID, msgs); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// 获取会话
	history, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session failed: %v", err)
	}
	if len(history) != 2 {
		t.Errorf("expected 2 messages, got %d", len(history))
	}
}

func TestGormStoreWithVector(t *testing.T) {
	dbPath := "./test_gorm_vector.db"
	defer os.Remove(dbPath)

	config := &Config{
		DBPath:              dbPath,
		DisableVectorSearch: false,
		EmbeddingDimension:  384,
	}

	store, err := NewGORMStore(config, mockEmbedding)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}

	retriever := NewHNSWRetriever(store.GetDB(), mockEmbedding, config)
	t.Cleanup(func() { retriever.Close(); store.Close() })

	ctx := context.Background()
	sessionID := "test-vector"

	// 保存消息
	msgs := []*schema.Message{
		schema.UserMessage("I love programming in Go"),
		schema.AssistantMessage("Go is a great language!", ""),
		schema.UserMessage("Python is also nice"),
	}

	if err := store.Save(ctx, sessionID, msgs); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// 等待索引就绪
	deadline := time.Now().Add(3 * time.Second)
	for !retriever.IndexReady() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	// 向量召回
	recalled, err := retriever.Recall(ctx, sessionID, "Go programming", 2)
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected vector recalled messages, got none")
	}
}

func TestGormStoreClearSession(t *testing.T) {
	dbPath := "./test_gorm_clear.db"
	defer os.Remove(dbPath)

	config := DefaultConfig()
	config.DBPath = dbPath

	store, err := NewGORMStore(config, nil)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := "test-clear"

	// 保存消息
	store.Save(ctx, sessionID, []*schema.Message{
		schema.UserMessage("Test"),
	})

	// 清空
	if err := store.ClearSession(ctx, sessionID); err != nil {
		t.Fatalf("clear failed: %v", err)
	}

	// 验证清空
	history, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session after clear failed: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected 0 messages after clear, got %d", len(history))
	}
}

func TestGormStoreTimeRange(t *testing.T) {
	dbPath := "./test_gorm_time.db"
	defer os.Remove(dbPath)

	config := DefaultConfig()
	config.DBPath = dbPath

	store, err := NewGORMStore(config, nil)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := "test-time"

	// 保存消息
	store.Save(ctx, sessionID, []*schema.Message{
		schema.UserMessage("Old message"),
	})

	time.Sleep(100 * time.Millisecond)

	store.Save(ctx, sessionID, []*schema.Message{
		schema.UserMessage("New message"),
	})

	// 按时间范围搜索
	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(time.Hour)

	results, err := store.SearchByTimeRange(ctx, sessionID, start, end)
	if err != nil {
		t.Fatalf("search by time range failed: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 messages, got %d", len(results))
	}
}

func TestGormStoreStats(t *testing.T) {
	dbPath := "./test_gorm_stats.db"
	defer os.Remove(dbPath)

	config := DefaultConfig()
	config.DBPath = dbPath

	store, err := NewGORMStore(config, nil)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sessionID := "test-stats"

	// 保存消息
	store.Save(ctx, sessionID, []*schema.Message{
		schema.UserMessage("Message 1"),
		schema.AssistantMessage("Reply 1", ""),
		schema.UserMessage("Message 2"),
	})

	// 获取统计
	stats, err := store.GetSessionStats(ctx, sessionID)
	if err != nil {
		t.Fatalf("get stats failed: %v", err)
	}

	count, ok := stats["message_count"].(int64)
	if !ok || count != 3 {
		t.Errorf("expected 3 messages, got %v", stats["message_count"])
	}
}

func TestCosineSimilarity(t *testing.T) {
	// 相同向量
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if sim := cosineSimilarity(a, b); sim != 1.0 {
		t.Errorf("expected 1.0 for identical vectors, got %f", sim)
	}

	// 正交向量
	c := []float32{1, 0, 0}
	d := []float32{0, 1, 0}
	if sim := cosineSimilarity(c, d); sim != 0.0 {
		t.Errorf("expected 0.0 for orthogonal vectors, got %f", sim)
	}

	// 反向向量
	e := []float32{1, 0, 0}
	f := []float32{-1, 0, 0}
	if sim := cosineSimilarity(e, f); sim != -1.0 {
		t.Errorf("expected -1.0 for opposite vectors, got %f", sim)
	}
}

// ============================================================================
// 召回模式测试
// ============================================================================

func setupStore(t *testing.T, config *Config, embedding EmbeddingFunc) *GORMStore {
	t.Helper()
	if config == nil {
		config = DefaultConfig()
	}
	tmpDir := t.TempDir()
	config.DBPath = tmpDir + "/test.db"
	store, err := NewGORMStore(config, embedding)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func setupCompositeStore(t *testing.T, config *Config, embedding EmbeddingFunc) *retrieverTestHelper {
	t.Helper()
	if config == nil {
		config = DefaultConfig()
	}
	tmpDir := t.TempDir()
	config.DBPath = tmpDir + "/test.db"
	store, err := NewGORMStore(config, embedding)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	retriever := NewHNSWRetriever(store.GetDB(), embedding, config)
	t.Cleanup(func() { retriever.Close(); store.Close() })
	return &retrieverTestHelper{store: store, retriever: retriever}
}

type retrieverTestHelper struct {
	store     *GORMStore
	retriever *HNSWRetriever
}

func (h *retrieverTestHelper) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	return h.store.Save(ctx, sessionID, msgs)
}

func (h *retrieverTestHelper) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	return h.retriever.Recall(ctx, sessionID, query, topK)
}

func (h *retrieverTestHelper) IndexReady() bool {
	return h.retriever.IndexReady()
}

func seedMessages(t *testing.T, store *GORMStore, sessionID string, count int) {
	t.Helper()
	msgs := make([]*schema.Message, count)
	for i := 0; i < count; i++ {
		if i%2 == 0 {
			msgs[i] = schema.UserMessage(fmt.Sprintf("user message %d about Go programming", i))
		} else {
			msgs[i] = schema.AssistantMessage(fmt.Sprintf("assistant reply %d about Go language", i), "")
		}
	}
	if err := store.Save(context.Background(), sessionID, msgs); err != nil {
		t.Fatalf("seed save: %v", err)
	}
}

func seedMessagesComposite(t *testing.T, h *retrieverTestHelper, sessionID string, count int) {
	t.Helper()
	msgs := make([]*schema.Message, count)
	for i := 0; i < count; i++ {
		if i%2 == 0 {
			msgs[i] = schema.UserMessage(fmt.Sprintf("user message %d about Go programming", i))
		} else {
			msgs[i] = schema.AssistantMessage(fmt.Sprintf("assistant reply %d about Go language", i), "")
		}
	}
	if err := h.Save(context.Background(), sessionID, msgs); err != nil {
		t.Fatalf("seed save: %v", err)
	}
}

func TestRecall_HybridMode(t *testing.T) {
	config := DefaultConfig()
	config.RecallMode = RecallModeHybrid
	config.DisableVectorSearch = true
	h := setupCompositeStore(t, config, nil)

	seedMessagesComposite(t, h, "s1", 6)

	recalled, err := h.Recall(context.Background(), "s1", "Go programming", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages")
	}
}

func TestRecall_VectorMode_Fallback(t *testing.T) {
	// Vector 模式，有 embedding
	config := DefaultConfig()
	config.RecallMode = RecallModeVector
	config.EmbeddingDimension = 384
	emb := &mockEmbedderBagOfChars{dim: 384}
	h := setupCompositeStore(t, config, emb.Embedder())

	seedMessagesComposite(t, h, "s1", 6)

	// 等待索引就绪
	deadline := time.Now().Add(3 * time.Second)
	for !h.IndexReady() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	recalled, err := h.Recall(context.Background(), "s1", "Go programming", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages")
	}
}

func TestRecall_VectorMode_WithEmbedding(t *testing.T) {
	// Vector 模式，有 embedding，验证向量召回路径
	config := DefaultConfig()
	config.RecallMode = RecallModeVector
	config.EmbeddingDimension = 384
	emb := &mockEmbedderBagOfChars{dim: 384}
	h := setupCompositeStore(t, config, emb.Embedder())

	seedMessagesComposite(t, h, "s1", 6)

	// 等待索引就绪
	deadline := time.Now().Add(3 * time.Second)
	for !h.IndexReady() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	recalled, err := h.Recall(context.Background(), "s1", "Go programming", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages")
	}
}

func TestRecall_VectorMode_NilEmbedding_FallbackToHybrid(t *testing.T) {
	// Vector 模式但没有 embedding 函数，应该 fallback 到 hybrid 而非 panic
	config := DefaultConfig()
	config.RecallMode = RecallModeVector
	config.DisableVectorSearch = true
	h := setupCompositeStore(t, config, nil)

	seedMessagesComposite(t, h, "s1", 4)

	recalled, err := h.Recall(context.Background(), "s1", "Go", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages from hybrid fallback")
	}
}

func TestRecall_CombinedMode_WithEmbedding(t *testing.T) {
	config := DefaultConfig()
	config.RecallMode = RecallModeCombined
	config.EmbeddingDimension = 384
	emb := &mockEmbedderBagOfChars{dim: 384}
	h := setupCompositeStore(t, config, emb.Embedder())

	seedMessagesComposite(t, h, "s1", 6)

	// 等待索引就绪
	deadline := time.Now().Add(3 * time.Second)
	for !h.IndexReady() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	recalled, err := h.Recall(context.Background(), "s1", "Go programming", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages")
	}
}

func TestRecall_CombinedMode_NoEmbedding_Fallback(t *testing.T) {
	// Combined 模式但没有 embedding，应该 fallback 到 hybrid
	config := DefaultConfig()
	config.RecallMode = RecallModeCombined
	h := setupCompositeStore(t, config, nil)

	seedMessagesComposite(t, h, "s1", 4)

	recalled, err := h.Recall(context.Background(), "s1", "Go", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages")
	}
}

func TestRecall_AutoMode(t *testing.T) {
	config := DefaultConfig()
	config.RecallMode = RecallModeAuto
	config.DisableVectorSearch = true
	h := setupCompositeStore(t, config, nil)

	seedMessagesComposite(t, h, "s1", 4)

	recalled, err := h.Recall(context.Background(), "s1", "Go", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages")
	}
}

func TestRecall_ZeroTopK_DefaultsTo3(t *testing.T) {
	h := setupCompositeStore(t, nil, nil)
	seedMessagesComposite(t, h, "s1", 10)

	recalled, err := h.Recall(context.Background(), "s1", "Go", 0)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	// topK=0 应该默认为 3
	if len(recalled) > 3 {
		t.Errorf("expected <= 3, got %d", len(recalled))
	}
}

// ============================================================================
// 多模态消息存储
// ============================================================================

func TestSave_MultimodalMessage(t *testing.T) {
	store := setupStore(t, nil, nil)

	msg := schema.UserMultimodalMessage(
		schema.TextPart("描述这张图片"),
		schema.ImagePart("https://example.com/img.png"),
	)

	err := store.Save(context.Background(), "s1", []*schema.Message{msg})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	history, err := store.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1, got %d", len(history))
	}
	// 应该保存了文本部分
	if history[0].Content != "描述这张图片" {
		t.Errorf("content: %s", history[0].Content)
	}
}

func TestSave_ToolCallMessage(t *testing.T) {
	store := setupStore(t, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("查天气"),
		{
			Role:    schema.AssistantRole,
			Content: "",
			ToolCalls: []schema.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: schema.FunctionCall{
						Name:      "get_weather",
						Arguments: `{"city":"北京"}`,
					},
				},
			},
		},
		{
			Role:       schema.ToolRole,
			Content:    "晴天 25°C",
			ToolCallID: "call_1",
		},
		schema.AssistantMessage("北京今天晴天，25°C", ""),
	}

	err := store.Save(context.Background(), "s1", msgs)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	history, err := store.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("expected 4, got %d", len(history))
	}
}

func TestSave_ReasoningContent(t *testing.T) {
	store := setupStore(t, nil, nil)

	msg := schema.AssistantMessage("答案是 42", "让我思考一下...42 是答案")

	err := store.Save(context.Background(), "s1", []*schema.Message{msg})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	history, err := store.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1, got %d", len(history))
	}
	if history[0].ReasoningContent != "让我思考一下...42 是答案" {
		t.Errorf("reasoning: %s", history[0].ReasoningContent)
	}
}

// ============================================================================
// GetSessionWithReasoning
// ============================================================================

func TestGetSessionWithReasoning(t *testing.T) {
	store := setupStore(t, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("问题"),
		schema.AssistantMessage("答案", "推理过程"),
	}
	store.Save(context.Background(), "s1", msgs)

	history, err := store.GetSessionWithReasoning(context.Background(), "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2, got %d", len(history))
	}
	if history[1].ReasoningContent != "推理过程" {
		t.Errorf("reasoning: %s", history[1].ReasoningContent)
	}
}

// ============================================================================
// SearchByRole
// ============================================================================

func TestSearchByRole(t *testing.T) {
	store := setupStore(t, nil, nil)

	msgs := []*schema.Message{
		schema.UserMessage("问题1"),
		schema.AssistantMessage("回答1", ""),
		schema.UserMessage("问题2"),
		schema.AssistantMessage("回答2", ""),
	}
	store.Save(context.Background(), "s1", msgs)

	// 只搜 user 消息
	userMsgs, err := store.SearchByRole(context.Background(), "s1", schema.UserRole, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(userMsgs) != 2 {
		t.Errorf("expected 2 user messages, got %d", len(userMsgs))
	}

	// 只搜 assistant 消息
	assistantMsgs, err := store.SearchByRole(context.Background(), "s1", schema.AssistantRole, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(assistantMsgs) != 2 {
		t.Errorf("expected 2 assistant messages, got %d", len(assistantMsgs))
	}
}

// ============================================================================
// 多会话隔离
// ============================================================================

func TestMultipleSessions_Isolation(t *testing.T) {
	store := setupStore(t, nil, nil)

	store.Save(context.Background(), "session_a", []*schema.Message{
		schema.UserMessage("session A message"),
	})
	store.Save(context.Background(), "session_b", []*schema.Message{
		schema.UserMessage("session B message 1"),
		schema.UserMessage("session B message 2"),
	})

	historyA, _ := store.GetSession(context.Background(), "session_a")
	historyB, _ := store.GetSession(context.Background(), "session_b")

	if len(historyA) != 1 {
		t.Errorf("session A: expected 1, got %d", len(historyA))
	}
	if len(historyB) != 2 {
		t.Errorf("session B: expected 2, got %d", len(historyB))
	}

	// 清空 A 不影响 B
	store.ClearSession(context.Background(), "session_a")
	historyA2, _ := store.GetSession(context.Background(), "session_a")
	historyB2, _ := store.GetSession(context.Background(), "session_b")

	if len(historyA2) != 0 {
		t.Errorf("session A after clear: expected 0, got %d", len(historyA2))
	}
	if len(historyB2) != 2 {
		t.Errorf("session B after clear A: expected 2, got %d", len(historyB2))
	}
}

// ============================================================================
// DefaultConfig
// ============================================================================

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()

	if c.DBPath != "./chat.db" {
		t.Errorf("DBPath: %s", c.DBPath)
	}
	if c.MaxOpenConns != 10 {
		t.Errorf("MaxOpenConns: %d", c.MaxOpenConns)
	}
	if c.MaxIdleConns != 5 {
		t.Errorf("MaxIdleConns: %d", c.MaxIdleConns)
	}
	if c.ConnMaxLifetime != time.Hour {
		t.Errorf("ConnMaxLifetime: %v", c.ConnMaxLifetime)
	}
	if c.DisableVectorSearch != false {
		t.Error("DisableVectorSearch should be false")
	}
	if c.EmbeddingDimension != 768 {
		t.Errorf("EmbeddingDimension: %d", c.EmbeddingDimension)
	}
	if c.RecallMode != RecallModeCombined {
		t.Errorf("RecallMode: %d", c.RecallMode)
	}
	if c.CombinedWeights == nil {
		t.Fatal("CombinedWeights is nil")
	}
	if c.CombinedWeights.VectorWeight != 0.5 {
		t.Errorf("VectorWeight: %f", c.CombinedWeights.VectorWeight)
	}
	if c.ChunkSize != 512 {
		t.Errorf("ChunkSize: %d", c.ChunkSize)
	}
	if c.ChunkOverlap != 64 {
		t.Errorf("ChunkOverlap: %d", c.ChunkOverlap)
	}
}

// ============================================================================
// 边界情况
// ============================================================================

func TestSave_EmptyMessages(t *testing.T) {
	store := setupStore(t, nil, nil)

	err := store.Save(context.Background(), "s1", nil)
	if err != nil {
		t.Fatalf("save nil: %v", err)
	}

	err = store.Save(context.Background(), "s1", []*schema.Message{})
	if err != nil {
		t.Fatalf("save empty: %v", err)
	}
}

func TestGetSession_EmptySession(t *testing.T) {
	store := setupStore(t, nil, nil)

	history, err := store.GetSession(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected 0, got %d", len(history))
	}
}

func TestClearSession_Nonexistent(t *testing.T) {
	store := setupStore(t, nil, nil)

	err := store.ClearSession(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
}

func TestSearchByRole_EmptyResult(t *testing.T) {
	store := setupStore(t, nil, nil)

	results, err := store.SearchByRole(context.Background(), "s1", schema.UserRole, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0, got %d", len(results))
	}
}

func TestSearchByTimeRange_NoResults(t *testing.T) {
	store := setupStore(t, nil, nil)
	seedMessages(t, store, "s1", 2)

	// 搜索未来时间范围
	start := time.Now().Add(24 * time.Hour)
	end := time.Now().Add(48 * time.Hour)

	results, err := store.SearchByTimeRange(context.Background(), "s1", start, end)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0, got %d", len(results))
	}
}

func TestGetSessionStats_Empty(t *testing.T) {
	store := setupStore(t, nil, nil)

	stats, err := store.GetSessionStats(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	count, ok := stats["message_count"].(int64)
	if !ok || count != 0 {
		t.Errorf("expected 0, got %v", stats["message_count"])
	}
}

func TestClose_Idempotent(t *testing.T) {
	store := setupStore(t, nil, nil)

	if err := store.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
}

func TestIndexReady_AfterInit(t *testing.T) {
	config := DefaultConfig()
	config.DisableVectorSearch = true
	h := setupCompositeStore(t, config, nil)

	// 等待后台索引重建完成
	time.Sleep(200 * time.Millisecond)

	if !h.IndexReady() {
		t.Error("index should be ready after init")
	}
}

// ============================================================================
// extractKeywords 测试
// ============================================================================

func TestExtractKeywords(t *testing.T) {
	keywords := extractKeywords("Go programming language is great for building microservices")

	if len(keywords) == 0 {
		t.Fatal("expected keywords")
	}

	// 不应该包含停用词
	for _, kw := range keywords {
		if kw == "is" || kw == "for" || kw == "a" {
			t.Errorf("stop word %q should be filtered", kw)
		}
	}
}

func TestExtractKeywords_Chinese(t *testing.T) {
	keywords := extractKeywords("Go 语言是一种高效的编程语言")

	if len(keywords) == 0 {
		t.Fatal("expected keywords")
	}
}

func TestExtractKeywords_Empty(t *testing.T) {
	keywords := extractKeywords("")
	if len(keywords) != 0 {
		t.Errorf("expected 0, got %d", len(keywords))
	}
}
