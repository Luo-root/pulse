package gorm

import (
	"context"
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

	store, err := NewStore(config, nil)
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

	// 召回
	recalled, err := store.Recall(ctx, sessionID, "Hello", 3)
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	if len(recalled) == 0 {
		t.Error("expected recalled messages, got none")
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

	store, err := NewStore(config, mockEmbedding)
	if err != nil {
		t.Fatalf("create store failed: %v", err)
	}
	defer store.Close()

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

	// 向量召回
	recalled, err := store.Recall(ctx, sessionID, "Go programming", 2)
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

	store, err := NewStore(config, nil)
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

	store, err := NewStore(config, nil)
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

	store, err := NewStore(config, nil)
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
