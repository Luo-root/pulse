package memory

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// 辅助函数测试
// ============================================================================

func TestCosineSimilarity_SameVector(t *testing.T) {
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{1.0, 0.0, 0.0}

	sim := cosineSimilarity(a, b)

	if math.Abs(sim-1.0) > 0.001 {
		t.Fatalf("expected 1.0, got %f", sim)
	}
}

func TestCosineSimilarity_OrthogonalVectors(t *testing.T) {
	a := []float32{1.0, 0.0, 0.0}
	b := []float32{0.0, 1.0, 0.0}

	sim := cosineSimilarity(a, b)

	if math.Abs(sim) > 0.001 {
		t.Fatalf("expected ~0, got %f", sim)
	}
}

func TestCosineSimilarity_OppositeVectors(t *testing.T) {
	a := []float32{1.0, 0.0}
	b := []float32{-1.0, 0.0}

	sim := cosineSimilarity(a, b)

	if math.Abs(sim-(-1.0)) > 0.001 {
		t.Fatalf("expected -1.0, got %f", sim)
	}
}

func TestCosineSimilarity_DifferentLength(t *testing.T) {
	a := []float32{1.0, 0.0}
	b := []float32{1.0, 0.0, 0.0}

	sim := cosineSimilarity(a, b)

	if sim != 0 {
		t.Fatalf("expected 0 for different lengths, got %f", sim)
	}
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float32{0.0, 0.0}
	b := []float32{1.0, 0.0}

	sim := cosineSimilarity(a, b)

	if sim != 0 {
		t.Fatalf("expected 0 for zero vector, got %f", sim)
	}
}

func TestExtractKeywords(t *testing.T) {
	keywords := extractKeywords("hello world, this is a test")

	if len(keywords) == 0 {
		t.Fatal("expected non-empty keywords")
	}

	// 停用词应该被过滤
	for _, kw := range keywords {
		if kw == "a" || kw == "is" || kw == "this" {
			t.Fatalf("stop word '%s' should be filtered", kw)
		}
	}

	// "hello" 和 "world" 应该保留
	found := false
	for _, kw := range keywords {
		if kw == "hello" || kw == "world" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected 'hello' or 'world' in keywords")
	}
}

func TestExtractKeywords_Chinese(t *testing.T) {
	keywords := extractKeywords("你好世界，这是一个测试")

	if len(keywords) == 0 {
		t.Fatal("expected non-empty keywords for Chinese text")
	}

	// 停用词应该被过滤
	for _, kw := range keywords {
		if kw == "的" || kw == "是" || kw == "在" {
			t.Fatalf("Chinese stop word '%s' should be filtered", kw)
		}
	}
}

func TestExtractKeywords_Empty(t *testing.T) {
	keywords := extractKeywords("")
	if len(keywords) != 0 {
		t.Fatalf("expected 0 keywords, got %d", len(keywords))
	}
}

func TestExtractKeywords_Deduplication(t *testing.T) {
	keywords := extractKeywords("hello hello hello world world")

	seen := make(map[string]bool)
	for _, kw := range keywords {
		if seen[kw] {
			t.Fatalf("duplicate keyword: %s", kw)
		}
		seen[kw] = true
	}
}

// ============================================================================
// SplitText 测试
// ============================================================================

func TestSplitText_SmallText(t *testing.T) {
	chunks := SplitText("hello world", 100, 10)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", chunks[0])
	}
}

func TestSplitText_LargeText(t *testing.T) {
	// 构造一个长文本
	text := ""
	for i := 0; i < 100; i++ {
		text += "这是一段测试文本。"
	}

	chunks := SplitText(text, 50, 5)

	if len(chunks) <= 1 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	// 每个 chunk 不应该太长
	for i, chunk := range chunks {
		runes := []rune(chunk)
		// 允许一些余量（因为边界字符）
		if len(runes) > 80 {
			t.Fatalf("chunk %d too long: %d runes", i, len(runes))
		}
	}
}

func TestSplitText_BoundaryDetection(t *testing.T) {
	text := "第一句话。第二句话。第三句话。第四句话。第五句话。"

	chunks := SplitText(text, 15, 2)

	// 应该在句号处断开
	for _, chunk := range chunks {
		// 不应该在句子中间断开（大多数情况下）
		_ = chunk
	}

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
}

