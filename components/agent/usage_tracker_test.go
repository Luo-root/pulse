package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel/mock"
	"github.com/Luo-root/pulse/components/schema"
)

func TestUsageTrackerBasic(t *testing.T) {
	tracker := NewUsageTracker()

	// 记录几次调用
	tracker.Record(schema.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, "gpt-4", 100*time.Millisecond)
	tracker.Record(schema.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300}, "gpt-4", 200*time.Millisecond)
	tracker.Record(schema.Usage{PromptTokens: 50, CompletionTokens: 25, TotalTokens: 75}, "gpt-3.5-turbo", 50*time.Millisecond)

	// 验证总统计
	stats := tracker.GetStats()
	if stats.TotalCalls != 3 {
		t.Errorf("expected 3 calls, got %d", stats.TotalCalls)
	}
	if stats.TotalPrompt != 350 {
		t.Errorf("expected 350 prompt tokens, got %d", stats.TotalPrompt)
	}
	if stats.TotalCompletion != 175 {
		t.Errorf("expected 175 CompletionTokens tokens, got %d", stats.TotalCompletion)
	}
	if stats.TotalTokens != 525 {
		t.Errorf("expected 525 total tokens, got %d", stats.TotalTokens)
	}

	// 验证成本（使用近似比较，避免浮点数精度问题）
	// gpt-4: (100+200)/1000 * 0.03 + (50+100)/1000 * 0.06 = 0.009 + 0.009 = 0.018
	// gpt-3.5: 50/1000 * 0.0005 + 25/1000 * 0.0015 = 0.000025 + 0.0000375 = 0.0000625
	// total: 0.018 + 0.0000625 = 0.0180625
	expectedCost := 0.0180625
	if math.Abs(stats.TotalCost-expectedCost) > 1e-9 {
		t.Errorf("expected cost %.7f, got %.7f", expectedCost, stats.TotalCost)
	}

	// 验证平均延迟
	expectedAvgLatency := time.Millisecond * 116 // (100+200+50)/3 = 116.67
	if stats.AverageLatency < expectedAvgLatency-10*time.Millisecond || stats.AverageLatency > expectedAvgLatency+10*time.Millisecond {
		t.Errorf("expected average latency around %v, got %v", expectedAvgLatency, stats.AverageLatency)
	}
}

func TestUsageTrackerModelStats(t *testing.T) {
	tracker := NewUsageTracker()

	tracker.Record(schema.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, "gpt-4", 100*time.Millisecond)
	tracker.Record(schema.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300}, "gpt-4", 200*time.Millisecond)
	tracker.Record(schema.Usage{PromptTokens: 50, CompletionTokens: 25, TotalTokens: 75}, "gpt-3.5-turbo", 50*time.Millisecond)

	// 验证 gpt-4 统计
	gpt4Stats := tracker.GetModelStats("gpt-4")
	if gpt4Stats.TotalCalls != 2 {
		t.Errorf("expected 2 gpt-4 calls, got %d", gpt4Stats.TotalCalls)
	}
	if gpt4Stats.TotalPrompt != 300 {
		t.Errorf("expected 300 gpt-4 prompt tokens, got %d", gpt4Stats.TotalPrompt)
	}

	// 验证 gpt-3.5 统计
	gpt35Stats := tracker.GetModelStats("gpt-3.5-turbo")
	if gpt35Stats.TotalCalls != 1 {
		t.Errorf("expected 1 gpt-3.5 call, got %d", gpt35Stats.TotalCalls)
	}
	if gpt35Stats.TotalPrompt != 50 {
		t.Errorf("expected 50 gpt-3.5 prompt tokens, got %d", gpt35Stats.TotalPrompt)
	}

	// 验证不存在的模型
	unknownStats := tracker.GetModelStats("unknown")
	if unknownStats.TotalCalls != 0 {
		t.Errorf("expected 0 unknown calls, got %d", unknownStats.TotalCalls)
	}
}

func TestUsageTrackerBudget(t *testing.T) {
	tracker := NewUsageTracker()

	// 设置预算
	tracker.SetBudget(1.0) // 1 美元

	// 记录调用（gpt-4: 1000 prompt + 500 CompletionTokens = 0.03 + 0.03 = 0.06）
	tracker.Record(schema.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, "gpt-4", 100*time.Millisecond)

	// 检查预算
	if tracker.IsOverBudget() {
		t.Error("should not be over budget yet")
	}

	remaining := tracker.GetRemainingBudget()
	expectedRemaining := 1.0 - 0.06
	if remaining != expectedRemaining {
		t.Errorf("expected remaining budget %.2f, got %.2f", expectedRemaining, remaining)
	}

	// 大量调用超出预算
	for i := 0; i < 20; i++ {
		tracker.Record(schema.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}, "gpt-4", 100*time.Millisecond)
	}

	if !tracker.IsOverBudget() {
		t.Error("should be over budget now")
	}
}

