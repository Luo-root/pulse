// Package milvus 提供基于 Milvus 的消息检索
package milvus

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// RetrieverConfig Milvus 检索配置
type RetrieverConfig struct {
	Addr       string // 默认 "localhost:19530"
	Collection string // 默认 "pulse_messages"
	VectorDim  int    // 默认 768

	// 认证配置（可选）
	Username string
	Password string
	DBName   string
	APIKey   string
	TLS      bool
}

func DefaultRetrieverConfig() *RetrieverConfig {
	return &RetrieverConfig{
		Addr:       "localhost:19530",
		Collection: "pulse_messages",
		VectorDim:  768,
	}
}

type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)

// IndexItem 用于重建索引的数据项
type IndexItem struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	Timestamp int64
	Embedding []float32
}

// Retriever 基于 Milvus 的检索器
type Retriever struct {
	client    client.Client
	config    *RetrieverConfig
	embedding EmbeddingFunc
}

func NewRetriever(config *RetrieverConfig, embedding EmbeddingFunc) (*Retriever, error) {
	if config == nil {
		config = DefaultRetrieverConfig()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := client.NewClient(ctx, client.Config{
		Address:       config.Addr,
		Username:      config.Username,
		Password:      config.Password,
		DBName:        config.DBName,
		APIKey:        config.APIKey,
		EnableTLSAuth: config.TLS,
	})
	if err != nil {
		return nil, fmt.Errorf("milvus retriever: connect failed: %w", err)
	}
	r := &Retriever{client: c, config: config, embedding: embedding}
	if err := r.ensureCollection(context.Background()); err != nil {
		return nil, fmt.Errorf("milvus retriever: init collection: %w", err)
	}
	return r, nil
}

// NewRetrieverFromStore 从已有 Store 创建 Retriever（共享连接）
func NewRetrieverFromStore(store *Store, embedding EmbeddingFunc) (*Retriever, error) {
	config := &RetrieverConfig{
		Addr:       store.config.Addr,
		Collection: store.config.Collection,
		VectorDim:  store.config.VectorDim,
	}
	r := &Retriever{client: store.GetClient(), config: config, embedding: embedding}
	return r, nil
}

func (r *Retriever) ensureCollection(ctx context.Context) error {
	has, err := r.client.HasCollection(ctx, r.config.Collection)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	schema := &entity.Schema{
		CollectionName: r.config.Collection,
		Fields: []*entity.Field{
			entity.NewField().WithName("id").WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(128),
			entity.NewField().WithName("session_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(128),
			entity.NewField().WithName("role").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32),
			entity.NewField().WithName("content").WithDataType(entity.FieldTypeVarChar).WithMaxLength(65535),
			entity.NewField().WithName("timestamp").WithDataType(entity.FieldTypeInt64),
			entity.NewField().WithName("embedding").WithDataType(entity.FieldTypeFloatVector).WithDim(int64(r.config.VectorDim)),
		},
	}
	if err := r.client.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
		return err
	}
	idx, err := entity.NewIndexIvfFlat(entity.L2, 128)
	if err != nil {
		return err
	}
	return r.client.CreateIndex(ctx, r.config.Collection, "embedding", idx, false)
}

// Indexer 接口实现

func (r *Retriever) AddToIndex(ctx context.Context, id string, content string, embedding []float32) error {
	vec := embedding
	if len(vec) == 0 {
		vec = make([]float32, r.config.VectorDim)
	}
	idCol := entity.NewColumnVarChar("id", []string{id})
	vecCol := entity.NewColumnFloatVector("embedding", r.config.VectorDim, [][]float32{vec})
	_, err := r.client.Insert(ctx, r.config.Collection, "", idCol, vecCol)
	return err
}

func (r *Retriever) RemoveFromIndex(ctx context.Context, sessionID string) error {
	filter := fmt.Sprintf("session_id == '%s'", sessionID)
	return r.client.Delete(ctx, r.config.Collection, "", filter)
}

