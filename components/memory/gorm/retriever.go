package gorm

import (
	"context"
	"encoding/gob"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/coder/hnsw"
	"gorm.io/gorm"
)

// ============================================================================
// HNSWRetriever 纯检索实现
// ============================================================================

// indexNode 实现 IndexItem 接口，用于 HNSW 索引
type indexNode struct {
	id      string
	content string
	vec     []float32
}

func (n *indexNode) GetID() string           { return n.id }
func (n *indexNode) GetContent() string      { return n.content }
func (n *indexNode) GetEmbedding() []float32 { return n.vec }

// 向后兼容 HNSW 库的 Embeddable 接口

func (n *indexNode) ID() string           { return n.id }
func (n *indexNode) Embedding() []float32 { return n.vec }

// vectorCandidate 向量搜索候选结果
type vectorCandidate struct {
	id         string
	content    string
	timestamp  int64
	similarity float64
}

// HNSWRetriever 基于 HNSW 的向量检索器
// 负责向量索引管理和多种召回策略
type HNSWRetriever struct {
	db        *gorm.DB
	config    *Config
	embedding EmbeddingFunc

	vecIndex     *hnsw.Graph[*indexNode]
	vecMu        sync.RWMutex
	indexedNodes map[string][]float32
	indexReady   atomic.Bool
}

// NewHNSWRetriever 创建 HNSW 检索器
// db 应来自 GORMStore.GetDB()，共享同一数据库连接
func NewHNSWRetriever(db *gorm.DB, embedding EmbeddingFunc, config *Config) *HNSWRetriever {
	if config == nil {
		config = DefaultConfig()
	}

	r := &HNSWRetriever{
		db:           db,
		config:       config,
		embedding:    embedding,
		indexedNodes: make(map[string][]float32),
	}

	if !config.DisableVectorSearch && embedding != nil {
		r.vecIndex = hnsw.NewGraph[*indexNode]()
		r.vecIndex.M = config.HNSW_M
		r.vecIndex.Ml = config.HNSW_Ml
		r.vecIndex.EfSearch = config.HNSW_EfSearch

		go func() {
			if !r.loadIndexCache() {
				r.rebuildIndexFromDB()
				r.saveIndexCache()
			}
			r.indexReady.Store(true)
		}()
	} else {
		r.indexReady.Store(true)
	}

	return r
}

// ============================================================================
// Retriever 接口实现
// ============================================================================

// Recall 智能召回（多策略混合）
func (r *HNSWRetriever) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	if topK <= 0 {
		topK = r.config.DefaultTopK
	}

	switch r.config.RecallMode {
	case RecallModeVector:
		results, err := r.vectorRecall(ctx, sessionID, query, topK)
		if err != nil || len(results) == 0 {
			return r.hybridRecall(ctx, sessionID, query, topK)
		}
		return results, nil

	case RecallModeHybrid:
		return r.hybridRecall(ctx, sessionID, query, topK)

	case RecallModeCombined:
		return r.combinedRecall(ctx, sessionID, query, topK)

	default: // RecallModeAuto
		if !r.config.DisableVectorSearch && r.embedding != nil && r.indexReady.Load() {
			results, err := r.vectorRecall(ctx, sessionID, query, topK)
			if err == nil && len(results) > 0 {
				return results, nil
			}
		}
		return r.hybridRecall(ctx, sessionID, query, topK)
	}
}

// Close 关闭检索器
func (r *HNSWRetriever) Close() error {
	r.saveIndexCache()
	r.vecMu.Lock()
	r.vecIndex = nil
	r.vecMu.Unlock()
	return nil
}

// ============================================================================
// Indexer 接口实现
// ============================================================================