func TestUsageTrackerReset(t *testing.T) {
	tracker := NewUsageTracker()

	tracker.Record(schema.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, "gpt-4", 100*time.Millisecond)

	stats := tracker.GetStats()
	if stats.TotalCalls != 1 {
		t.Errorf("expected 1 call, got %d", stats.TotalCalls)
	}

	tracker.Reset()

	stats = tracker.GetStats()
	if stats.TotalCalls != 0 {
		t.Errorf("expected 0 calls after reset, got %d", stats.TotalCalls)
	}
}

func TestUsageTrackerExport(t *testing.T) {
	tracker := NewUsageTracker()

	tracker.Record(schema.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, "gpt-4", 100*time.Millisecond)
	tracker.Record(schema.Usage{PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300}, "gpt-4", 200*time.Millisecond)

	// 测试导出统计
	jsonData, err := tracker.ExportJSON()
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	var stats UsageStats
	if err := json.Unmarshal(jsonData, &stats); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if stats.TotalCalls != 2 {
		t.Errorf("expected 2 calls in export, got %d", stats.TotalCalls)
	}

	// 测试导出记录
	recordsJSON, err := tracker.ExportRecordsJSON()
	if err != nil {
		t.Fatalf("export records failed: %v", err)
	}

	var records []UsageRecord
	if err := json.Unmarshal(recordsJSON, &records); err != nil {
		t.Fatalf("unmarshal records failed: %v", err)
	}

	if len(records) != 2 {
		t.Errorf("expected 2 records in export, got %d", len(records))
	}
}

func TestUsageTrackerRecentRecords(t *testing.T) {
	tracker := NewUsageTracker()

	// 记录 5 次调用
	for i := 0; i < 5; i++ {
		tracker.Record(schema.Usage{PromptTokens: uint64(i * 10), CompletionTokens: uint64(i * 5), TotalTokens: uint64(i * 15)}, "gpt-4", time.Duration(i)*time.Millisecond)
	}

	// 获取最近 3 条
	recent := tracker.GetRecentRecords(3)
	if len(recent) != 3 {
		t.Errorf("expected 3 recent records, got %d", len(recent))
	}

	// 验证最近一条
	if recent[2].PromptTokens != 40 {
		t.Errorf("expected last record prompt tokens 40, got %d", recent[2].PromptTokens)
	}

	// 获取所有记录
	all := tracker.GetRecentRecords(0)
	if len(all) != 5 {
		t.Errorf("expected 5 total records, got %d", len(all))
	}
}

func TestUsageTrackerFormatStats(t *testing.T) {
	tracker := NewUsageTracker()

	tracker.Record(schema.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}, "gpt-4", 100*time.Millisecond)

	formatted := tracker.FormatStats()
	if formatted == "" {
		t.Error("FormatStats should not return empty string")
	}

	// 验证包含关键信息
	if !contains(formatted, "Total Calls: 1") {
		t.Error("FormatStats should contain total calls")
	}
	if !contains(formatted, "Total Prompt Tokens: 100") {
		t.Error("FormatStats should contain prompt tokens")
	}
}

func TestUsageTrackerWithAgent(t *testing.T) {
	// 创建 MockModel（支持 Usage 返回）
	mock := mock.NewMockModel()
	mock.SetGenerateFunc(func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.AssistantRole,
			Content: "Test response",
			Usage: &schema.Usage{
				PromptTokens:     100,
				CompletionTokens: 50,
				TotalTokens:      150,
			},
		}, nil
	})
	mock.SetModelName("gpt-4")

	// 创建 UsageTracker
	tracker := NewUsageTracker()

	// 创建 Agent（不带 executor）
	agent := NewAgent(mock, nil, WithUsageTracker(tracker))

	// 发送消息
	_, err := agent.SendMessage(context.Background(), schema.UserMessage("Hello"))
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}

	// 验证 Usage 被记录
	stats := tracker.GetStats()
	fmt.Printf("%v \n", stats.TotalTokens)
	if stats.TotalCalls != 1 {
		t.Errorf("expected 1 call recorded, got %d", stats.TotalCalls)
	}
	if stats.TotalPrompt != 100 {
		t.Errorf("expected 100 prompt tokens, got %d", stats.TotalPrompt)
	}
	if stats.TotalCompletion != 50 {
		t.Errorf("expected 50 CompletionTokens tokens, got %d", stats.TotalCompletion)
	}
}

func TestUsageTrackerEmpty(t *testing.T) {
	tracker := NewUsageTracker()

	// 空 tracker 的统计
	stats := tracker.GetStats()
	if stats.TotalCalls != 0 {
		t.Errorf("expected 0 calls, got %d", stats.TotalCalls)
	}
	if stats.TotalCost != 0 {
		t.Errorf("expected 0 cost, got %.4f", stats.TotalCost)
	}

	// 空 tracker 不应超预算
	tracker.SetBudget(1.0)
	if tracker.IsOverBudget() {
		t.Error("empty tracker should not be over budget")
	}

	// 空 tracker 的剩余预算
	remaining := tracker.GetRemainingBudget()
	if remaining != 1.0 {
		t.Errorf("expected remaining budget 1.0, got %.2f", remaining)
	}
}
