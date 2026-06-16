// Package redis 提供基于 Redis 的消息存储
// 纯 Hash 存储，不涉及检索逻辑
package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/redis/go-redis/v9"
)

// StoreConfig Redis 存储配置
type StoreConfig struct {
	Addr      string // 默认 "localhost:6379"
	Password  string
	DB        int
	KeyPrefix string // 默认 "pulse:"
}

func DefaultStoreConfig() *StoreConfig {
	return &StoreConfig{
		Addr:      "localhost:6379",
		KeyPrefix: "pulse:",
	}
}

// Store 纯 Redis Hash 消息存储
type Store struct {
	rdb    *redis.Client
	config *StoreConfig
}

func NewStore(config *StoreConfig) (*Store, error) {
	if config == nil {
		config = DefaultStoreConfig()
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     config.Addr,
		Password: config.Password,
		DB:       config.DB,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis store: connect failed: %w", err)
	}
	return &Store{rdb: rdb, config: config}, nil
}

func (s *Store) msgKey(id string) string {
	return s.config.KeyPrefix + "msg:" + id
}

func (s *Store) sessKey(sessionID string) string {
	return s.config.KeyPrefix + "sess:" + sessionID
}

func (s *Store) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	pipe := s.rdb.Pipeline()
	for i, msg := range msgs {
		id := fmt.Sprintf("%s_%d_%d", sessionID, time.Now().UnixMilli(), i)
		ts := time.Now().UnixMilli() + int64(i)
		pipe.HSet(ctx, s.msgKey(id), map[string]interface{}{
			"id":         id,
			"session_id": sessionID,
			"role":       string(msg.Role),
			"content":    msg.TextContent(),
			"timestamp":  ts,
		})
		pipe.SAdd(ctx, s.sessKey(sessionID), id)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	ids, err := s.rdb.SMembers(ctx, s.sessKey(sessionID)).Result()
	if err != nil {
		return nil, err
	}
	type timed struct {
		msg       *schema.Message
		timestamp int64
	}
	var items []timed
	for _, id := range ids {
		data, err := s.rdb.HGetAll(ctx, s.msgKey(id)).Result()
		if err != nil || len(data) == 0 {
			continue
		}
		ts, _ := strconv.ParseInt(data["timestamp"], 10, 64)
		items = append(items, timed{
			msg:       &schema.Message{Role: schema.RoleType(data["role"]), Content: data["content"]},
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
	results := make([]*schema.Message, len(items))
	for i, item := range items {
		results[i] = item.msg
	}
	return results, nil
}

func (s *Store) ClearSession(ctx context.Context, sessionID string) error {
	ids, err := s.rdb.SMembers(ctx, s.sessKey(sessionID)).Result()
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = s.msgKey(id)
		}
		s.rdb.Del(ctx, keys...)
	}
	return s.rdb.Del(ctx, s.sessKey(sessionID)).Err()
}

func (s *Store) Close() error {
	return s.rdb.Close()
}

// GetClient 获取底层 Redis 客户端（供 Retriever 使用）
func (s *Store) GetClient() *redis.Client {
	return s.rdb
}

// GetKeyPrefix 获取键前缀
func (s *Store) GetKeyPrefix() string {
	return s.config.KeyPrefix
}