// AddToIndex 将消息的嵌入向量添加到索引
func (r *HNSWRetriever) AddToIndex(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if r.vecIndex == nil || r.embedding == nil {
		return nil
	}

	for _, msg := range msgs {
		content := msg.TextContent()
		if content == "" {
			continue
		}

		embedText := content
		if msg.ReasoningContent != "" {
			embedText = msg.ReasoningContent + " " + embedText
		}

		vec, err := r.embedding(ctx, embedText)
		if err != nil || !isValidVector(vec) {
			continue
		}

		node := &indexNode{
			id:  fmt.Sprintf("%s_*", sessionID),
			vec: vec,
		}

		r.vecMu.Lock()
		if r.vecIndex != nil {
			func() {
				defer func() { recover() }()
				r.vecIndex.Add(node)
				r.indexedNodes[node.ID()] = vec
			}()
		}
		r.vecMu.Unlock()
	}
	return nil
}

// RemoveFromIndex 从索引中移除指定会话的所有向量
func (r *HNSWRetriever) RemoveFromIndex(sessionID string) error {
	if r.vecIndex == nil {
		return nil
	}

	var ids []string
	if err := r.db.Model(&MessageModel{}).
		Where("session_id = ?", sessionID).
		Pluck("id", &ids).Error; err != nil {
		return err
	}

	r.vecMu.Lock()
	for _, id := range ids {
		if r.vecIndex != nil {
			r.vecIndex.Delete(id)
		}
		delete(r.indexedNodes, id)
	}

	var chunkIDs []string
	if err := r.db.Model(&EmbeddingChunk{}).
		Where("session_id = ?", sessionID).
		Pluck("id", &chunkIDs).Error; err == nil {
		for _, cid := range chunkIDs {
			if r.vecIndex != nil {
				r.vecIndex.Delete(cid)
			}
			delete(r.indexedNodes, cid)
		}
	}
	r.vecMu.Unlock()

	return nil
}

// RebuildIndex 从存储中重建完整索引
func (r *HNSWRetriever) RebuildIndex() {
	if r.vecIndex == nil {
		return
	}

	r.vecMu.Lock()
	r.vecIndex = hnsw.NewGraph[*indexNode]()
	r.vecIndex.M = r.config.HNSW_M
	r.vecIndex.Ml = r.config.HNSW_Ml
	r.vecIndex.EfSearch = r.config.HNSW_EfSearch
	r.indexedNodes = make(map[string][]float32)
	r.vecMu.Unlock()

	r.rebuildIndexFromDB()
	r.saveIndexCache()
}

// IndexReady 返回向量索引是否就绪
func (r *HNSWRetriever) IndexReady() bool {
	return r.indexReady.Load()
}

// ============================================================================
// 向量搜索核心
// ============================================================================

// vectorSearch 执行 HNSW 搜索并返回带相似度的候选，限定 sessionID
func (r *HNSWRetriever) vectorSearch(ctx context.Context, sessionID string, queryVec []float32, topK int) ([]vectorCandidate, error) {
	r.vecMu.RLock()
	defer r.vecMu.RUnlock()

	if r.vecIndex == nil {
		return nil, fmt.Errorf("vector index not initialized")
	}

	// HNSW Search 在图为空或维度不匹配时会 panic，安全捕获
	fetchN := topK * 3
	if fetchN < 10 {
		fetchN = 10
	}

	var nodes []*indexNode
	func() {
		defer func() {
			if r := recover(); r != nil {
				nodes = nil
			}
		}()
		nodes = r.vecIndex.Search(queryVec, fetchN)
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
		var content string
		var ts int64

		if strings.Contains(nodeID, "_chunk_") {
			var chunk EmbeddingChunk
			if err := r.db.WithContext(ctx).Where("id = ? AND session_id = ?", nodeID, sessionID).First(&chunk).Error; err != nil {
				continue
			}
			msgID = chunk.MessageID
			content = chunk.Content
			sim = cosineSimilarity(queryVec, node.Embedding())
			// 获取父消息的时间戳
			var parentMsg MessageModel
			if err := r.db.WithContext(ctx).Where("id = ?", msgID).First(&parentMsg).Error; err == nil {
				ts = parentMsg.Timestamp
			}
		} else {
			msgID = nodeID
			content = node.GetContent()
			sim = cosineSimilarity(queryVec, node.Embedding())
			var msg MessageModel
			if err := r.db.WithContext(ctx).Where("id = ? AND session_id = ?", nodeID, sessionID).First(&msg).Error; err != nil {
				continue
			}
			ts = msg.Timestamp
		}

		if existing, exists := msgMap[msgID]; exists {
			if sim > existing.similarity {
				existing.similarity = sim
			}
			continue
		}

		msgMap[msgID] = &vectorCandidate{id: msgID, content: content, timestamp: ts, similarity: sim}
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
func (r *HNSWRetriever) vectorRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	if r.embedding == nil {
		return nil, fmt.Errorf("vector recall: embedding function is nil")
	}

	queryVec, err := r.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return r.hybridRecall(ctx, sessionID, query, topK)
	}

	candidates, err := r.vectorSearch(ctx, sessionID, queryVec, topK)
	if err != nil {
		return r.hybridRecall(ctx, sessionID, query, topK)
	}

	results := make([]*schema.Message, len(candidates))
	for i, c := range candidates {
		results[i] = &schema.Message{Role: schema.AssistantRole, Content: c.content}
	}
	return results, nil
}