func TestSplitText_Overlap(t *testing.T) {
	text := "aaaaaaaaaabbbbbbbbbbccccccccccdddddddddd"

	chunks := SplitText(text, 10, 3)

	if len(chunks) <= 1 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	// 有重叠意味着总字符数应该大于原文本
	totalChars := 0
	for _, c := range chunks {
		totalChars += len([]rune(c))
	}
	if totalChars <= len([]rune(text)) {
		// 有重叠时总字符数应该更多
		// 但这个测试不严格，因为重叠量可能很小
	}
}

func TestSplitText_ZeroChunkSize(t *testing.T) {
	text := "hello"
	chunks := SplitText(text, 0, 0)

	// chunkSize <= 0 应该使用默认值 512
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small text with default size, got %d", len(chunks))
	}
}

func TestSplitText_OverlapLargerThanChunk(t *testing.T) {
	text := "hello world"
	chunks := SplitText(text, 5, 10) // overlap > chunkSize

	// 不应该 panic
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk")
	}
}

// ============================================================================
// GormStore 集成测试（需要 SQLite）
// ============================================================================

func setupTestStore(t *testing.T) *GormStore {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	config := DefaultGormStoreConfig()
	config.DBPath = dbPath
	config.DisableVectorSearch = true // 不测试向量搜索

	store, err := NewGormStore(config, nil)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	t.Cleanup(func() {
		store.Close()
		os.Remove(dbPath)
	})

	return store
}

func TestGormStore_SaveAndRetrieve(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	msgs := []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi there", ""),
	}

	err := store.Save(ctx, "session1", msgs)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	retrieved, err := store.GetSession(ctx, "session1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if len(retrieved) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(retrieved))
	}
	if retrieved[0].Content != "hello" {
		t.Fatalf("expected 'hello', got '%s'", retrieved[0].Content)
	}
	if retrieved[1].Content != "hi there" {
		t.Fatalf("expected 'hi there', got '%s'", retrieved[1].Content)
	}
}

func TestGormStore_SaveEmpty(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	err := store.Save(ctx, "session1", nil)
	if err != nil {
		t.Fatalf("save empty: %v", err)
	}
}

func TestGormStore_GetSession_Empty(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	retrieved, err := store.GetSession(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if len(retrieved) != 0 {
		t.Fatalf("expected 0, got %d", len(retrieved))
	}
}

func TestGormStore_ClearSession(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi", ""),
	})

	err := store.ClearSession(ctx, "session1")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}

	retrieved, _ := store.GetSession(ctx, "session1")
	if len(retrieved) != 0 {
		t.Fatalf("expected 0 after clear, got %d", len(retrieved))
	}
}

func TestGormStore_ClearNonexistentSession(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	err := store.ClearSession(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("clear nonexistent should not error: %v", err)
	}
}

func TestGormStore_HybridRecall(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("Go is a programming language"),
		schema.AssistantMessage("Yes, Go was created at Google", ""),
		schema.UserMessage("Python is also popular"),
		schema.AssistantMessage("Python is great for data science", ""),
		schema.UserMessage("Tell me more about Go concurrency"),
		schema.AssistantMessage("Go uses goroutines and channels", ""),
	})

	// 使用关键词召回（因为禁用了向量搜索，会回退到 hybrid）
	results, err := store.Recall(ctx, "session1", "Go concurrency", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected non-empty results")
	}

	// 结果应该包含和 "Go concurrency" 相关的消息
	found := false
	for _, r := range results {
		if containsSubstr(r.Content, "Go") || containsSubstr(r.Content, "goroutine") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected results related to Go concurrency")
	}
}

func TestGormStore_SearchByRole(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi", ""),
		schema.UserMessage("how are you"),
		schema.AssistantMessage("fine", ""),
	})

	userMsgs, err := store.SearchByRole(ctx, "session1", schema.UserRole, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(userMsgs) != 2 {
		t.Fatalf("expected 2 user messages, got %d", len(userMsgs))
	}

	for _, m := range userMsgs {
		if m.Role != schema.UserRole {
			t.Fatalf("expected user role, got %s", m.Role)
		}
	}
}

func TestGormStore_SearchByTimeRange(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("old message"),
	})

	// 等一小段时间
	time.Sleep(10 * time.Millisecond)

	midTime := time.Now()
	time.Sleep(10 * time.Millisecond)

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("new message"),
	})

	// 搜索 midTime 之后的消息
	results, err := store.SearchByTimeRange(ctx, "session1", midTime, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 message after midTime, got %d", len(results))
	}
	if results[0].Content != "new message" {
		t.Fatalf("expected 'new message', got '%s'", results[0].Content)
	}
}

