//go:build !plan9 && !js

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	// CGO-free SQLite 驱动（modernc 转译，FTS5 默认启用）。
	_ "modernc.org/sqlite"
)

// schemaVersion 是 SQLite 存储的 schema 版本（PRAGMA user_version）。
// 版本不兼容拒绝加载，不猜测迁移（与 session header 同口径）。
const schemaVersion = 1

// nsSep 是 namespace 元素的连接分隔符（unit separator）：join 后做前缀
// 匹配，「ns + nsSep + '%'」的 LIKE 不会跨元素边界（"tenant:a" 不会前缀
// 命中 "tenant:ab"）。
const nsSep = "\x1f"

// SQLiteStore 是 MemoryStore 的 SQLite 实现（CGO-free）：与内存实现同一
// 接口契约与错误语义。plan9/js 平台由构建约束排除——store 主包不被锁死。
//
// 并发取舍：MaxOpenConns(1) + busy_timeout——SQLite 写锁语义下最稳的
// 正确性（本包单机场景对吞吐不敏感）；WAL 等调优留给宿主 DSN 自定义。
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore 打开（或创建）SQLite item store：建表（items + FTS5 外部
// 内容表 + 同步触发器 + audit）、schema 版本校验。dsn 形如
// "file:/path/to/memory.db"（modernc 驱动）。
func NewSQLiteStore(ctx context.Context, dsn string) (*SQLiteStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("%w: empty dsn", ErrInvalidQuery)
	}
	db, err := sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite 写锁：单连接串行化，正确性优先
	s := &SQLiteStore{db: db}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// init 建表并校验 schema 版本。
func (s *SQLiteStore) init(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("%w: file schema v%d, store speaks v%d（不猜测迁移）", ErrCorruptSchema, version, schemaVersion)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_items (
			id          TEXT PRIMARY KEY,
			ns_key      TEXT NOT NULL,
			namespace   TEXT NOT NULL,
			kind        TEXT NOT NULL,
			content     TEXT NOT NULL,
			structured  TEXT,
			status      TEXT NOT NULL,
			confidence  REAL NOT NULL,
			source_refs TEXT NOT NULL,
			taint       TEXT NOT NULL,
			valid_from  TEXT,
			valid_until TEXT,
			known_at    TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL,
			revision    INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_items_ns ON memory_items(ns_key)`,
		`CREATE INDEX IF NOT EXISTS idx_items_status ON memory_items(status)`,
		// FTS5 外部内容表：随 items 增删改由触发器同步，不复制数据。
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_items_fts USING fts5(
			content, content='memory_items', content_rowid='rowid'
		)`,
		`CREATE TRIGGER IF NOT EXISTS items_fts_ai AFTER INSERT ON memory_items BEGIN
			INSERT INTO memory_items_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS items_fts_ad AFTER DELETE ON memory_items BEGIN
			INSERT INTO memory_items_fts(memory_items_fts, rowid, content)
			VALUES ('delete', old.rowid, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS items_fts_au AFTER UPDATE ON memory_items BEGIN
			INSERT INTO memory_items_fts(memory_items_fts, rowid, content)
			VALUES ('delete', old.rowid, old.content);
			INSERT INTO memory_items_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		`CREATE TABLE IF NOT EXISTS memory_audit (
			seq     INTEGER PRIMARY KEY AUTOINCREMENT,
			at      TEXT NOT NULL,
			action  TEXT NOT NULL,
			item_id TEXT NOT NULL,
			reason  TEXT NOT NULL DEFAULT '',
			next_id TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: init schema: %w", err)
		}
	}
	if version < schemaVersion {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("store: set schema version: %w", err)
		}
	}
	return nil
}

// Close 关闭底层连接。
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Put 实现 MemoryStore：新建（ExpectedRevision=0）或 CAS 更新。
func (s *SQLiteStore) Put(ctx context.Context, item MemoryItem, opts PutMemoryOptions) (MemoryItem, error) {
	if err := item.validate(); err != nil {
		return MemoryItem{}, err
	}
	now := time.Now().UTC()
	if opts.ExpectedRevision == 0 {
		if _, err := s.Get(ctx, item.Namespace, item.ID); err == nil {
			return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemExists, item.ID)
		}
		item.Revision = 1
		item.KnownAt, item.CreatedAt, item.UpdatedAt = now, now, now
		if err := s.writeItem(ctx, item, 0, now); err != nil {
			return MemoryItem{}, err
		}
		return s.Get(ctx, item.Namespace, item.ID)
	}
	cur, err := s.Get(ctx, item.Namespace, item.ID)
	if err != nil {
		return MemoryItem{}, err // ErrItemNotFound（含 namespace 不互见）
	}
	if cur.Revision != opts.ExpectedRevision {
		return MemoryItem{}, fmt.Errorf("%w: id %s expected rev %d, current %d",
			ErrRevisionConflict, item.ID, opts.ExpectedRevision, cur.Revision)
	}
	// 状态迁移只走 Supersede/Revoke（各有审计与替代链）：Put 更新禁止
	// 改变 Status——否则 active→pending 绕过 P2-D 的 taint gate、active→
	// superseded 绕过替代链（§10.2 追溯性）。
	if cur.Status != item.Status {
		return MemoryItem{}, fmt.Errorf("%w: id %s %s → %s（用 Supersede/Revoke）",
			ErrStatusTransition, item.ID, cur.Status, item.Status)
	}
	item.Revision = cur.Revision + 1
	item.CreatedAt, item.KnownAt = cur.CreatedAt, cur.KnownAt
	item.UpdatedAt = now
	if err := s.writeItem(ctx, item, cur.Revision, now); err != nil {
		return MemoryItem{}, err
	}
	return s.Get(ctx, item.Namespace, item.ID)
}

