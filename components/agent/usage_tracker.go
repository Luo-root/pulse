package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// UsageRecord 单次模型调用的使用记录
type UsageRecord struct {
	PromptTokens     uint64        `json:"prompt_tokens"`
	CompletionTokens uint64        `json:"completion_tokens"`
	TotalTokens      uint64        `json:"total_tokens"`
	CachedTokens     uint64        `json:"cached_tokens,omitempty"`
	Model            string        `json:"model"`
	Timestamp        time.Time     `json:"timestamp"`
	Duration         time.Duration `json:"duration_ms"`
}

// UsageStats 累计统计信息
type UsageStats struct {
	TotalCalls      int           `json:"total_calls"`
	TotalPrompt     uint64        `json:"total_prompt_tokens"`
	TotalCompletion uint64        `json:"total_completion_tokens"`
	TotalTokens     uint64        `json:"total_tokens"`
	TotalCached     uint64        `json:"total_cached_tokens"`
	TotalCost       float64       `json:"total_cost_usd"`
	AverageLatency  time.Duration `json:"average_latency_ms"`
	Records         []UsageRecord `json:"records,omitempty"`
}

// ModelPricing 模型定价（每 1K Token 的美元价格）
type ModelPricing struct {
	InputPrice  float64 // 输入价格
	OutputPrice float64 // 输出价格
	CachedPrice float64 // 缓存价格（可选）
}

// DefaultPricingTable 内置价格表
// 价格单位：元 / 1k tokens
var DefaultPricingTable = map[string]ModelPricing{
	"deepseek-v4-flash": {InputPrice: 0.001, OutputPrice: 0.002},
	"deepseek-v4-pro":   {InputPrice: 0.012, OutputPrice: 0.024},
	"kimi-k2.6":         {InputPrice: 0.0065, OutputPrice: 0.027},
}

// UsageTracker Token 使用追踪器
type UsageTracker struct {
	records []UsageRecord
	pricing map[string]ModelPricing
	budget  float64 // 美元预算，0 表示无限制
	mu      sync.RWMutex
}

// NewUsageTracker 创建 UsageTracker
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{
		records: make([]UsageRecord, 0),
		pricing: DefaultPricingTable,
		budget:  0,
	}
}

// NewUsageTrackerWithPricing 创建带自定义定价的 UsageTracker
func NewUsageTrackerWithPricing(pricing map[string]ModelPricing) *UsageTracker {
	return &UsageTracker{
		records: make([]UsageRecord, 0),
		pricing: pricing,
		budget:  0,
	}
}

// Record 记录一次模型调用
func (ut *UsageTracker) Record(usage schema.Usage, model string, duration time.Duration) {
	ut.mu.Lock()
	defer ut.mu.Unlock()

	record := UsageRecord{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.Completion,
		TotalTokens:      usage.TotalTokens,
		Model:            model,
		Timestamp:        time.Now(),
		Duration:         duration,
	}

	ut.records = append(ut.records, record)
}

// RecordWithCached 记录一次模型调用（含缓存信息）
func (ut *UsageTracker) RecordWithCached(usage schema.Usage, cachedTokens uint64, model string, duration time.Duration) {
	ut.mu.Lock()
	defer ut.mu.Unlock()

	record := UsageRecord{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.Completion,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     cachedTokens,
		Model:            model,
		Timestamp:        time.Now(),
		Duration:         duration,
	}

	ut.records = append(ut.records, record)
}

// GetStats 获取累计统计
func (ut *UsageTracker) GetStats() UsageStats {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	return ut.calculateStats(ut.records)
}

// GetModelStats 获取指定模型的统计
func (ut *UsageTracker) GetModelStats(model string) UsageStats {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	var modelRecords []UsageRecord
	for _, r := range ut.records {
		if r.Model == model {
			modelRecords = append(modelRecords, r)
		}
	}

	return ut.calculateStats(modelRecords)
}

// GetRecentRecords 获取最近 N 条记录
func (ut *UsageTracker) GetRecentRecords(n int) []UsageRecord {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	if n <= 0 || n >= len(ut.records) {
		// 返回副本
		result := make([]UsageRecord, len(ut.records))
		copy(result, ut.records)
		return result
	}

	start := len(ut.records) - n
	result := make([]UsageRecord, n)
	copy(result, ut.records[start:])
	return result
}

