package gorm

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"gorm.io/gorm/logger"
)

// ============================================================================
// GORM 模型定义
// ============================================================================

// MessageModel GORM 消息模型
type MessageModel struct {
	ID               string `gorm:"primaryKey;size:64"`
	SessionID        string `gorm:"index:idx_session_time,priority:1;size:128"`
	Role             string `gorm:"size:32;not null"`
	Content          string `gorm:"type:text;not null"`
	ReasoningContent string `gorm:"type:text"`
	Embedding        string `gorm:"type:text"` // JSON 序列化的向量（空字符串表示无嵌入）
	Timestamp        int64  `gorm:"index:idx_session_time,priority:2;not null"`
	Metadata         string `gorm:"type:text"` // JSON 序列化的元数据
	CreatedAt        time.Time
}

// TableName 指定表名
func (m *MessageModel) TableName() string {
	return "messages"
}

// ToSchemaMessage 转换为 schema.Message
func (m *MessageModel) ToSchemaMessage() *schema.Message {
	msg := &schema.Message{
		Role:             schema.RoleType(m.Role),
		Content:          m.Content,
		ReasoningContent: m.ReasoningContent,
	}

	// 从 Metadata 还原工具调用和多模态信息
	if m.Metadata != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(m.Metadata), &meta); err == nil {
			// 还原 tool_calls
			if tcRaw, ok := meta["tool_calls"]; ok {
				if tcBytes, err := json.Marshal(tcRaw); err == nil {
					var toolCalls []schema.ToolCall
					if err := json.Unmarshal(tcBytes, &toolCalls); err == nil {
						msg.ToolCalls = toolCalls
					}
				}
			}
			// 还原 tool_call_id
			if tcid, ok := meta["tool_call_id"].(string); ok {
				msg.ToolCallID = tcid
			}
			// 标记曾为多模态消息（元数据已在 metaData 中记录，ContentParts 不恢复）
			// 注意：从 DB 恢复的消息只有纯文本 Content，原始多模态数据（图片等）不持久化
		}
	}

	return msg
}

