package gorm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ============================================================================
// GORMStore 纯存储实现
// ============================================================================

// GORMStore 基于 GORM 的消息持久化存储
// 负责 SQLite 持久化，包括嵌入生成和分块，但不管理向量索引
type GORMStore struct {
	db        *gorm.DB
	config    *Config
	embedding EmbeddingFunc
}

// NewGORMStore 创建 GORM 存储
func NewGORMStore(config *Config, embedding EmbeddingFunc) (*GORMStore, error) {
	if config == nil {
		config = DefaultConfig()
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

	return &GORMStore{
		db:        db,
		config:    config,
		embedding: embedding,
	}, nil
}

// GetDB 返回底层数据库连接（供 Retriever 使用）
func (s *GORMStore) GetDB() *gorm.DB {
	return s.db
}

// Save 保存消息（支持批量 + 嵌入生成）
// 每条消息使用递增时间戳，确保同一轮对话内的顺序正确
// 嵌入生成在事务外执行，避免网络 I/O 阻塞 SQLite 写锁
func (s *GORMStore) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	baseTime := time.Now().UnixMilli()

	type preparedMsg struct {
		model     *MessageModel
		chunks    []*EmbeddingChunk
		chunkVecs [][]float32
		msgVec    []float32
		needChunk bool
	}

	// Phase 1: 在事务外准备所有数据（包括 embedding 网络调用）
	prepared := make([]preparedMsg, len(msgs))
	for i, msg := range msgs {
		timestamp := baseTime + int64(i)
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

		// 序列化元数据
		metaData := make(map[string]any)
		if len(msg.ToolCalls) > 0 {
			metaData["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			metaData["tool_call_id"] = msg.ToolCallID
		}
		if msg.IsMultimodal() {
			metaData["multimodal"] = true
			metaData["image_count"] = msg.ImageCount()
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

		prepared[i].model = model

		// 嵌入生成（网络调用，在事务外执行）
		if !s.config.DisableVectorSearch && s.embedding != nil {
			embedText := content
			if msg.ReasoningContent != "" {
				embedText = msg.ReasoningContent + " " + embedText
			}

			estTokens := len([]rune(embedText)) / 2
			prepared[i].needChunk = s.config.ChunkSize > 0 && estTokens > s.config.ChunkSize

			if prepared[i].needChunk {
				textChunks := SplitText(embedText, s.config.ChunkSize, s.config.ChunkOverlap)
				prepared[i].chunks = make([]*EmbeddingChunk, len(textChunks))
				prepared[i].chunkVecs = make([][]float32, len(textChunks))
				for idx, chunkContent := range textChunks {
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
					prepared[i].chunks[idx] = chunkModel
					prepared[i].chunkVecs[idx] = vec
				}
			} else {
				vec, err := s.embedding(ctx, embedText)
				if err == nil && len(vec) > 0 {
					model.SetEmbedding(vec)
					prepared[i].msgVec = vec
				}
			}
		}
	}

	// Phase 2: 在事务内只做数据库写入（纯磁盘 I/O）
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, p := range prepared {
			if p.needChunk {
				for _, chunk := range p.chunks {
					if chunk == nil {
						continue
					}
					if err := tx.Create(chunk).Error; err != nil {
						return err
					}
				}
			}

			if err := tx.Create(p.model).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// GetSession 获取完整会话历史
func (s *GORMStore) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
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
func (s *GORMStore) GetSessionWithReasoning(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	return s.GetSession(ctx, sessionID)
}

// ClearSession 清空会话（硬删除）
func (s *GORMStore) ClearSession(ctx context.Context, sessionID string) error {
	if err := s.db.WithContext(ctx).Where("session_id = ?", sessionID).Delete(&EmbeddingChunk{}).Error; err != nil {
		return err
	}
	return s.db.WithContext(ctx).Where("session_id = ?", sessionID).Delete(&MessageModel{}).Error
}

// Close 关闭存储
func (s *GORMStore) Close() error {
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
func (s *GORMStore) SearchByRole(ctx context.Context, sessionID string, role schema.RoleType, limit int) ([]*schema.Message, error) {
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
func (s *GORMStore) SearchByTimeRange(ctx context.Context, sessionID string, start, end time.Time) ([]*schema.Message, error) {
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
func (s *GORMStore) GetSessionStats(ctx context.Context, sessionID string) (map[string]any, error) {
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