// GetAllRecords 获取所有记录副本
func (ut *UsageTracker) GetAllRecords() []UsageRecord {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	result := make([]UsageRecord, len(ut.records))
	copy(result, ut.records)
	return result
}

// SetBudget 设置预算限制（美元）
func (ut *UsageTracker) SetBudget(budget float64) {
	ut.mu.Lock()
	defer ut.mu.Unlock()
	ut.budget = budget
}

// IsOverBudget 检查是否超出预算
func (ut *UsageTracker) IsOverBudget() bool {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	if ut.budget <= 0 {
		return false
	}

	totalCost := ut.calculateTotalCost(ut.records)
	return totalCost > ut.budget
}

// GetRemainingBudget 获取剩余预算
func (ut *UsageTracker) GetRemainingBudget() float64 {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	if ut.budget <= 0 {
		return -1 // 无预算限制
	}

	totalCost := ut.calculateTotalCost(ut.records)
	return ut.budget - totalCost
}

// Reset 重置所有记录
func (ut *UsageTracker) Reset() {
	ut.mu.Lock()
	defer ut.mu.Unlock()
	ut.records = make([]UsageRecord, 0)
}

// ExportJSON 导出为 JSON
func (ut *UsageTracker) ExportJSON() ([]byte, error) {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	stats := ut.calculateStats(ut.records)
	return json.MarshalIndent(stats, "", "  ")
}

// ExportRecordsJSON 导出所有记录为 JSON
func (ut *UsageTracker) ExportRecordsJSON() ([]byte, error) {
	ut.mu.RLock()
	defer ut.mu.RUnlock()

	return json.MarshalIndent(ut.records, "", "  ")
}

// calculateStats 计算统计信息（内部方法，调用方需持有锁）
func (ut *UsageTracker) calculateStats(records []UsageRecord) UsageStats {
	if len(records) == 0 {
		return UsageStats{Records: []UsageRecord{}}
	}

	var totalPrompt, totalCompletion, totalTokens, totalCached uint64
	var totalLatency time.Duration

	for _, r := range records {
		totalPrompt += r.PromptTokens
		totalCompletion += r.CompletionTokens
		totalTokens += r.TotalTokens
		totalCached += r.CachedTokens
		totalLatency += r.Duration
	}

	avgLatency := time.Duration(0)
	if len(records) > 0 {
		avgLatency = totalLatency / time.Duration(len(records))
	}

	// 计算成本
	cost := ut.calculateCost(records)

	// 复制记录
	recordsCopy := make([]UsageRecord, len(records))
	copy(recordsCopy, records)

	return UsageStats{
		TotalCalls:      len(records),
		TotalPrompt:     totalPrompt,
		TotalCompletion: totalCompletion,
		TotalTokens:     totalTokens,
		TotalCached:     totalCached,
		TotalCost:       cost,
		AverageLatency:  avgLatency,
		Records:         recordsCopy,
	}
}

// calculateCost 计算成本（内部方法）
func (ut *UsageTracker) calculateCost(records []UsageRecord) float64 {
	var totalCost float64

	for _, r := range records {
		pricing, ok := ut.pricing[r.Model]
		if !ok {
			// 使用默认定价或跳过
			continue
		}

		// 输入成本
		inputCost := float64(r.PromptTokens) / 1000.0 * pricing.InputPrice

		// 输出成本
		outputCost := float64(r.CompletionTokens) / 1000.0 * pricing.OutputPrice

		// 缓存成本（如果有）
		cachedCost := float64(r.CachedTokens) / 1000.0 * pricing.CachedPrice

		totalCost += inputCost + outputCost + cachedCost
	}

	return totalCost
}

// calculateTotalCost 计算总成本（内部方法，调用方需持有读锁）
func (ut *UsageTracker) calculateTotalCost(records []UsageRecord) float64 {
	return ut.calculateCost(records)
}

// FormatStats 格式化统计信息为可读字符串
func (ut *UsageTracker) FormatStats() string {
	stats := ut.GetStats()

	return fmt.Sprintf(
		"Usage Statistics:\n"+
			"  Total Calls: %d\n"+
			"  Total Prompt Tokens: %d\n"+
			"  Total Completion Tokens: %d\n"+
			"  Total Tokens: %d\n"+
			"  Total Cost: $%.4f\n"+
			"  Average Latency: %v",
		stats.TotalCalls,
		stats.TotalPrompt,
		stats.TotalCompletion,
		stats.TotalTokens,
		stats.TotalCost,
		stats.AverageLatency,
	)
}