// combinedRecall 组合召回（向量 + 关键词 + 时间衰减）
func (r *HNSWRetriever) combinedRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	if r.embedding == nil {
		return r.hybridRecall(ctx, sessionID, query, topK)
	}

	queryVec, err := r.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return r.hybridRecall(ctx, sessionID, query, topK)
	}

	if r.vecIndex == nil {
		return r.hybridRecall(ctx, sessionID, query, topK)
	}

	candidates, err := r.vectorSearch(ctx, sessionID, queryVec, topK*r.config.CombinedCandidateMult)
	if err != nil || len(candidates) == 0 {
		return r.hybridRecall(ctx, sessionID, query, topK)
	}

	keywords := extractKeywords(query)
	now := float64(time.Now().UnixMilli())
	weights := r.config.CombinedWeights
	if weights == nil {
		weights = DefaultCombinedWeights()
	}

	type scored struct {
		id      string
		content string
		score   float64
	}
	scoredList := make([]scored, 0, len(candidates))

	for _, c := range candidates {
		vecScore := c.similarity

		keywordScore := 0.0
		lowerContent := strings.ToLower(c.content)
		for _, kw := range keywords {
			if strings.Contains(lowerContent, strings.ToLower(kw)) {
				keywordScore += 1.0
			}
		}
		if len(keywords) > 0 {
			keywordScore /= float64(len(keywords))
		}

		age := now - float64(c.timestamp)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / r.config.TimeDecayHalfLifeMs)

		total := weights.VectorWeight*vecScore +
			weights.KeywordWeight*keywordScore +
			weights.TimeWeight*timeScore

		scoredList = append(scoredList, scored{id: c.id, content: c.content, score: total})
	}

	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].score > scoredList[j].score
	})

	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}

	results := make([]*schema.Message, len(scoredList))
	for i, sc := range scoredList {
		results[i] = &schema.Message{Role: schema.AssistantRole, Content: sc.content}
	}
	return results, nil
}

// hybridRecall 混合召回（关键词 + 时间衰减）
func (r *HNSWRetriever) hybridRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	// 分词提取关键词
	keywords := extractKeywords(query)

	// 构建查询条件
	var models []MessageModel
	db := r.db.WithContext(ctx).Where("session_id = ?", sessionID)

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
		age := now - float64(models[i].Timestamp)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / r.config.TimeDecayHalfLifeMs)

		keywordScore := 0.0
		content := strings.ToLower(models[i].Content)
		for _, kw := range keywords {
			if strings.Contains(content, strings.ToLower(kw)) {
				keywordScore += 1.0
			}
		}
		if len(keywords) > 0 {
			keywordScore /= float64(len(keywords))
		}

		score := timeScore*r.config.HybridTimeWeight + keywordScore*r.config.HybridKeywordWeight
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

// ============================================================================
// HNSW 索引内部操作
// ============================================================================

// addToIndex 安全地向 HNSW 索引添加节点
func (r *HNSWRetriever) addToIndex(node *indexNode) {
	vec := node.Embedding()
	if r.vecIndex == nil || !isValidVector(vec) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// HNSW Add panic，跳过此节点
		}
	}()
	r.vecIndex.Add(node)
	r.indexedNodes[node.ID()] = vec
}

