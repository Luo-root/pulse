// Package redis 提供基于 RediSearch 的消息检索
// 依赖 Redis Stack（>= 7.0）的 RediSearch 模块
package redis

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	goredis "github.com/redis/go-redis/v9"
)

// RetrieverConfig RediSearch 检索配置
type RetrieverConfig struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string // 默认 "pulse:"
	IndexName string // 默认 "pulse:idx"
	VectorDim int    // 默认 768
}

func DefaultRetrieverConfig() *RetrieverConfig {
	return &RetrieverConfig{
		Addr:      "localhost:6379",
		KeyPrefix: "pulse:",
		IndexName: "pulse:idx",
		VectorDim: 768,
	}
}

type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)

// Retriever 基于 RediSearch 的检索器
type Retriever struct {
	rdb       *goredis.Client
	config    *RetrieverConfig
	embedding EmbeddingFunc
}

func NewRetriever(config *RetrieverConfig, embedding EmbeddingFunc) (*Retriever, error) {
	if config == nil {
		config = DefaultRetrieverConfig()
	}
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     config.Addr,
		Password: config.Password,
		DB:       config.DB,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis retriever: connect failed: %w", err)
	}
	r := &Retriever{rdb: rdb, config: config, embedding: embedding}
	if err := r.ensureIndex(ctx); err != nil {
		return nil, fmt.Errorf("redis retriever: create index: %w", err)
	}
	return r, nil
}

