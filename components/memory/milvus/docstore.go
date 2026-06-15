package milvus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// DocStore 基于 Milvus 的通用文档存储
type DocStore struct {
	client    *Store
	config    *DocStoreConfig
	embedding EmbeddingFunc
}

type DocStoreConfig struct {
	Addr       string
	Collection string // Milvus collection 名
	VectorDim  int
	Username   string
	Password   string
	DBName     string
	APIKey     string
	TLS        bool
}

func DefaultDocStoreConfig() *DocStoreConfig {
	return &DocStoreConfig{
		Addr:       "localhost:19530",
		Collection: "pulse_documents",
		VectorDim:  768,
	}
}

func NewDocStore(config *DocStoreConfig, embedding EmbeddingFunc) (*DocStore, error) {
	if config == nil {
		config = DefaultDocStoreConfig()
	}

	storeCfg := &StoreConfig{
		Addr:       config.Addr,
		Collection: config.Collection,
		VectorDim:  config.VectorDim,
		Username:   config.Username,
		Password:   config.Password,
		DBName:     config.DBName,
		APIKey:     config.APIKey,
		TLS:        config.TLS,
	}

	store, err := NewStore(storeCfg)
	if err != nil {
		return nil, err
	}

	return &DocStore{client: store, config: config, embedding: embedding}, nil
}

func (d *DocStore) SaveDocuments(ctx context.Context, collection string, docs []*schema.Document) error {
	if len(docs) == 0 {
		return nil
	}

	baseTime := time.Now().UnixMilli()
	ids := make([]string, len(docs))
	collections := make([]string, len(docs))
	contents := make([]string, len(docs))
	timestamps := make([]int64, len(docs))
	metadataJSON := make([]string, len(docs))
	embeddings := make([][]float32, len(docs))

	for i, doc := range docs {
		id := doc.ID
		if id == "" {
			id = fmt.Sprintf("%s_%d_%d", collection, baseTime, i)
		}
		ids[i] = id
		collections[i] = collection
		contents[i] = doc.Content
		timestamps[i] = baseTime + int64(i)

		if doc.MetaData != nil {
			metaBytes, _ := json.Marshal(doc.MetaData)
			metadataJSON[i] = string(metaBytes)
		}

		if d.embedding != nil {
			vec, err := d.embedding(ctx, doc.Content)
			if err == nil && len(vec) > 0 {
				embeddings[i] = vec
			} else {
				embeddings[i] = make([]float32, d.config.VectorDim)
			}
		} else {
			embeddings[i] = make([]float32, d.config.VectorDim)
		}
	}

	// 确保 collection 存在
	d.ensureDocCollection(ctx)

	idCol := entity.NewColumnVarChar("id", ids)
	collCol := entity.NewColumnVarChar("collection", collections)
	contentCol := entity.NewColumnVarChar("content", contents)
	tsCol := entity.NewColumnInt64("timestamp", timestamps)
	metaCol := entity.NewColumnVarChar("metadata", metadataJSON)
	vecCol := entity.NewColumnFloatVector("embedding", d.config.VectorDim, embeddings)

	_, err := d.client.client.Insert(ctx, d.config.Collection, "", idCol, collCol, contentCol, tsCol, metaCol, vecCol)
	return err
}

func (d *DocStore) ensureDocCollection(ctx context.Context) error {
	has, err := d.client.client.HasCollection(ctx, d.config.Collection)
	if err != nil {
		return err
	}
	if has {
		// collection 存在，检查是否有 collection 字段
		// 如果 schema 不匹配，删除重建
		desc, err := d.client.client.DescribeCollection(ctx, d.config.Collection)
		if err != nil {
			return err
		}
		hasCollectionField := false
		for _, f := range desc.Schema.Fields {
			if f.Name == "collection" {
				hasCollectionField = true
				break
			}
		}
		if !hasCollectionField {
			d.client.client.DropCollection(ctx, d.config.Collection)
		} else {
			return nil
		}
	}

	schema := &entity.Schema{
		CollectionName: d.config.Collection,
		Fields: []*entity.Field{
			entity.NewField().WithName("id").WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(128),
			entity.NewField().WithName("collection").WithDataType(entity.FieldTypeVarChar).WithMaxLength(128),
			entity.NewField().WithName("content").WithDataType(entity.FieldTypeVarChar).WithMaxLength(65535),
			entity.NewField().WithName("timestamp").WithDataType(entity.FieldTypeInt64),
			entity.NewField().WithName("metadata").WithDataType(entity.FieldTypeVarChar).WithMaxLength(65535),
			entity.NewField().WithName("embedding").WithDataType(entity.FieldTypeFloatVector).WithDim(int64(d.config.VectorDim)),
		},
	}

	if err := d.client.client.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
		return err
	}
	idx, _ := entity.NewIndexIvfFlat(entity.L2, 128)
	return d.client.client.CreateIndex(ctx, d.config.Collection, "embedding", idx, false)
}