// GetEmbedding 获取嵌入向量
func (m *MessageModel) GetEmbedding() ([]float32, error) {
	if m.Embedding == "" {
		return nil, nil
	}
	var vec []float32
	if err := json.Unmarshal([]byte(m.Embedding), &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

// SetEmbedding 设置嵌入向量
func (m *MessageModel) SetEmbedding(vec []float32) error {
	if len(vec) == 0 {
		m.Embedding = ""
		return nil
	}
	data, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	m.Embedding = string(data)
	return nil
}

// ============================================================================
// RecallMode 和配置
// ============================================================================

// RecallMode 召回策略模式
type RecallMode int

const (
	RecallModeAuto     RecallMode = iota // 自动：优先向量，失败回退混合（默认行为）
	RecallModeVector                     // 仅向量语义搜索
	RecallModeHybrid                     // 仅关键词 + 时间衰减
	RecallModeCombined                   // 向量 + 关键词 + 时间组合权重
)

// Config GORM 存储配置
type Config struct {
	DBPath              string
	MaxOpenConns        int
	MaxIdleConns        int
	ConnMaxLifetime     time.Duration
	LogLevel            logger.LogLevel
	DisableVectorSearch bool
	EmbeddingDimension  int
	RecallMode          RecallMode
	CombinedWeights     *CombinedWeights
	ChunkSize           int
	ChunkOverlap        int
	IndexCachePath      string

	// HNSW 参数
	HNSW_M        int     // 每节点最大双向链接数，默认 16
	HNSW_Ml       float64 // 层级生成因子，默认 0.25
	HNSW_EfSearch int     // 搜索宽度（越大越精确越慢），默认 200

	// 召回参数
	DefaultTopK           int     // 默认召回数量，默认 3
	CombinedCandidateMult int     // 组合召回候选倍数，默认 10
	TimeDecayHalfLifeMs   float64 // 时间衰减半衰期（毫秒），默认 7 天
	IndexRebuildBatchSize int     // 索引重建批次大小，默认 500
	HybridTimeWeight      float64 // 混合召回时间权重，默认 0.3
	HybridKeywordWeight   float64 // 混合召回关键词权重，默认 0.7
}

// CombinedWeights 组合召回的各因子权重，总和应为 1.0
type CombinedWeights struct {
	VectorWeight  float64 // 向量相似度权重
	KeywordWeight float64 // 关键词匹配权重
	TimeWeight    float64 // 时间衰减权重
}

// DefaultCombinedWeights 默认组合权重
func DefaultCombinedWeights() *CombinedWeights {
	return &CombinedWeights{
		VectorWeight:  0.5,
		KeywordWeight: 0.3,
		TimeWeight:    0.2,
	}
}

// DefaultConfig 默认配置
func DefaultConfig() *Config {
	return &Config{
		DBPath:              "./chat.db",
		MaxOpenConns:        10,
		MaxIdleConns:        5,
		ConnMaxLifetime:     time.Hour,
		LogLevel:            logger.Warn,
		DisableVectorSearch: false,
		EmbeddingDimension:  768,
		RecallMode:          RecallModeCombined,
		CombinedWeights:     DefaultCombinedWeights(),
		ChunkSize:           512,
		ChunkOverlap:        64,

		HNSW_M:        16,
		HNSW_Ml:       0.25,
		HNSW_EfSearch: 200,

		DefaultTopK:           3,
		CombinedCandidateMult: 10,
		TimeDecayHalfLifeMs:   7 * 24 * 3600 * 1000,
		IndexRebuildBatchSize: 500,
		HybridTimeWeight:      0.3,
		HybridKeywordWeight:   0.7,
	}
}

// ============================================================================
// 分块嵌入
// ============================================================================

// EmbeddingChunk 分块嵌入记录，用于长文本分段存储
type EmbeddingChunk struct {
	ID         string `gorm:"primaryKey;size:64"`
	MessageID  string `gorm:"index;size:64;not null"`  // 关联 MessageModel.ID
	SessionID  string `gorm:"index;size:128;not null"` // 冗余字段，加速查询
	ChunkIndex int    `gorm:"not null"`                // 块序号
	Content    string `gorm:"type:text;not null"`      // 块文本
	Embedding  string `gorm:"type:text"`               // 嵌入向量 JSON
}

func (c *EmbeddingChunk) TableName() string {
	return "embedding_chunks"
}

func (c *EmbeddingChunk) GetEmbedding() ([]float32, error) {
	if c.Embedding == "" {
		return nil, nil
	}
	var vec []float32
	if err := json.Unmarshal([]byte(c.Embedding), &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

func (c *EmbeddingChunk) SetEmbedding(vec []float32) error {
	if len(vec) == 0 {
		c.Embedding = ""
		return nil
	}
	data, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	c.Embedding = string(data)
	return nil
}

// ============================================================================
// EmbeddingFunc
// ============================================================================

// EmbeddingFunc 文本嵌入函数签名
// 将文本转换为向量，用于语义相似度搜索
type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)

// ============================================================================
// 辅助函数
// ============================================================================

// cosineSimilarity 计算余弦相似度
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := 0; i < len(a); i++ {
		dotProduct += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// extractKeywords 简单关键词提取
func extractKeywords(text string) []string {
	// 移除标点，分词
	text = strings.ToLower(text)
	for _, r := range []string{
		"，", "。", "！", "？", "；", "：", "\"", "'", "（", "）", "【", "】",
		",", ".", "!", "?", ";", ":", "(", ")", "[", "]",
	} {
		text = strings.ReplaceAll(text, r, " ")
	}

	words := strings.Fields(text)
	// 过滤停用词（简单实现）
	stopWords := map[string]bool{
		"的": true, "了": true, "是": true, "我": true, "你": true,
		"在": true, "有": true, "和": true, "就": true, "不": true,
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"this": true, "that": true, "it": true, "he": true, "she": true,
		"we": true, "they": true, "me": true, "him": true, "her": true,
		"us": true, "them": true, "my": true, "his": true, "its": true,
		"our": true, "your": true, "their": true, "i": true, "you": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "can": true, "shall": true,
		"to": true, "of": true, "in": true, "for": true, "on": true,
		"with": true, "at": true, "by": true, "from": true, "as": true,
		"into": true, "through": true, "during": true, "before": true,
		"after": true, "above": true, "below": true, "between": true,
		"and": true, "but": true, "or": true, "nor": true, "not": true,
		"so": true, "yet": true, "both": true, "either": true, "neither": true,
		"each": true, "every": true, "all": true, "any": true, "few": true,
		"more": true, "most": true, "other": true, "some": true, "such": true,
		"no": true, "only": true, "own": true, "same": true, "than": true,
		"too": true, "very": true, "just": true, "about": true,
	}

	var keywords []string
	seen := make(map[string]bool)
	for _, w := range words {
		if len(w) > 1 && !stopWords[w] && !seen[w] {
			keywords = append(keywords, w)
			seen[w] = true
		}
	}

	return keywords
}

// SplitText 将文本按字符数分块，优先在自然边界（句号、换行等）处断开
func SplitText(text string, chunkSize, chunkOverlap int) []string {
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if chunkOverlap >= chunkSize {
		chunkOverlap = chunkSize / 5
	}

	var chunks []string
	runes := []rune(text)
	if len(runes) <= chunkSize {
		return []string{text}
	}

	// 定义自然边界字符
	isBoundary := func(r rune) bool {
		switch r {
		case '。', '！', '？', '\n', '.', '!', '?', '；', ';':
			return true
		}
		return false
	}

	i := 0
	for i < len(runes) {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}

		// 尝试在自然边界处断开
		if end < len(runes) {
			// 从 end 往前找第一个边界，但不能低于 chunkSize 的一半
			for j := end; j > i+chunkSize/2; j-- {
				if isBoundary(runes[j]) {
					end = j + 1 // 包含边界字符
					break
				}
			}
		}

		chunkText := string(runes[i:end])
		chunks = append(chunks, strings.TrimSpace(chunkText))

		if end >= len(runes) {
			break
		}
		i = end - chunkOverlap
	}
	return chunks
}

// isValidVector 检查向量是否可用于 HNSW 索引
// 排除：nil、空、全零、含 NaN、含 Inf
func isValidVector(vec []float32) bool {
	if len(vec) == 0 {
		return false
	}
	var normSq float64
	for _, v := range vec {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
		normSq += f * f
	}
	return normSq > 0
}