// Get 实现 MemoryStore。
func (s *SQLiteStore) Get(ctx context.Context, ns []string, id string) (MemoryItem, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, ns_key, namespace, kind, content, structured, status, confidence,
		        source_refs, taint, valid_from, valid_until, known_at, created_at, updated_at, revision
		 FROM memory_items WHERE id = ?`, id)
	it, err := scanItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemNotFound, id)
		}
		return MemoryItem{}, fmt.Errorf("store: get %s: %w", id, err)
	}
	if !namespaceVisible(ns, it.Namespace) {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemNotFound, id)
	}
	return it, nil
}

// Search 实现 MemoryStore：与内存版同语义（子串 LIKE 转义、状态过滤、
// UpdatedAt 降序 + ID tiebreak、Limit 硬上限）；FTS token 检索走 SearchFTS。
func (s *SQLiteStore) Search(ctx context.Context, q MemoryQuery) ([]MemoryHit, error) {
	if q.Limit < 0 {
		return nil, fmt.Errorf("%w: negative limit", ErrInvalidQuery)
	}
	where := []string{"1 = 1"}
	args := []any{}
	if len(q.Namespace) > 0 {
		prefix := strings.Join(q.Namespace, nsSep)
		where = append(where, "(ns_key = ? OR ns_key LIKE ? ESCAPE '\\')")
		args = append(args, prefix, escapeLike(prefix+nsSep)+"%")
	}
	if len(q.Kinds) > 0 {
		ph := make([]string, 0, len(q.Kinds))
		for _, k := range q.Kinds {
			ph = append(ph, "?")
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if !q.IncludeInactive {
		where = append(where, "status = ?")
		args = append(args, string(StatusActive))
	}
	if needle := strings.TrimSpace(q.Query); needle != "" {
		where = append(where, "instr(lower(content), ?) > 0")
		args = append(args, strings.ToLower(needle))
	}
	sqlText := `SELECT id, ns_key, namespace, kind, content, structured, status, confidence,
	       source_refs, taint, valid_from, valid_until, known_at, created_at, updated_at, revision
	FROM memory_items WHERE ` + strings.Join(where, " AND ") + ` ORDER BY updated_at DESC, id ASC`
	if q.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search: %w", err)
	}
	defer rows.Close()
	hits := []MemoryHit{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, MemoryHit{Item: it})
	}
	return hits, rows.Err()
}

// SearchFTS 是 SQLite 实现特有的 FTS5 检索（token 前缀语义，C3 Assembler
// 的召回入口）：match 形如 "toml config"，自动转成 `"toml"* AND "config"*`。
// namespace 过滤与 Search 同口径。本方法不在 §7.1 接口面（类型断言使用）。
func (s *SQLiteStore) SearchFTS(ctx context.Context, ns []string, match string, limit int) ([]MemoryHit, error) {
	terms := strings.Fields(strings.TrimSpace(match))
	if len(terms) == 0 {
		return nil, fmt.Errorf("%w: empty fts match", ErrInvalidQuery)
	}
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		if strings.Contains(t, "\"") {
			return nil, fmt.Errorf("%w: fts term %q contains a quote", ErrInvalidQuery, t)
		}
		quoted = append(quoted, `"`+t+`"*`)
	}
	matchExpr := strings.Join(quoted, " AND ")
	sqlText := `SELECT i.id, i.ns_key, i.namespace, i.kind, i.content, i.structured, i.status,
	       i.confidence, i.source_refs, i.taint, i.valid_from, i.valid_until, i.known_at,
	       i.created_at, i.updated_at, i.revision
	FROM memory_items_fts f JOIN memory_items i ON i.rowid = f.rowid
	WHERE memory_items_fts MATCH ?`
	args := []any{matchExpr}
	if len(ns) > 0 {
		prefix := strings.Join(ns, nsSep)
		sqlText += ` AND (i.ns_key = ? OR i.ns_key LIKE ? ESCAPE '\')`
		args = append(args, prefix, escapeLike(prefix+nsSep)+"%")
	}
	sqlText += ` ORDER BY rank, i.id ASC`
	if limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: fts search: %w", err)
	}
	defer rows.Close()
	hits := []MemoryHit{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, MemoryHit{Item: it})
	}
	return hits, rows.Err()
}