func (r *Retriever) RebuildIndex(ctx context.Context, items []IndexItem) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, len(items))
	sessionIDs := make([]string, len(items))
	roles := make([]string, len(items))
	contents := make([]string, len(items))
	timestamps := make([]int64, len(items))
	embeddings := make([][]float32, len(items))
	for i, item := range items {
		ids[i] = item.ID
		sessionIDs[i] = item.SessionID
		roles[i] = item.Role
		contents[i] = item.Content
		timestamps[i] = item.Timestamp
		if len(item.Embedding) > 0 {
			embeddings[i] = item.Embedding
		} else {
			embeddings[i] = make([]float32, r.config.VectorDim)
		}
	}
	idCol := entity.NewColumnVarChar("id", ids)
	sessCol := entity.NewColumnVarChar("session_id", sessionIDs)
	roleCol := entity.NewColumnVarChar("role", roles)
	contentCol := entity.NewColumnVarChar("content", contents)
	tsCol := entity.NewColumnInt64("timestamp", timestamps)
	vecCol := entity.NewColumnFloatVector("embedding", r.config.VectorDim, embeddings)
	_, err := r.client.Insert(ctx, r.config.Collection, "", idCol, sessCol, roleCol, contentCol, tsCol, vecCol)
	return err
}

func (r *Retriever) IndexReady() bool {
	return true
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
	return r.getRecent(ctx, sessionID, topK)
}

func (r *Retriever) vectorRecall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	queryVec, err := r.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return nil, fmt.Errorf("embedding failed")
	}
	r.client.LoadCollection(ctx, r.config.Collection, false)
	sp, _ := entity.NewIndexIvfFlatSearchParam(32)
	results, err := r.client.Search(ctx, r.config.Collection,
		[]string{},
		fmt.Sprintf("session_id == \"%s\"", sessionID),
		[]string{"role", "content", "timestamp"},
		[]entity.Vector{entity.FloatVector(queryVec)},
		"embedding", entity.L2, topK*3, sp,
	)
	if err != nil || len(results) == 0 {
		return nil, fmt.Errorf("vector search failed: %w", err)
	}
	type scored struct {
		msg       *schema.Message
		timestamp int64
	}
	var scoredList []scored
	for _, result := range results {
		if result.Err != nil {
			continue
		}
		roleCol := result.Fields.GetColumn("role").(*entity.ColumnVarChar)
		contentCol := result.Fields.GetColumn("content").(*entity.ColumnVarChar)
		tsCol := result.Fields.GetColumn("timestamp").(*entity.ColumnInt64)
		for i := 0; i < result.ResultCount; i++ {
			role, _ := roleCol.ValueByIdx(i)
			content, _ := contentCol.ValueByIdx(i)
			ts, _ := tsCol.ValueByIdx(i)
			scoredList = append(scoredList, scored{
				msg:       &schema.Message{Role: schema.RoleType(role), Content: content},
				timestamp: ts,
			})
		}
	}
	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].timestamp > scoredList[j].timestamp
	})
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}
	msgs := make([]*schema.Message, len(scoredList))
	for i, sc := range scoredList {
		msgs[i] = sc.msg
	}
	return msgs, nil
}

func (r *Retriever) getRecent(ctx context.Context, sessionID string, limit int) ([]*schema.Message, error) {
	filter := fmt.Sprintf("session_id == \"%s\"", sessionID)
	results, err := r.client.Query(ctx, r.config.Collection, []string{}, filter, []string{"role", "content", "timestamp"})
	if err != nil {
		return nil, err
	}
	type timed struct {
		msg       *schema.Message
		timestamp int64
	}
	var items []timed
	roleCol := results.GetColumn("role").(*entity.ColumnVarChar)
	contentCol := results.GetColumn("content").(*entity.ColumnVarChar)
	tsCol := results.GetColumn("timestamp").(*entity.ColumnInt64)
	for i := 0; i < results.Len(); i++ {
		role, _ := roleCol.ValueByIdx(i)
		content, _ := contentCol.ValueByIdx(i)
		ts, _ := tsCol.ValueByIdx(i)
		items = append(items, timed{
			msg:       &schema.Message{Role: schema.RoleType(role), Content: content},
			timestamp: ts,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].timestamp > items[j].timestamp
	})
	if len(items) > limit {
		items = items[:limit]
	}
	msgs := make([]*schema.Message, len(items))
	for i, item := range items {
		msgs[i] = item.msg
	}
	return msgs, nil
}

func (r *Retriever) Close() error {
	r.client.Close()
	return nil
}

var _ = time.Now
