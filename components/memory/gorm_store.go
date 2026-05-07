package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/coder/hnsw"
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
func (MessageModel) TableName() string {
	return "messages"
}

// ToSchemaMessage 转换为 schema.Message
func (m *MessageModel) ToSchemaMessage() *schema.Message {
	return &schema.Message{
		Role:             schema.RoleType(m.Role),
		Content:          m.Content,
		ReasoningContent: m.ReasoningContent,
	}
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
// GormStore 配置
// ============================================================================

// RecallMode 召回策略模式
type RecallMode int

const (
	RecallModeAuto     RecallMode = iota // 自动：优先向量，失败回退混合（默认行为）
	RecallModeVector                     // 仅向量语义搜索
	RecallModeHybrid                     // 仅关键词 + 时间衰减
	RecallModeCombined                   // 向量 + 关键词 + 时间组合权重
)

// GormStoreConfig GORM 存储配置
type GormStoreConfig struct {
	// DBPath 数据库路径
	DBPath string
	// MaxOpenConns 最大连接数，默认 10
	MaxOpenConns int
	// MaxIdleConns 最大空闲连接数，默认 5
	MaxIdleConns int
	// ConnMaxLifetime 连接最大生命周期，默认 1 小时
	ConnMaxLifetime time.Duration
	// LogLevel GORM 日志级别，默认 Warn
	LogLevel logger.LogLevel
	// DisableVectorSearch 禁用向量搜索（不使用嵌入时设为 true）
	DisableVectorSearch bool
	// EmbeddingDimension 向量维度，默认 384（all-MiniLM-L6-v2）
	EmbeddingDimension int
	// RecallMode 召回策略模式，默认 RecallModeAuto
	RecallMode RecallMode
	// CombinedWeights 组合模式的权重（仅当 RecallModeCombined 时生效）
	CombinedWeights *CombinedWeights
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

// DefaultGormStoreConfig 默认配置
func DefaultGormStoreConfig() *GormStoreConfig {
	return &GormStoreConfig{
		DBPath:              "./chat.db",
		MaxOpenConns:        10,
		MaxIdleConns:        5,
		ConnMaxLifetime:     time.Hour,
		LogLevel:            logger.Warn,
		DisableVectorSearch: false,
		EmbeddingDimension:  384,
		RecallMode:          RecallModeAuto,
		CombinedWeights:     DefaultCombinedWeights(),
	}
}

// ============================================================================
// GormStore 实现
// ============================================================================
// messageNode 包装 MessageModel 以实现 hnsw.Embeddable
type messageNode struct {
	model *MessageModel
	vec   []float32 // 缓存嵌入向量，避免重复反序列化
}

func (n *messageNode) ID() string           { return n.model.ID }
func (n *messageNode) Embedding() []float32 { return n.vec }

// GormStore 基于 GORM 的高级记忆存储
// 完全兼容 Store 接口，可替代 LocalStore/SqliteStore
type GormStore struct {
	db        *gorm.DB
	config    *GormStoreConfig
	embedding EmbeddingFunc // 嵌入函数（可选）

	// HNSW 向量索引（线程安全）
	vecIndex *hnsw.Graph[*messageNode]
	vecMu    sync.RWMutex
}

// EmbeddingFunc 文本嵌入函数签名
// 将文本转换为向量，用于语义相似度搜索
type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)

// NewGormStore 创建 GORM 记忆存储
func NewGormStore(config *GormStoreConfig, embedding EmbeddingFunc) (*GormStore, error) {
	if config == nil {
		config = DefaultGormStoreConfig()
	}

	// 使用纯 Go 的 SQLite 驱动（无需 CGO）
	db, err := gorm.Open(sqlite.Open(config.DBPath), &gorm.Config{
		Logger: logger.Default.LogMode(config.LogLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("gorm: open database failed: %w", err)
	}

	// 配置连接池
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(config.MaxOpenConns)
	sqlDB.SetMaxIdleConns(config.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(config.ConnMaxLifetime)

	// 自动迁移
	if err := db.AutoMigrate(&MessageModel{}); err != nil {
		return nil, fmt.Errorf("gorm: auto migrate failed: %w", err)
	}

	// 创建额外索引（忽略已存在错误）
	_ = db.Exec("CREATE INDEX IF NOT EXISTS idx_content ON messages(content)")
	_ = db.Exec("CREATE INDEX IF NOT EXISTS idx_role_session ON messages(role, session_id)")

	store := &GormStore{
		db:        db,
		config:    config,
		embedding: embedding,
	}

	if !config.DisableVectorSearch && embedding != nil {
		store.vecIndex = hnsw.NewGraph[*messageNode]()
		store.vecIndex.M = 16
		store.vecIndex.Ml = 0.25
		store.vecIndex.EfSearch = 200
		// 距离函数默认就是 CosineDistance，无需显式设置（也可设：store.vecIndex.Distance = hnsw.CosineDistance）

		go store.rebuildIndexFromDB()
	}

	return store, nil
}

func (s *GormStore) rebuildIndexFromDB() {
	var models []MessageModel
	if err := s.db.Find(&models).Error; err != nil {
		return
	}

	nodes := make([]*messageNode, 0, len(models))
	for i := range models {
		vec, err := models[i].GetEmbedding()
		if err != nil || len(vec) == 0 {
			continue
		}
		nodes = append(nodes, &messageNode{model: &models[i], vec: vec})
	}

	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	s.vecIndex.Add(nodes...)
}

// Save 保存消息（支持批量 + 嵌入生成）
// 每条消息使用递增时间戳，确保同一轮对话内的顺序正确
func (s *GormStore) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	baseTime := time.Now().UnixMilli()

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i, msg := range msgs {
			// 递增时间戳：每条消息相差 1 毫秒，确保顺序
			timestamp := baseTime + int64(i)

			model := &MessageModel{
				ID:               fmt.Sprintf("%s_%s", sessionID, uuid.New().String()),
				SessionID:        sessionID,
				Role:             string(msg.Role),
				Content:          msg.Content,
				ReasoningContent: msg.ReasoningContent,
				Timestamp:        timestamp,
				CreatedAt:        time.Now(),
			}

			// 序列化元数据（ToolCalls 等）
			if len(msg.ToolCalls) > 0 || len(msg.ToolResults) > 0 {
				meta, _ := json.Marshal(map[string]any{
					"tool_calls":   msg.ToolCalls,
					"tool_results": msg.ToolResults,
				})
				model.Metadata = string(meta)
			}

			// 生成嵌入向量后
			if !s.config.DisableVectorSearch && s.embedding != nil {
				text := msg.Content
				if msg.ReasoningContent != "" {
					text = msg.ReasoningContent + " " + text
				}
				vec, err := s.embedding(ctx, text)
				if err == nil && len(vec) > 0 {
					model.SetEmbedding(vec)
					s.vecMu.Lock()
					if s.vecIndex != nil {
						s.vecIndex.Add(&messageNode{model: model, vec: vec})
					}
					s.vecMu.Unlock()
				}
			}

			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// Recall 智能召回（多策略混合）
func (s *GormStore) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	if topK <= 0 {
		topK = 3
	}

	switch s.config.RecallMode {
	case RecallModeVector:
		// 仅向量，失败回退到混合（保证有结果）
		results, err := s.vectorRecall(ctx, sessionID, query, topK)
		if err != nil || len(results) == 0 {
			return s.hybridRecall(ctx, sessionID, query, topK)
		}
		return results, nil

	case RecallModeHybrid:
		return s.hybridRecall(ctx, sessionID, query, topK)

	case RecallModeCombined:
		return s.combinedRecall(ctx, sessionID, query, topK)

	default: // RecallModeAuto 或未设置
		if !s.config.DisableVectorSearch && s.embedding != nil {
			results, err := s.vectorRecall(ctx, sessionID, query, topK)
			if err == nil && len(results) > 0 {
				return results, nil
			}
		}
		return s.hybridRecall(ctx, sessionID, query, topK)
	}
}

func (s *GormStore) combinedRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	queryVec, err := s.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	// 粗筛：取 topK*10 候选
	candidates, err := s.vectorSearch(ctx, sessionID, queryVec, topK*10)
	if err != nil || len(candidates) == 0 {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	// 关键词提取
	keywords := extractKeywords(query)

	// 时间衰减参数
	now := float64(time.Now().UnixMilli())
	weights := s.config.CombinedWeights
	if weights == nil {
		weights = DefaultCombinedWeights()
	}

	type scored struct {
		model *MessageModel
		score float64
	}
	scoredList := make([]scored, 0, len(candidates))

	for _, c := range candidates {
		// 向量得分
		vecScore := c.similarity

		// 关键词得分
		keywordScore := 0.0
		content := strings.ToLower(c.model.Content)
		for _, kw := range keywords {
			if strings.Contains(content, strings.ToLower(kw)) {
				keywordScore += 1.0
			}
		}
		if len(keywords) > 0 {
			keywordScore /= float64(len(keywords))
		}

		// 时间得分
		age := now - float64(c.model.Timestamp)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / (7 * 24 * 3600 * 1000))

		total := weights.VectorWeight*vecScore +
			weights.KeywordWeight*keywordScore +
			weights.TimeWeight*timeScore

		scoredList = append(scoredList, scored{model: c.model, score: total})
	}

	// 按总分降序
	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].score > scoredList[j].score
	})

	// 取 topK
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}

	results := make([]*schema.Message, len(scoredList))
	for i, s := range scoredList {
		results[i] = s.model.ToSchemaMessage()
	}
	return results, nil
}