// NewRetrieverFromStore 从已有 Store 创建 Retriever（共享连接）
func NewRetrieverFromStore(store *Store, config *RetrieverConfig, embedding EmbeddingFunc) (*Retriever, error) {
	if config == nil {
		config = DefaultRetrieverConfig()
		config.Addr = store.config.Addr
		config.Password = store.config.Password
		config.DB = store.config.DB
		config.KeyPrefix = store.config.KeyPrefix
	}
	rdb := store.GetClient()
	r := &Retriever{rdb: rdb, config: config, embedding: embedding}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.ensureIndex(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Retriever) ensureIndex(ctx context.Context) error {
	_, err := r.rdb.FTInfo(ctx, r.config.IndexName).Result()
	if err == nil {
		return nil
	}
	fields := []*goredis.FieldSchema{
		{FieldName: "session_id", As: "session_id", FieldType: goredis.SearchFieldTypeTag},
		{FieldName: "role", As: "role", FieldType: goredis.SearchFieldTypeTag},
		{FieldName: "content", As: "content", FieldType: goredis.SearchFieldTypeText},
		{FieldName: "timestamp", As: "timestamp", FieldType: goredis.SearchFieldTypeNumeric},
	}
	if r.embedding != nil {
		fields = append(fields, &goredis.FieldSchema{
			FieldName: "embedding",
			As:        "embedding",
			FieldType: goredis.SearchFieldTypeVector,
			VectorArgs: &goredis.FTVectorArgs{
				FlatOptions: &goredis.FTFlatOptions{
					Type:           "FLOAT32",
					Dim:            r.config.VectorDim,
					DistanceMetric: "COSINE",
				},
			},
		})
	}
	return r.rdb.FTCreate(ctx, r.config.IndexName, &goredis.FTCreateOptions{
		OnHash: true,
		Prefix: []interface{}{r.config.KeyPrefix + "msg:"},
	}, fields...).Err()
}

// Indexer 接口实现

func (r *Retriever) AddToIndex(ctx context.Context, id string, content string, embedding []float32) error {
	fields := map[string]interface{}{
		"id": id,
	}
	if len(embedding) > 0 {
		fields["embedding"] = vecToBytes(embedding)
	}
	return r.rdb.HSet(ctx, r.config.KeyPrefix+"msg:"+id, fields).Err()
}

func (r *Retriever) RemoveFromIndex(ctx context.Context, sessionID string) error {
	pattern := r.config.KeyPrefix + "msg:*"
	var cursor uint64
	for {
		keys, nextCursor, err := r.rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			sid, _ := r.rdb.HGet(ctx, key, "session_id").Result()
			if sid == sessionID {
				r.rdb.Del(ctx, key)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

func (r *Retriever) RebuildIndex(ctx context.Context, msgs []IndexItem) error {
	pipe := r.rdb.Pipeline()
	for _, msg := range msgs {
		fields := map[string]interface{}{
			"id":         msg.ID,
			"session_id": msg.SessionID,
			"role":       msg.Role,
			"content":    msg.Content,
			"timestamp":  msg.Timestamp,
		}
		if len(msg.Embedding) > 0 {
			fields["embedding"] = vecToBytes(msg.Embedding)
		}
		pipe.HSet(ctx, r.config.KeyPrefix+"msg:"+msg.ID, fields)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Retriever) IndexReady() bool {
	return true // RediSearch 索引是同步的
}

// IndexItem 用于重建索引的数据项
type IndexItem struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	Timestamp int64
	Embedding []float32
}

// Retriever 接口实现

func (r *Retriever) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	if topK <= 0 {
		topK = 3
	}
	if r.embedding != nil {
		results, err := r.vectorRecall(ctx, sessionID, query, topK)
		if err == nil && len(results) > 0 {
			return results, nil
		}
	}
	return r.keywordRecall(ctx, sessionID, query, topK)
}

func (r *Retriever) vectorRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	queryVec, err := r.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return nil, fmt.Errorf("embedding failed")
	}
	vecBytes := vecToBytes(queryVec)
	// 使用 Do 发送原始 FT.SEARCH 命令，向量以原始字节传递
	rawArgs := []interface{}{
		"FT.SEARCH", r.config.IndexName,
		"(*)=>[KNN 50 @embedding $vec AS __vector_distance]",
		"PARAMS", "2", "vec", vecBytes,
		"DIALECT", "2",
		"RETURN", "4", "session_id", "role", "content", "timestamp",
		"LIMIT", "0", fmt.Sprintf("%d", topK*10),
	}
	rawResult, err := r.rdb.Do(ctx, rawArgs...).Result()
	if err != nil {
		return nil, fmt.Errorf("FT.SEARCH vector: %w", err)
	}
	if rawResult == nil {
		return nil, nil
	}

	// 解析 RediSearch RESP3 结果格式
	resultMap := toStrMap(rawResult)
	if resultMap == nil {
		return nil, fmt.Errorf("unexpected result type: %T", rawResult)
	}
	totalFloat, _ := resultMap["total_results"].(int64)
	if totalFloat == 0 {
		return nil, nil
	}

	type scored struct {
		msg       *schema.Message
		timestamp int64
	}
	var scoredList []scored

	resultsList, ok := resultMap["results"].([]interface{})
	if !ok {
		return nil, nil
	}
	for _, item := range resultsList {
		docMap := toStrMap(item)
		if docMap == nil {
			continue
		}
		attrs := toStrMap(docMap["extra_attributes"])
		if attrs == nil {
			continue
		}

		sid := fmt.Sprintf("%v", attrs["session_id"])
		if sid != sessionID {
			continue
		}
		role := fmt.Sprintf("%v", attrs["role"])
		content := fmt.Sprintf("%v", attrs["content"])
		ts, _ := strconv.ParseInt(fmt.Sprintf("%v", attrs["timestamp"]), 10, 64)
		scoredList = append(scoredList, scored{
			msg:       &schema.Message{Role: schema.RoleType(role), Content: content},
			timestamp: ts,
		})
	}
	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].timestamp > scoredList[j].timestamp
	})
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}
	results := make([]*schema.Message, len(scoredList))
	for i, sc := range scoredList {
		results[i] = sc.msg
	}
	return results, nil
}

