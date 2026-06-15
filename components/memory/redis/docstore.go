package redis

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// DocStore 基于 Redis 的通用文档存储
type DocStore struct {
	store *Store
}

// NewDocStore 创建通用文档存储
func NewDocStore(store *Store) *DocStore {
	return &DocStore{store: store}
}

func (d *DocStore) docKey(collection, id string) string {
	return d.store.config.KeyPrefix + "doc:" + collection + ":" + id
}

func (d *DocStore) collKey(collection string) string {
	return d.store.config.KeyPrefix + "coll:" + collection
}

func (d *DocStore) SaveDocuments(ctx context.Context, collection string, docs []*schema.Document) error {
	if len(docs) == 0 {
		return nil
	}
	pipe := d.store.rdb.Pipeline()
	baseTime := time.Now().UnixMilli()

	for i, doc := range docs {
		id := doc.ID
		if id == "" {
			id = fmt.Sprintf("%s_%d_%d", collection, baseTime, i)
		}

		fields := map[string]interface{}{
			"id":         id,
			"collection": collection,
			"content":    doc.Content,
			"timestamp":  baseTime + int64(i),
		}
		if doc.MetaData != nil {
			for k, v := range doc.MetaData {
				fields["meta_"+k] = fmt.Sprintf("%v", v)
			}
		}

		pipe.HSet(ctx, d.docKey(collection, id), fields)
		pipe.SAdd(ctx, d.collKey(collection), id)
	}

	_, err := pipe.Exec(ctx)
	return err
}

func (d *DocStore) RecallDocuments(ctx context.Context, collection string, query string, topK int) ([]*schema.Document, error) {
	if topK <= 0 {
		topK = 5
	}

	keywords := extractKeywords(query)
	if len(keywords) == 0 {
		return d.GetDocuments(ctx, collection)
	}

	// 搜索该集合下的所有文档
	ids, err := d.store.rdb.SMembers(ctx, d.collKey(collection)).Result()
	if err != nil {
		return nil, err
	}

	now := float64(time.Now().UnixMilli())
	type scored struct {
		doc   *schema.Document
		score float64
	}
	var scoredList []scored

	for _, id := range ids {
		data, err := d.store.rdb.HGetAll(ctx, d.docKey(collection, id)).Result()
		if err != nil || len(data) == 0 {
			continue
		}

		content := data["content"]
		ts, _ := strconv.ParseInt(data["timestamp"], 10, 64)

		// 关键词匹配
		keywordScore := 0.0
		lowerContent := strings.ToLower(content)
		for _, kw := range keywords {
			if strings.Contains(lowerContent, strings.ToLower(kw)) {
				keywordScore += 1.0
			}
		}
		if len(keywords) > 0 {
			keywordScore /= float64(len(keywords))
		}

		// 时间衰减
		age := now - float64(ts)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / (7 * 24 * 3600 * 1000))

		score := timeScore*0.3 + keywordScore*0.7

		// 构建元数据
		meta := make(map[string]any)
		for k, v := range data {
			if strings.HasPrefix(k, "meta_") {
				meta[strings.TrimPrefix(k, "meta_")] = v
			}
		}

		doc := &schema.Document{
			ID:       data["id"],
			Content:  content,
			MetaData: meta,
		}
		scoredList = append(scoredList, scored{doc: doc, score: score})
	}

	sort.Slice(scoredList, func(i, j int) bool {
		return scoredList[i].score > scoredList[j].score
	})
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}

	results := make([]*schema.Document, len(scoredList))
	for i, sc := range scoredList {
		results[i] = sc.doc
	}
	return results, nil
}

func (d *DocStore) GetDocuments(ctx context.Context, collection string) ([]*schema.Document, error) {
	ids, err := d.store.rdb.SMembers(ctx, d.collKey(collection)).Result()
	if err != nil {
		return nil, err
	}

	var results []*schema.Document
	for _, id := range ids {
		data, err := d.store.rdb.HGetAll(ctx, d.docKey(collection, id)).Result()
		if err != nil || len(data) == 0 {
			continue
		}

		meta := make(map[string]any)
		for k, v := range data {
			if strings.HasPrefix(k, "meta_") {
				meta[strings.TrimPrefix(k, "meta_")] = v
			}
		}

		results = append(results, &schema.Document{
			ID:       data["id"],
			Content:  data["content"],
			MetaData: meta,
		})
	}
	return results, nil
}

func (d *DocStore) DeleteCollection(ctx context.Context, collection string) error {
	ids, err := d.store.rdb.SMembers(ctx, d.collKey(collection)).Result()
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = d.docKey(collection, id)
		}
		d.store.rdb.Del(ctx, keys...)
	}
	return d.store.rdb.Del(ctx, d.collKey(collection)).Err()
}

func (d *DocStore) Close() error {
	return nil
}