type vectorCandidate struct {
	model      *MessageModel
	similarity float64
}

// vectorSearch 执行 HNSW 搜索并返回带相似度的候选，限定 sessionID
func (s *GormStore) vectorSearch(ctx context.Context, sessionID string, queryVec []float32, topK int) ([]vectorCandidate, error) {
	s.vecMu.RLock()
	defer s.vecMu.RUnlock()

	if s.vecIndex == nil {
		return nil, fmt.Errorf("index not initialized")
	}

	// 多取一些候选，用于过滤 session
	fetchN := topK * 3
	if fetchN < 10 {
		fetchN = 10
	}
	results := s.vecIndex.Search(queryVec, fetchN)
	if len(results) == 0 {
		return nil, nil
	}

	candidates := make([]vectorCandidate, 0, topK)
	for _, node := range results {
		if node.model.SessionID == sessionID {
			sim := cosineSimilarity(queryVec, node.Embedding())
			candidates = append(candidates, vectorCandidate{
				model:      node.model,
				similarity: sim,
			})
			if len(candidates) >= topK {
				break
			}
		}
	}
	return candidates, nil
}

// vectorRecall 向量语义召回
func (s *GormStore) vectorRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	queryVec, err := s.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	candidates, err := s.vectorSearch(ctx, sessionID, queryVec, topK)
	if err != nil {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	results := make([]*schema.Message, len(candidates))
	for i, c := range candidates {
		results[i] = c.model.ToSchemaMessage()
	}
	return results, nil
}

