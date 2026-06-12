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
	"sync/atomic"
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
	// EmbeddingDimension 向量维度，默认 768
	EmbeddingDimension int
	// RecallMode 召回策略模式，默认 RecallModeAuto
	RecallMode RecallMode
	// CombinedWeights 组合模式的权重（仅当 RecallModeCombined 时生效）
	CombinedWeights *CombinedWeights
	// ChunkSize 分块大小（中文字符数），0 或负数表示不分块
	ChunkSize int
	// ChunkOverlap 分块重叠大小（中文字符数）
	ChunkOverlap int
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
		EmbeddingDimension:  768,
		RecallMode:          RecallModeCombined,
		CombinedWeights:     DefaultCombinedWeights(),
		ChunkSize:           512, // 大约对应 256 token
		ChunkOverlap:        64,
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

func (EmbeddingChunk) TableName() string {
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
// GormStore
// ============================================================================

// EmbeddingFunc 文本嵌入函数签名
// 将文本转换为向量，用于语义相似度搜索
type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)

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
	// 异步索引重建的 readiness 信号
	indexReady atomic.Bool
}

// NewGormStore 创建 GORM 记忆存储
func NewGormStore(config *GormStoreConfig, embedding EmbeddingFunc) (*GormStore, error) {
	if config == nil {
		config = DefaultGormStoreConfig()
	}

	db, err := gorm.Open(sqlite.Open(config.DBPath), &gorm.Config{
		Logger: logger.Default.LogMode(config.LogLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("gorm: open database failed: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(config.MaxOpenConns)
	sqlDB.SetMaxIdleConns(config.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(config.ConnMaxLifetime)

	if err := db.AutoMigrate(&MessageModel{}, &EmbeddingChunk{}); err != nil {
		return nil, fmt.Errorf("gorm: auto migrate failed: %w", err)
	}

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

		go func() {
			store.rebuildIndexFromDB()
			store.indexReady.Store(true)
		}()
	} else {
		store.indexReady.Store(true)
	}

	return store, nil
}

// IndexReady 返回向量索引是否就绪
func (s *GormStore) IndexReady() bool {
	return s.indexReady.Load()
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

// addToIndex 安全地向 HNSW 索引添加节点，捕获可能的 panic
func (s *GormStore) addToIndex(node *messageNode) {
	if s.vecIndex == nil || !isValidVector(node.Embedding()) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// HNSW Add panic，跳过此节点
		}
	}()
	s.vecIndex.Add(node)
}

func (s *GormStore) rebuildIndexFromDB() {
	defer s.indexReady.Store(true) // 确保无论成功/失败/panic 都标记就绪

	var models []MessageModel
	if err := s.db.Find(&models).Error; err != nil {
		return
	}
	var chunks []EmbeddingChunk
	if err := s.db.Find(&chunks).Error; err != nil {
		// 表可能不存在，忽略
	}

	s.vecMu.Lock()
	defer s.vecMu.Unlock()

	for i := range models {
		vec, err := models[i].GetEmbedding()
		if err != nil || !isValidVector(vec) {
			continue
		}
		s.addToIndex(&messageNode{model: &models[i], vec: vec})
	}

	for i := range chunks {
		vec, err := chunks[i].GetEmbedding()
		if err != nil || !isValidVector(vec) {
			continue
		}
		s.addToIndex(&messageNode{model: &MessageModel{ID: chunks[i].ID}, vec: vec})
	}
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
			timestamp := baseTime + int64(i)

			// 提取文本内容（多模态消息也要保存文本部分）
			content := msg.TextContent()

			model := &MessageModel{
				ID:               fmt.Sprintf("%s_%s", sessionID, uuid.New().String()),
				SessionID:        sessionID,
				Role:             string(msg.Role),
				Content:          content,
				ReasoningContent: msg.ReasoningContent,
				Timestamp:        timestamp,
				CreatedAt:        time.Now(),
			}

			// 序列化元数据（包含工具调用和多模态标记）
			metaData := make(map[string]any)
			if len(msg.ToolCalls) > 0 {
				metaData["tool_calls"] = msg.ToolCalls
			}
			if msg.ToolCallID != "" {
				metaData["tool_call_id"] = msg.ToolCallID
			}
			// 记录多模态信息（不存储图片数据，只存元数据）
			if msg.IsMultimodal() {
				metaData["multimodal"] = true
				metaData["image_count"] = msg.ImageCount()
				// 记录内容类型分布
				typeCounts := make(map[string]int)
				for _, p := range msg.ContentParts {
					typeCounts[p.Type]++
				}
				metaData["content_types"] = typeCounts
			}
			if len(msg.OutputImages) > 0 {
				metaData["output_images"] = len(msg.OutputImages)
			}
			if msg.OutputAudio != nil {
				metaData["output_audio"] = msg.OutputAudio.Format
			}
			if len(metaData) > 0 {
				meta, _ := json.Marshal(metaData)
				model.Metadata = string(meta)
			}

			// 嵌入生成（只对文本部分做嵌入，不嵌入图片）
			if !s.config.DisableVectorSearch && s.embedding != nil {
				embedText := content
				if msg.ReasoningContent != "" {
					embedText = msg.ReasoningContent + " " + embedText
				}

				estTokens := len([]rune(embedText)) / 2
				needChunk := s.config.ChunkSize > 0 && estTokens > s.config.ChunkSize

				if needChunk {
					chunks := SplitText(embedText, s.config.ChunkSize, s.config.ChunkOverlap)
					for idx, chunkContent := range chunks {
						vec, err := s.embedding(ctx, chunkContent)
						if err != nil || len(vec) == 0 {
							continue
						}
						chunkModel := &EmbeddingChunk{
							ID:         fmt.Sprintf("%s_chunk_%d", model.ID, idx),
							MessageID:  model.ID,
							SessionID:  sessionID,
							ChunkIndex: idx,
							Content:    chunkContent,
						}
						chunkModel.SetEmbedding(vec)

						if err := tx.Create(chunkModel).Error; err != nil {
							return err
						}
						s.vecMu.Lock()
						if s.vecIndex != nil {
							s.addToIndex(&messageNode{
								model: &MessageModel{ID: chunkModel.ID},
								vec:   vec,
							})
						}
						s.vecMu.Unlock()
					}
				} else {
					vec, err := s.embedding(ctx, embedText)
					if err == nil && len(vec) > 0 {
						model.SetEmbedding(vec)
						s.vecMu.Lock()
						if s.vecIndex != nil {
							s.addToIndex(&messageNode{
								model: model,
								vec:   vec,
							})
						}
						s.vecMu.Unlock()
					}
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
		results, err := s.vectorRecall(ctx, sessionID, query, topK)
		if err != nil || len(results) == 0 {
			return s.hybridRecall(ctx, sessionID, query, topK)
		}
		return results, nil

	case RecallModeHybrid:
		return s.hybridRecall(ctx, sessionID, query, topK)

	case RecallModeCombined:
		return s.combinedRecall(ctx, sessionID, query, topK)

	default: // RecallModeAuto
		if !s.config.DisableVectorSearch && s.embedding != nil && s.indexReady.Load() {
			results, err := s.vectorRecall(ctx, sessionID, query, topK)
			if err == nil && len(results) > 0 {
				return results, nil
			}
		}
		return s.hybridRecall(ctx, sessionID, query, topK)
	}
}

// combinedRecall 组合召回（向量 + 关键词 + 时间衰减）
func (s *GormStore) combinedRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	if s.embedding == nil {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	queryVec, err := s.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	if s.vecIndex == nil {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	candidates, err := s.vectorSearch(ctx, sessionID, queryVec, topK*10)
	if err != nil || len(candidates) == 0 {
		return s.hybridRecall(ctx, sessionID, query, topK)
	}

	keywords := extractKeywords(query)
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
		vecScore := c.similarity

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

	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].score > scoredList[j].score
	})

	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}

	results := make([]*schema.Message, len(scoredList))
	for i, sc := range scoredList {
		results[i] = sc.model.ToSchemaMessage()
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
		return nil, fmt.Errorf("vector index not initialized")
	}

	// HNSW Search 在图为空或维度不匹配时会 panic，安全捕获
	fetchN := topK * 3
	if fetchN < 10 {
		fetchN = 10
	}

	var nodes []*messageNode
	func() {
		defer func() {
			if r := recover(); r != nil {
				nodes = nil
			}
		}()
		nodes = s.vecIndex.Search(queryVec, fetchN)
	}()

	if len(nodes) == 0 {
		return nil, nil
	}

	msgMap := make(map[string]*vectorCandidate)
	var orderedIDs []string

	for _, node := range nodes {
		nodeID := node.ID()

		var msgID string
		var sim float64

		if strings.Contains(nodeID, "_chunk_") {
			var chunk EmbeddingChunk
			if err := s.db.WithContext(ctx).Where("id = ? AND session_id = ?", nodeID, sessionID).First(&chunk).Error; err != nil {
				continue
			}
			msgID = chunk.MessageID
			sim = cosineSimilarity(queryVec, node.Embedding())
		} else {
			var msg MessageModel
			if err := s.db.WithContext(ctx).Where("id = ? AND session_id = ?", nodeID, sessionID).First(&msg).Error; err != nil {
				continue
			}
			msgID = nodeID
			sim = cosineSimilarity(queryVec, node.Embedding())
		}

		if existing, exists := msgMap[msgID]; exists {
			if sim > existing.similarity {
				existing.similarity = sim
			}
			continue
		}

		var fullMsg MessageModel
		if err := s.db.WithContext(ctx).Where("id = ?", msgID).First(&fullMsg).Error; err != nil {
			continue
		}
		if fullMsg.SessionID != sessionID {
			continue
		}

		msgMap[msgID] = &vectorCandidate{model: &fullMsg, similarity: sim}
		orderedIDs = append(orderedIDs, msgID)

		if len(msgMap) >= topK {
			break
		}
	}

	var candidates []vectorCandidate
	for _, id := range orderedIDs {
		if cand, ok := msgMap[id]; ok {
			candidates = append(candidates, *cand)
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
	// 1. 清理向量索引中的消息节点
	var ids []string
	if err := s.db.WithContext(ctx).
		Model(&MessageModel{}).
		Where("session_id = ?", sessionID).
		Pluck("id", &ids).Error; err != nil {
		return err
	}

	if s.vecIndex != nil && len(ids) > 0 {
		s.vecMu.Lock()
		for _, id := range ids {
			s.vecIndex.Delete(id)
		}
		s.vecMu.Unlock()
	}

	// 2. 清理分块向量索引
	var chunkIDs []string
	if err := s.db.WithContext(ctx).Model(&EmbeddingChunk{}).
		Where("session_id = ?", sessionID).
		Pluck("id", &chunkIDs).Error; err == nil && len(chunkIDs) > 0 {
		if s.vecIndex != nil {
			s.vecMu.Lock()
			for _, cid := range chunkIDs {
				s.vecIndex.Delete(cid)
			}
			s.vecMu.Unlock()
		}
	}

	// 3. 删除数据
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).Delete(&EmbeddingChunk{}).Error; err != nil {
		return err
	}

	return s.db.WithContext(ctx).Where("session_id = ?", sessionID).Delete(&MessageModel{}).Error
}

// Close 关闭存储
func (s *GormStore) Close() error {
	// 清理向量索引
	s.vecMu.Lock()
	s.vecIndex = nil
	s.vecMu.Unlock()

	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ============================================================================
// 高级查询
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
				"message_count": count,
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
