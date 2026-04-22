package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	_ "github.com/glebarez/sqlite"
)

type LocalStore struct {
	db *sql.DB
}

// NewLocalStore 创建本地 SQLite 存储
func NewLocalStore(dbPath string) (*LocalStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// 创建表
	schema := `
	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		embedding BLOB,
		timestamp INTEGER NOT NULL,
		metadata TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_session ON messages(session_id, timestamp);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	return &LocalStore{db: db}, nil
}

func (s *LocalStore) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO messages (id, session_id, role, content, timestamp, metadata)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, msg := range msgs {
		id := fmt.Sprintf("%s_%d", sessionID, time.Now().UnixNano())
		metadata, _ := json.Marshal(msg.ToolCalls)

		_, err := stmt.ExecContext(ctx,
			id,
			sessionID,
			string(msg.Role),
			msg.Content,
			time.Now().Unix(),
			string(metadata),
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *LocalStore) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	// 简单实现：按关键词匹配 + 时间倒序
	// 后续接入 embedding 后用向量相似度
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content, timestamp FROM messages
		WHERE session_id = ? AND content LIKE ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, sessionID, "%"+query+"%", topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*schema.Message
	for rows.Next() {
		var role, content string
		var timestamp int64
		if err := rows.Scan(&role, &content, &timestamp); err != nil {
			continue
		}

		results = append(results, &schema.Message{
			Role:    schema.RoleType(role),
			Content: content,
		})
	}

	return results, nil
}

func (s *LocalStore) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content FROM messages
		WHERE session_id = ?
		ORDER BY timestamp ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*schema.Message
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			continue
		}
		results = append(results, &schema.Message{
			Role:    schema.RoleType(role),
			Content: content,
		})
	}

	return results, nil
}

func (s *LocalStore) ClearSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, sessionID)
	return err
}

func (s *LocalStore) Close() error {
	return s.db.Close()
}