// hybridRecall 混合召回（关键词 + 时间衰减）
func (s *GormStore) hybridRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	// 分词提取关键词
	keywords := extractKeywords(query)

	// 构建查询条件
	var models []MessageModel
	db := s.db.WithContext(ctx).Where("session_id = ?", sessionID)

	// 关键词匹配
	if len(keywords) > 0 {
		var conditions []string
		var args []any
		for _, kw := range keywords {
			conditions = append(conditions, "content LIKE ?")
			args = append(args, "%"+kw+"%")
		}
		db = db.Where(strings.Join(conditions, " OR "), args...)
	}

	// 限制范围并排序（时间倒序）
	if err := db.Order("timestamp DESC").Limit(topK * 3).Find(&models).Error; err != nil {
		return nil, err
	}

	// 时间衰减重排序
	now := float64(time.Now().UnixMilli())

	type scoredMsg struct {
		msg   *MessageModel
		score float64
	}
	scored := make([]scoredMsg, 0, len(models))

	for i := range models {
		// 时间衰减：越新的消息权重越高
		age := now - float64(models[i].Timestamp)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / (7 * 24 * 3600 * 1000)) // 7天半衰期（时间戳是毫秒级）

		// 关键词匹配度
		keywordScore := 0.0
		content := strings.ToLower(models[i].Content)
		for _, kw := range keywords {
			if strings.Contains(content, strings.ToLower(kw)) {
				keywordScore += 1.0
			}
		}

		// 综合得分
		score := timeScore*0.3 + keywordScore*0.7
		scored = append(scored, scoredMsg{msg: &models[i], score: score})
	}

	// 按综合得分排序
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// 取 topK
	if len(scored) > topK {
		scored = scored[:topK]
	}

	// 转换为 schema.Message
	results := make([]*schema.Message, len(scored))
	for i, s := range scored {
		results[i] = s.msg.ToSchemaMessage()
	}

	return results, nil
}

