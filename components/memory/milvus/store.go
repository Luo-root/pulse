// Package milvus 提供基于 Milvus 的消息存储
package milvus

import (
	"context"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// StoreConfig Milvus 存储配置
type StoreConfig struct {
	Addr       string // 默认 "localhost:19530"
	Collection string // 默认 "pulse_messages"
	VectorDim  int    // 默认 768

	// 认证配置（可选）
	Username string // Milvus 用户名
	Password string // Milvus 密码
	DBName   string // 数据库名
	APIKey   string // API Key
	TLS      bool   // 启用 TLS
}

func DefaultStoreConfig() *StoreConfig {
	return &StoreConfig{
		Addr:       "localhost:19530",
		Collection: "pulse_messages",
		VectorDim:  768,
	}
}

// Store 基于 Milvus 的消息持久化存储
type Store struct {
	client client.Client
	config *StoreConfig
}

func NewStore(config *StoreConfig) (*Store, error) {
	if config == nil {
		config = DefaultStoreConfig()
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
		return nil, fmt.Errorf("milvus store: connect failed: %w", err)
	}
	s := &Store{client: c, config: config}
	if err := s.ensureCollection(context.Background()); err != nil {
		return nil, fmt.Errorf("milvus store: init collection: %w", err)
	}
	return s, nil
}

// NewStoreFromClient 从已有 client 创建 Store（与 Retriever 共享连接）
func NewStoreFromClient(c client.Client, config *StoreConfig) (*Store, error) {
	if config == nil {
		config = DefaultStoreConfig()
	}
	s := &Store{client: c, config: config}
	if err := s.ensureCollection(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureCollection(ctx context.Context) error {
	has, err := s.client.HasCollection(ctx, s.config.Collection)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	schema := &entity.Schema{
		CollectionName: s.config.Collection,
		Fields: []*entity.Field{
			entity.NewField().WithName("id").WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithMaxLength(128),
			entity.NewField().WithName("session_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(128),
			entity.NewField().WithName("role").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32),
			entity.NewField().WithName("content").WithDataType(entity.FieldTypeVarChar).WithMaxLength(65535),
			entity.NewField().WithName("timestamp").WithDataType(entity.FieldTypeInt64),
			entity.NewField().WithName("embedding").WithDataType(entity.FieldTypeFloatVector).WithDim(int64(s.config.VectorDim)),
		},
	}
	if err := s.client.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
		return err
	}
	idx, err := entity.NewIndexIvfFlat(entity.L2, 128)
	if err != nil {
		return err
	}
	return s.client.CreateIndex(ctx, s.config.Collection, "embedding", idx, false)
}

func (s *Store) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]string, len(msgs))
	sessionIDs := make([]string, len(msgs))
	roles := make([]string, len(msgs))
	contents := make([]string, len(msgs))
	timestamps := make([]int64, len(msgs))
	embeddings := make([][]float32, len(msgs))

	baseTime := time.Now().UnixMilli()
	for i, msg := range msgs {
		ids[i] = fmt.Sprintf("%s_%d_%d", sessionID, baseTime, i)
		sessionIDs[i] = sessionID
		roles[i] = string(msg.Role)
		contents[i] = msg.TextContent()
		timestamps[i] = baseTime + int64(i)
		embeddings[i] = make([]float32, s.config.VectorDim)
	}

	idCol := entity.NewColumnVarChar("id", ids)
	sessCol := entity.NewColumnVarChar("session_id", sessionIDs)
	roleCol := entity.NewColumnVarChar("role", roles)
	contentCol := entity.NewColumnVarChar("content", contents)
	tsCol := entity.NewColumnInt64("timestamp", timestamps)
	vecCol := entity.NewColumnFloatVector("embedding", s.config.VectorDim, embeddings)

	_, err := s.client.Insert(ctx, s.config.Collection, "", idCol, sessCol, roleCol, contentCol, tsCol, vecCol)
	return err
}

func (s *Store) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	s.client.LoadCollection(ctx, s.config.Collection, false)
	filter := fmt.Sprintf("session_id == \"%s\"", sessionID)
	results, err := s.client.Query(ctx, s.config.Collection, []string{}, filter, []string{"role", "content", "timestamp"})
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
	// 按时间排序
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].timestamp < items[i].timestamp {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	msgs := make([]*schema.Message, len(items))
	for i, item := range items {
		msgs[i] = item.msg
	}
	return msgs, nil
}

func (s *Store) ClearSession(ctx context.Context, sessionID string) error {
	filter := fmt.Sprintf("session_id == '%s'", sessionID)
	err := s.client.Delete(ctx, s.config.Collection, "", filter)
	if err != nil {
		return err
	}
	// Flush 确保删除生效
	s.client.Flush(ctx, s.config.Collection, false)
	return nil
}

func (s *Store) Close() error {
	s.client.Close()
	return nil
}

// GetClient 获取底层 Milvus 客户端（供 Retriever 共享连接）
func (s *Store) GetClient() client.Client {
	return s.client
}