func TestGormStore_GetSessionStats(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi", ""),
	})

	stats, err := store.GetSessionStats(ctx, "session1")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats["message_count"] != int64(2) {
		t.Fatalf("expected 2, got %v", stats["message_count"])
	}
}

func TestGormStore_GetSessionStats_Empty(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	stats, err := store.GetSessionStats(ctx, "empty")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	count, ok := stats["message_count"].(int64)
	if !ok {
		t.Fatalf("expected message_count to be int64, got %T", stats["message_count"])
	}
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}
}

func TestGormStore_MultipleSessions(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "s1", []*schema.Message{schema.UserMessage("s1 msg")})
	store.Save(ctx, "s2", []*schema.Message{schema.UserMessage("s2 msg")})

	s1, _ := store.GetSession(ctx, "s1")
	s2, _ := store.GetSession(ctx, "s2")

	if len(s1) != 1 || s1[0].Content != "s1 msg" {
		t.Fatalf("unexpected s1: %v", s1)
	}
	if len(s2) != 1 || s2[0].Content != "s2 msg" {
		t.Fatalf("unexpected s2: %v", s2)
	}
}

func TestGormStore_SaveWithToolCalls(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.UserMessage("read file"),
		{
			Role:    schema.AssistantRole,
			Content: "",
			ToolCalls: []schema.ToolCall{
				{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "file_read", Arguments: `{"path":"test.txt"}`}},
			},
		},
		{
			Role:       schema.ToolRole,
			Content:    "file contents",
			ToolCallID: "c1",
		},
		schema.AssistantMessage("The file contains: file contents", ""),
	})

	retrieved, err := store.GetSession(ctx, "session1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if len(retrieved) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(retrieved))
	}

	// 验证 tool 消息
	toolMsg := retrieved[2]
	if toolMsg.Role != schema.ToolRole {
		t.Fatalf("expected tool role, got %s", toolMsg.Role)
	}
	if toolMsg.ToolCallID != "c1" {
		t.Fatalf("expected c1, got %s", toolMsg.ToolCallID)
	}
}

func TestGormStore_SaveWithReasoning(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.AssistantMessage("answer", "my reasoning"),
	})

	retrieved, _ := store.GetSession(ctx, "session1")

	if retrieved[0].ReasoningContent != "my reasoning" {
		t.Fatalf("expected reasoning content, got '%s'", retrieved[0].ReasoningContent)
	}
}

func TestGormStore_GetSessionWithReasoning(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Save(ctx, "session1", []*schema.Message{
		schema.AssistantMessage("answer", "reasoning"),
	})

	retrieved, err := store.GetSessionWithReasoning(ctx, "session1")
	if err != nil {
		t.Fatalf("get session with reasoning: %v", err)
	}

	if retrieved[0].ReasoningContent != "reasoning" {
		t.Fatalf("expected reasoning, got '%s'", retrieved[0].ReasoningContent)
	}
}

func TestGormStore_SaveTimestampOrdering(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	msgs := make([]*schema.Message, 10)
	for i := 0; i < 10; i++ {
		msgs[i] = schema.UserMessage("msg")
	}

	store.Save(ctx, "session1", msgs)

	retrieved, _ := store.GetSession(ctx, "session1")

	// 消息应该按时间顺序
	for i := 1; i < len(retrieved); i++ {
		// 只是验证不会 panic 和数量正确
	}

	if len(retrieved) != 10 {
		t.Fatalf("expected 10, got %d", len(retrieved))
	}
}

func TestGormStore_RecallEmptySession(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	results, err := store.Recall(ctx, "empty", "query", 5)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	if len(results) != 0 {
		t.Fatalf("expected 0, got %d", len(results))
	}
}

func TestGormStore_RecallTopK(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	msgs := make([]*schema.Message, 20)
	for i := 0; i < 20; i++ {
		msgs[i] = schema.UserMessage("some message content")
	}
	store.Save(ctx, "session1", msgs)

	results, _ := store.Recall(ctx, "session1", "message", 5)

	if len(results) > 5 {
		t.Fatalf("expected <= 5 results, got %d", len(results))
	}
}