// GetSession 获取完整会话历史
func (s *GormStore) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	var models []MessageModel
	if err := s.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("timestamp ASC").
		Find(&models).Error; err != nil {
		return nil, err
	}

	results := make([]*schema.Message, len(models))
	for i, m := range models {
		results[i] = m.ToSchemaMessage()
	}
	return results, nil
}

// GetSessionWithReasoning 获取完整会话历史（含推理内容）
// 当前实现与 GetSession 相同，因为 MessageModel.ToSchemaMessage 已包含 ReasoningContent
// 如需扩展（如返回 ToolCalls），可在此添加额外处理
func (s *GormStore) GetSessionWithReasoning(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	return s.GetSession(ctx, sessionID)
}

// ClearSession 清空会话（硬删除 + 同步清理向量索引）
func (s *GormStore) ClearSession(ctx context.Context, sessionID string) error {
	// 1. 查询该会话所有消息 ID（用于同步删除向量索引）
	var ids []string
	if err := s.db.WithContext(ctx).
		Model(&MessageModel{}).
		Where("session_id = ?", sessionID).
		Pluck("id", &ids).Error; err != nil {
		return err
	}

	// 2. 删除向量索引中的节点
	if s.vecIndex != nil && len(ids) > 0 {
		s.vecMu.Lock()
		for _, id := range ids {
			s.vecIndex.Delete(id)
		}
		s.vecMu.Unlock()
	}

	// 3. 硬删除数据库中的消息记录
	return s.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Delete(&MessageModel{}).Error
}

// Close 关闭存储
func (s *GormStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ============================================================================
// 高级查询方法（GormStore 扩展功能）
// ============================================================================

// SearchByRole 按角色搜索
func (s *GormStore) SearchByRole(ctx context.Context, sessionID string, role schema.RoleType, limit int) ([]*schema.Message, error) {
	var models []MessageModel
	if err := s.db.WithContext(ctx).
		Where("session_id = ? AND role = ?", sessionID, string(role)).
		Order("timestamp DESC").
		Limit(limit).
		Find(&models).Error; err != nil {
		return nil, err
	}

	results := make([]*schema.Message, len(models))
	for i := range models {
		results[i] = models[i].ToSchemaMessage()
	}
	return results, nil
}

// SearchByTimeRange 按时间范围搜索
func (s *GormStore) SearchByTimeRange(ctx context.Context, sessionID string, start, end time.Time) ([]*schema.Message, error) {
	var models []MessageModel
	if err := s.db.WithContext(ctx).
		Where("session_id = ? AND timestamp >= ? AND timestamp <= ?",
			sessionID, start.UnixMilli(), end.UnixMilli()).
		Order("timestamp ASC").
		Find(&models).Error; err != nil {
		return nil, err
	}

	results := make([]*schema.Message, len(models))
	for i := range models {
		results[i] = models[i].ToSchemaMessage()
	}
	return results, nil
}

// GetSessionStats 获取会话统计
func (s *GormStore) GetSessionStats(ctx context.Context, sessionID string) (map[string]any, error) {
	var count int64
	if err := s.db.WithContext(ctx).
		Model(&MessageModel{}).
		Where("session_id = ?", sessionID).
		Count(&count).Error; err != nil {
		return nil, err
	}

	var firstMsg, lastMsg MessageModel
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("timestamp ASC").First(&firstMsg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return map[string]any{
				"message_count": 0,
				"first_message": nil,
				"last_message":  nil,
			}, nil
		}
		return nil, err
	}
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).Order("timestamp DESC").First(&lastMsg).Error; err != nil {
		return nil, err
	}

	return map[string]any{
		"message_count": count,
		"first_message": time.UnixMilli(firstMsg.Timestamp),
		"last_message":  time.UnixMilli(lastMsg.Timestamp),
	}, nil
}

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