func (r *Retriever) keywordRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	keywords := extractKeywords(query)
	if len(keywords) == 0 {
		return nil, nil
	}
	// 用关键词搜索，不过滤 session（在 Go 层过滤）
	var conditions []string
	for _, kw := range keywords {
		conditions = append(conditions, fmt.Sprintf("@content:(%s)", escapeQuery(kw)))
	}
	searchQuery := strings.Join(conditions, " | ")
	res, err := r.rdb.FTSearchWithArgs(ctx, r.config.IndexName, searchQuery, &goredis.FTSearchOptions{
		Return: []goredis.FTSearchReturn{
			{FieldName: "session_id"},
			{FieldName: "role"},
			{FieldName: "content"},
			{FieldName: "timestamp"},
		},
		LimitOffset: 0,
		Limit:       topK * 10,
	}).Result()
	if err != nil {
		return nil, err
	}
	now := float64(time.Now().UnixMilli())
	type scored struct {
		msg   *schema.Message
		score float64
	}
	var scoredList []scored
	for _, doc := range res.Docs {
		if doc.Error != nil {
			continue
		}
		// 过滤 session
		if doc.Fields["session_id"] != sessionID {
			continue
		}
		ts, _ := strconv.ParseInt(doc.Fields["timestamp"], 10, 64)
		age := now - float64(ts)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / (7 * 24 * 3600 * 1000))
		keywordScore := 0.0
		content := strings.ToLower(doc.Fields["content"])
		for _, kw := range keywords {
			if strings.Contains(content, strings.ToLower(kw)) {
				keywordScore += 1.0
			}
		}
		if len(keywords) > 0 {
			keywordScore /= float64(len(keywords))
		}
		score := timeScore*0.3 + keywordScore*0.7
		scoredList = append(scoredList, scored{
			msg:   &schema.Message{Role: schema.RoleType(doc.Fields["role"]), Content: doc.Fields["content"]},
			score: score,
		})
	}
	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].score > scoredList[j].score
	})
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}
	results := make([]*schema.Message, len(scoredList))
	for i, sc := range scoredList {
		results[i] = sc.msg
	}
	return results, nil
}

func (r *Retriever) Close() error {
	return r.rdb.Close()
}

// 辅助函数

func vecToBytes(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		bits := math.Float32bits(v)
		buf[i*4] = byte(bits)
		buf[i*4+1] = byte(bits >> 8)
		buf[i*4+2] = byte(bits >> 16)
		buf[i*4+3] = byte(bits >> 24)
	}
	return buf
}

func vecToBase64(vec []float32) string {
	return base64.StdEncoding.EncodeToString(vecToBytes(vec))
}

// toStrMap 将 map[interface{}]interface{} 或 map[string]interface{} 统一转为 map[string]interface{}
func toStrMap(v interface{}) map[string]interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		return m
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(m))
		for k, val := range m {
			result[fmt.Sprintf("%v", k)] = val
		}
		return result
	default:
		return nil
	}
}

func vecToString(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%f", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func escapeTag(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\", ",", "\\,", ".", "\\.", "<", "\\<", ">", "\\>",
		"{", "\\{", "}", "\\}", "[", "\\[", "]", "\\]", "\"", "\\\"",
		"'", "\\'", ":", "\\:", ";", "\\;", "!", "\\!", "@", "\\@",
		"#", "\\#", "$", "\\$", "%", "\\%", "^", "\\^", "&", "\\&",
		"*", "\\*", "(", "\\(", ")", "\\)",
	)
	return r.Replace(s)
}

func escapeQuery(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\", ",", "\\,", ".", "\\.", "<", "\\<", ">", "\\>",
		"{", "\\{", "}", "\\}", "[", "\\[", "]", "\\]", "\"", "\\\"",
		"'", "\\'", ":", "\\:", ";", "\\;", "!", "\\!", "@", "\\@",
		"#", "\\#", "$", "\\$", "%", "\\%", "^", "\\^", "&", "\\&",
		"*", "\\*", "(", "\\(", ")", "\\)", "|", "\\|", "-", "\\-",
		"~", "\\~",
	)
	return r.Replace(s)
}

func extractKeywords(text string) []string {
	text = strings.ToLower(text)
	for _, r := range []string{",", ".", "!", "?", ";", ":", "(", ")", "[", "]", "，", "。", "！", "？", "；", "："} {
		text = strings.ReplaceAll(text, r, " ")
	}
	words := strings.Fields(text)
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"this": true, "that": true, "it": true, "i": true, "you": true,
		"we": true, "they": true, "to": true, "of": true, "in": true,
		"for": true, "on": true, "and": true, "but": true, "or": true,
		"not": true,
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
