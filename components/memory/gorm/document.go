package gorm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/google/uuid"
)

// DocumentModel GORM 通用文档模型
// 用于存储任意非对话数据
type DocumentModel struct {
	ID         string `gorm:"primaryKey;size:128"`
	Collection string `gorm:"index:idx_collection;size:128;not null"`
	Content    string `gorm:"type:text;not null"`
	Embedding  string `gorm:"type:text"`
	MetaData   string `gorm:"type:text"` // JSON 序列化
	Timestamp  int64  `gorm:"not null"`
	CreatedAt  time.Time
}

func (m *DocumentModel) TableName() string {
	return "documents"
}

func (m *DocumentModel) ToDocument() *schema.Document {
	doc := &schema.Document{
		ID:      m.ID,
		Content: m.Content,
	}
	if m.MetaData != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(m.MetaData), &meta); err == nil {
			doc.MetaData = meta
		}
	}
	return doc
}

func (m *DocumentModel) GetEmbedding() []float32 {
	if m.Embedding == "" {
		return nil
	}
	var vec []float32
	json.Unmarshal([]byte(m.Embedding), &vec)
	return vec
}

// GORMDocStore 基于 GORM 的通用文档存储
type GORMDocStore struct {
	store *GORMStore
}

// NewGORMDocStore 创建通用文档存储
func NewGORMDocStore(store *GORMStore) *GORMDocStore {
	// 自动迁移 documents 表
	store.db.AutoMigrate(&DocumentModel{})
	return &GORMDocStore{store: store}
}

func (d *GORMDocStore) SaveDocuments(ctx context.Context, collection string, docs []*schema.Document) error {
	if len(docs) == 0 {
		return nil
	}

	baseTime := time.Now().UnixMilli()
	for i, doc := range docs {
		model := &DocumentModel{
			ID:         doc.ID,
			Collection: collection,
			Content:    doc.Content,
			Timestamp:  baseTime + int64(i),
			CreatedAt:  time.Now(),
		}
		if doc.ID == "" {
			model.ID = fmt.Sprintf("%s_%s", collection, uuid.New().String())
		}
		if doc.MetaData != nil {
			meta, _ := json.Marshal(doc.MetaData)
			model.MetaData = string(meta)
		}

		// 生成 embedding
		if d.store.embedding != nil {
			vec, err := d.store.embedding(ctx, doc.Content)
			if err == nil && len(vec) > 0 {
				vecData, _ := json.Marshal(vec)
				model.Embedding = string(vecData)
			}
		}

		if err := d.store.db.WithContext(ctx).Create(model).Error; err != nil {
			return err
		}
	}
	return nil
}

func (d *GORMDocStore) RecallDocuments(ctx context.Context, collection string, query string, topK int) ([]*schema.Document, error) {
	if topK <= 0 {
		topK = 5
	}

	// 关键词检索
	var models []DocumentModel
	db := d.store.db.WithContext(ctx).Where("collection = ?", collection)

	keywords := extractKeywords(query)
	if len(keywords) > 0 {
		var conditions []string
		var args []any
		for _, kw := range keywords {
			conditions = append(conditions, "content LIKE ?")
			args = append(args, "%"+kw+"%")
		}
		db = db.Where(fmt.Sprintf("(%s)", strings.Join(conditions, " OR ")), args...)
	}

	if err := db.Order("timestamp DESC").Limit(topK * 3).Find(&models).Error; err != nil {
		return nil, err
	}

	// 简单关键词评分排序
	now := float64(time.Now().UnixMilli())
	type scored struct {
		doc   *schema.Document
		score float64
	}
	var scoredList []scored
	for i := range models {
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

		age := now - float64(models[i].Timestamp)
		if age < 0 {
			age = 0
		}
		timeScore := math.Exp(-age / (7 * 24 * 3600 * 1000))
		score := timeScore*0.3 + keywordScore*0.7
		scoredList = append(scoredList, scored{doc: models[i].ToDocument(), score: score})
	}

	// 排序
	for i := 0; i < len(scoredList); i++ {
		for j := i + 1; j < len(scoredList); j++ {
			if scoredList[j].score > scoredList[i].score {
				scoredList[i], scoredList[j] = scoredList[j], scoredList[i]
			}
		}
	}
	if len(scoredList) > topK {
		scoredList = scoredList[:topK]
	}

	results := make([]*schema.Document, len(scoredList))
	for i, sc := range scoredList {
		results[i] = sc.doc
	}
	return results, nil
}

func (d *GORMDocStore) GetDocuments(ctx context.Context, collection string) ([]*schema.Document, error) {
	var models []DocumentModel
	if err := d.store.db.WithContext(ctx).Where("collection = ?", collection).Order("timestamp ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	results := make([]*schema.Document, len(models))
	for i := range models {
		results[i] = models[i].ToDocument()
	}
	return results, nil
}

func (d *GORMDocStore) DeleteCollection(ctx context.Context, collection string) error {
	return d.store.db.WithContext(ctx).Where("collection = ?", collection).Delete(&DocumentModel{}).Error
}

func (d *GORMDocStore) Close() error {
	return nil
}