func (r *HNSWRetriever) rebuildIndexFromDB() {
	defer r.indexReady.Store(true)

	batchSize := r.config.IndexRebuildBatchSize
	var offset int

	r.vecMu.Lock()
	defer r.vecMu.Unlock()

	for {
		var batch []MessageModel
		if err := r.db.Offset(offset).Limit(batchSize).Order("timestamp ASC").Find(&batch).Error; err != nil {
			break
		}
		if len(batch) == 0 {
			break
		}

		for i := range batch {
			vec, err := batch[i].GetEmbedding()
			if err != nil || !isValidVector(vec) {
				continue
			}
			r.addToIndex(&indexNode{id: batch[i].ID, content: batch[i].Content, vec: vec})
		}

		if len(batch) < batchSize {
			break
		}
		offset += batchSize
	}

	offset = 0
	for {
		var batch []EmbeddingChunk
		if err := r.db.Offset(offset).Limit(batchSize).Order("chunk_index ASC").Find(&batch).Error; err != nil {
			break
		}
		if len(batch) == 0 {
			break
		}

		for i := range batch {
			vec, err := batch[i].GetEmbedding()
			if err != nil || !isValidVector(vec) {
				continue
			}
			r.addToIndex(&indexNode{id: batch[i].ID, content: batch[i].Content, vec: vec})
		}

		if len(batch) < batchSize {
			break
		}
		offset += batchSize
	}
}

// ============================================================================
// HNSW 索引缓存
// ============================================================================

type indexCacheEntry struct {
	ID  string
	Vec []float32
}

type indexCacheData struct {
	Entries      []indexCacheEntry
	MessageCount int64
	ChunkCount   int64
}

// loadIndexCache 从磁盘加载索引缓存，返回是否成功
func (r *HNSWRetriever) loadIndexCache() bool {
	if r.config.IndexCachePath == "" {
		return false
	}

	f, err := os.Open(r.config.IndexCachePath)
	if err != nil {
		return false
	}
	defer f.Close()

	var cache indexCacheData
	if err := gob.NewDecoder(f).Decode(&cache); err != nil {
		return false
	}

	// 验证缓存是否有效：检查 DB 中消息数量
	var msgCount, chunkCount int64
	r.db.Model(&MessageModel{}).Count(&msgCount)
	r.db.Model(&EmbeddingChunk{}).Count(&chunkCount)

	if msgCount != cache.MessageCount || chunkCount != cache.ChunkCount {
		return false
	}

	// 缓存有效，加载到索引
	r.vecMu.Lock()
	defer r.vecMu.Unlock()

	for _, entry := range cache.Entries {
		if isValidVector(entry.Vec) {
			r.addToIndex(&indexNode{
				id:  entry.ID,
				vec: entry.Vec,
			})
		}
	}
	return true
}

// saveIndexCache 将索引保存到磁盘
func (r *HNSWRetriever) saveIndexCache() {
	if r.config.IndexCachePath == "" {
		return
	}

	r.vecMu.RLock()
	entries := make([]indexCacheEntry, 0, len(r.indexedNodes))
	for id, vec := range r.indexedNodes {
		if isValidVector(vec) {
			entries = append(entries, indexCacheEntry{ID: id, Vec: vec})
		}
	}
	r.vecMu.RUnlock()

	// 查询 DB 记录数
	var msgCount, chunkCount int64
	r.db.Model(&MessageModel{}).Count(&msgCount)
	r.db.Model(&EmbeddingChunk{}).Count(&chunkCount)

	cache := indexCacheData{
		Entries:      entries,
		MessageCount: msgCount,
		ChunkCount:   chunkCount,
	}

	// 确保目录存在
	dir := filepath.Dir(r.config.IndexCachePath)
	os.MkdirAll(dir, 0755)

	f, err := os.Create(r.config.IndexCachePath)
	if err != nil {
		return
	}
	defer f.Close()

	gob.NewEncoder(f).Encode(cache)
}