// Supersede 实现 MemoryStore：旧 item Status→Superseded 与 next 入库在
// 同一事务（BEGIN IMMEDIATE）——替代链不断。
func (s *SQLiteStore) Supersede(ctx context.Context, oldID string, next MemoryItem) (MemoryItem, error) {
	if next.ID == oldID {
		return MemoryItem{}, fmt.Errorf("%w: %s", ErrSupersedeSelf, oldID)
	}
	if err := next.validate(); err != nil {
		return MemoryItem{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryItem{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	txq := txQueries{tx: tx}
	old, err := txq.get(ctx, oldID)
	if err != nil {
		return MemoryItem{}, err
	}
	if old.Status == StatusRevoked {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrSupersedeRevoked, oldID)
	}
	if _, getErr := txq.get(ctx, next.ID); getErr == nil {
		return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemExists, next.ID)
	}
	now := time.Now().UTC()
	next.Revision, next.KnownAt, next.CreatedAt, next.UpdatedAt = 1, now, now, now
	if err := txq.write(ctx, next, 0, now); err != nil {
		return MemoryItem{}, err
	}
	superseded := old
	superseded.Status = StatusSuperseded
	superseded.UpdatedAt = now
	superseded.Revision++
	if err := txq.write(ctx, superseded, old.Revision, now); err != nil {
		return MemoryItem{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memory_audit (at, action, item_id, reason, next_id) VALUES (?,?,?,?,?)`,
		now.Format(time.RFC3339Nano), "supersede", oldID, "", next.ID); err != nil {
		return MemoryItem{}, fmt.Errorf("store: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MemoryItem{}, fmt.Errorf("store: commit supersede: %w", err)
	}
	tx = nil
	return next, nil
}

// Revoke 实现 MemoryStore：Status→Revoked + 审计落表；Revoked 幂等；
// Superseded → ErrRevokeSuperseded。
func (s *SQLiteStore) Revoke(ctx context.Context, id string, reason string) error {
	cur, err := s.Get(ctx, nil, id)
	if err != nil {
		return err
	}
	switch cur.Status {
	case StatusRevoked:
		return nil // 幂等
	case StatusSuperseded:
		return fmt.Errorf("%w: id %s", ErrRevokeSuperseded, id)
	}
	now := time.Now().UTC()
	revoked := cur
	revoked.Status = StatusRevoked
	revoked.UpdatedAt = now
	revoked.Revision++
	if err := s.writeItem(ctx, revoked, cur.Revision, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_audit (at, action, item_id, reason, next_id) VALUES (?,?,?,?,?)`,
		now.Format(time.RFC3339Nano), "revoke", id, reason, ""); err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

// AuditLog 返回审计记录（实现特有；reason 落 SQLite audit 表）。
func (s *SQLiteStore) AuditLog() ([]AuditEntry, error) {
	rows, err := s.db.Query(`SELECT seq, at, action, item_id, reason, next_id FROM memory_audit ORDER BY seq ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: audit log: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.Seq, &at, &e.Action, &e.ItemID, &e.Reason, &e.NextID); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- 内部 ----

// writeItem 写入/更新一条 item（CAS：expected>0 时 WHERE 带 revision 校验，
// RowsAffected==0 → 冲突）。
func (s *SQLiteStore) writeItem(ctx context.Context, item MemoryItem, expected uint64, now time.Time) error {
	txq := txQueries{tx: s.db}
	return txq.write(ctx, item, expected, now)
}

// txQueries 把 item 读写绑定到一个执行体（*sql.DB 或 *sql.Tx），Supersede
// 的两写复用同一套语句。
type txQueries struct {
	tx interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
}

func (t txQueries) get(ctx context.Context, id string) (MemoryItem, error) {
	row := t.tx.QueryRowContext(ctx,
		`SELECT id, ns_key, namespace, kind, content, structured, status, confidence,
		        source_refs, taint, valid_from, valid_until, known_at, created_at, updated_at, revision
		 FROM memory_items WHERE id = ?`, id)
	it, err := scanItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryItem{}, fmt.Errorf("%w: id %s", ErrItemNotFound, id)
		}
		return MemoryItem{}, fmt.Errorf("store: get %s: %w", id, err)
	}
	return it, nil
}

func (t txQueries) write(ctx context.Context, item MemoryItem, expected uint64, now time.Time) error {
	nsKey := strings.Join(item.Namespace, nsSep)
	nsJSON, _ := json.Marshal(item.Namespace)
	refsJSON, err := json.Marshal(item.SourceRefs)
	if err != nil {
		return fmt.Errorf("%w: source refs: %v", ErrInvalidItem, err)
	}
	structured := any(nil)
	if len(item.Structured) > 0 {
		structured = string(item.Structured)
	}
	validFrom := any(nil)
	if !item.ValidFrom.IsZero() {
		validFrom = item.ValidFrom.Format(time.RFC3339Nano)
	}
	validUntil := any(nil)
	if item.ValidUntil != nil {
		validUntil = item.ValidUntil.Format(time.RFC3339Nano)
	}
	var res sql.Result
	if expected == 0 {
		res, err = t.tx.ExecContext(ctx, `INSERT INTO memory_items
			(id, ns_key, namespace, kind, content, structured, status, confidence,
			 source_refs, taint, valid_from, valid_until, known_at, created_at, updated_at, revision)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.ID, nsKey, string(nsJSON), string(item.Kind), item.Content, structured,
			string(item.Status), item.Confidence, string(refsJSON), string(item.Taint),
			validFrom, validUntil,
			item.KnownAt.Format(time.RFC3339Nano), item.CreatedAt.Format(time.RFC3339Nano),
			item.UpdatedAt.Format(time.RFC3339Nano), item.Revision)
	} else {
		res, err = t.tx.ExecContext(ctx, `UPDATE memory_items SET
			ns_key=?, namespace=?, kind=?, content=?, structured=?, status=?, confidence=?,
			source_refs=?, taint=?, valid_from=?, valid_until=?, known_at=?, created_at=?, updated_at=?, revision=?
			WHERE id=? AND revision=?`,
			nsKey, string(nsJSON), string(item.Kind), item.Content, structured,
			string(item.Status), item.Confidence, string(refsJSON), string(item.Taint),
			validFrom, validUntil,
			item.KnownAt.Format(time.RFC3339Nano), item.CreatedAt.Format(time.RFC3339Nano),
			item.UpdatedAt.Format(time.RFC3339Nano), item.Revision,
			item.ID, expected)
	}
	if err != nil {
		return fmt.Errorf("store: write %s: %w", item.ID, err)
	}
	if expected > 0 {
		if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
			return fmt.Errorf("%w: id %s expected rev %d", ErrRevisionConflict, item.ID, expected)
		}
	}
	return nil
}

// scanItem 从一行读出 MemoryItem（namespace/source_refs 为 JSON 列；列序
// 与 SELECT 一致：ns_key 丢弃、taint 回填）。
func scanItem(row interface {
	Scan(dest ...any) error
}) (MemoryItem, error) {
	var it MemoryItem
	var nsKey, nsJSON, refsJSON, taint, kind, status string
	var structured, validFrom, validUntil sql.NullString
	var knownAt, createdAt, updatedAt string
	var confidence float64
	if err := row.Scan(&it.ID, &nsKey, &nsJSON, &kind, &it.Content, &structured,
		&status, &confidence, &refsJSON, &taint, &validFrom, &validUntil,
		&knownAt, &createdAt, &updatedAt, &it.Revision); err != nil {
		return MemoryItem{}, err
	}
	it.Kind, it.Status, it.Taint = MemoryKind(kind), MemoryStatus(status), TaintLevel(taint)
	it.Confidence = float32(confidence)
	if err := json.Unmarshal([]byte(nsJSON), &it.Namespace); err != nil {
		return MemoryItem{}, fmt.Errorf("%w: namespace json: %v", ErrCorruptSchema, err)
	}
	if err := json.Unmarshal([]byte(refsJSON), &it.SourceRefs); err != nil {
		return MemoryItem{}, fmt.Errorf("%w: source refs json: %v", ErrCorruptSchema, err)
	}
	if structured.Valid {
		it.Structured = json.RawMessage(structured.String)
	}
	if t, err := time.Parse(time.RFC3339Nano, knownAt); err == nil {
		it.KnownAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		it.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		it.UpdatedAt = t
	}
	if validFrom.Valid {
		if t, err := time.Parse(time.RFC3339Nano, validFrom.String); err == nil {
			it.ValidFrom = t
		}
	}
	if validUntil.Valid {
		if t, err := time.Parse(time.RFC3339Nano, validUntil.String); err == nil {
			it.ValidUntil = &t
		}
	}
	return it, nil
}

// escapeLike 转义 LIKE 通配符（配合 ESCAPE '\'）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}