func (d *DocStore) RecallDocuments(ctx context.Context, collection string, query string, topK int) ([]*schema.Document, error) {
	if topK <= 0 {
		topK = 5
	}

	if d.embedding != nil {
		results, err := d.vectorSearch(ctx, collection, query, topK)
		if err == nil && len(results) > 0 {
			return results, nil
		}
	}

	return d.GetDocuments(ctx, collection)
}

func (d *DocStore) vectorSearch(ctx context.Context, collection string, query string, topK int) ([]*schema.Document, error) {
	queryVec, err := d.embedding(ctx, query)
	if err != nil || len(queryVec) == 0 {
		return nil, fmt.Errorf("embedding failed")
	}

	d.client.client.LoadCollection(ctx, d.config.Collection, false)
	sp, _ := entity.NewIndexIvfFlatSearchParam(32)

	filter := fmt.Sprintf("collection == '%s'", collection)
	results, err := d.client.client.Search(ctx, d.config.Collection,
		[]string{},
		filter,
		[]string{"id", "collection", "content", "timestamp", "metadata"},
		[]entity.Vector{entity.FloatVector(queryVec)},
		"embedding", entity.L2, topK*3, sp,
	)
	if err != nil || len(results) == 0 {
		return nil, fmt.Errorf("vector search failed: %w", err)
	}

	type scored struct {
		doc       *schema.Document
		timestamp int64
	}
	var scoredList []scored

	for _, result := range results {
		if result.Err != nil {
			continue
		}
		idCol := result.Fields.GetColumn("id").(*entity.ColumnVarChar)
		contentCol := result.Fields.GetColumn("content").(*entity.ColumnVarChar)
		tsCol := result.Fields.GetColumn("timestamp").(*entity.ColumnInt64)
		metaCol := result.Fields.GetColumn("metadata").(*entity.ColumnVarChar)

		for i := 0; i < result.ResultCount; i++ {
			id, _ := idCol.ValueByIdx(i)
			content, _ := contentCol.ValueByIdx(i)
			ts, _ := tsCol.ValueByIdx(i)
			metaStr, _ := metaCol.ValueByIdx(i)

			var meta map[string]any
			if metaStr != "" {
				json.Unmarshal([]byte(metaStr), &meta)
			}

			scoredList = append(scoredList, scored{
				doc:       &schema.Document{ID: id, Content: content, MetaData: meta},
				timestamp: ts,
			})
		}
	}

	// 按时间排序
	for i := 0; i < len(scoredList); i++ {
		for j := i + 1; j < len(scoredList); j++ {
			if scoredList[j].timestamp > scoredList[i].timestamp {
				scoredList[i], scoredList[j] = scoredList[j], scoredList[i]
			}
		}
	}
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}

	results2 := make([]*schema.Document, len(scoredList))
	for i, sc := range scoredList {
		results2[i] = sc.doc
	}
	return results2, nil
}

func (d *DocStore) GetDocuments(ctx context.Context, collection string) ([]*schema.Document, error) {
	d.client.client.LoadCollection(ctx, d.config.Collection, false)

	filter := fmt.Sprintf("collection == '%s'", collection)
	qResults, err := d.client.client.Query(ctx, d.config.Collection, []string{}, filter,
		[]string{"id", "content", "timestamp", "metadata"})
	if err != nil {
		return nil, err
	}

	idCol := qResults.GetColumn("id").(*entity.ColumnVarChar)
	contentCol := qResults.GetColumn("content").(*entity.ColumnVarChar)
	metaCol := qResults.GetColumn("metadata").(*entity.ColumnVarChar)

	results := make([]*schema.Document, qResults.Len())
	for i := 0; i < qResults.Len(); i++ {
		id, _ := idCol.ValueByIdx(i)
		content, _ := contentCol.ValueByIdx(i)
		metaStr, _ := metaCol.ValueByIdx(i)

		var meta map[string]any
		if metaStr != "" {
			json.Unmarshal([]byte(metaStr), &meta)
		}

		results[i] = &schema.Document{ID: id, Content: content, MetaData: meta}
	}
	return results, nil
}

func (d *DocStore) DeleteCollection(ctx context.Context, collection string) error {
	d.client.client.LoadCollection(ctx, d.config.Collection, false)
	filter := fmt.Sprintf("collection == '%s'", collection)
	err := d.client.client.Delete(ctx, d.config.Collection, "", filter)
	if err != nil {
		return err
	}
	d.client.client.Flush(ctx, d.config.Collection, false)
	return nil
}

func (d *DocStore) Close() error {
	return d.client.Close()
}
