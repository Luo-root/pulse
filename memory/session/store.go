package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Option 配置内存 store。
type Option func(*memStore)

// WithListPageSize 设置 List 的页大小；n <= 0 表示不分页（全量返回，
// next 为空）。默认不分页——内存实现全量返回天然不截断。
func WithListPageSize(n int) Option {
	return func(s *memStore) { s.pageSize = n }
}

// WithRegistry 使用自定义事件注册表（插件扩展事件类型时提供）；nil 用
// 内置最小族。
func WithRegistry(reg *Registry) Option {
	return func(s *memStore) {
		if reg != nil {
			s.reg = reg
		}
	}
}

// NewMemoryStore 创建 P2-A1 的内存会话 store：完整 §7.1 接口 + 单写者，
// 先把 event-sourced 语义测绿。持久化（JSONL + blobs + 文件锁）在 P2-A2。
func NewMemoryStore(opts ...Option) *memStore {
	s := &memStore{sessions: make(map[string]*memSession), reg: NewRegistry()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// memStore 是 SessionStore 的内存实现。进程内每个 SessionID 只有一个
// *memSession 实例（单写者的内存形态：不存在第二个 writer 对象）。
type memStore struct {
	mu       sync.Mutex
	sessions map[string]*memSession
	reg      *Registry
	pageSize int
}

// Create 实现 SessionStore。header 归一规则见接口注释；同 ID 已存在时
// 拒绝（并发 Create 同一 ID 恰好一个成功——内存单写者的确定性保证）。
func (s *memStore) Create(ctx context.Context, header SessionHeader) (Session, error) {
	if header.FormatVersion == 0 {
		header.FormatVersion = FormatVersion
	}
	if header.FormatVersion != FormatVersion && header.FormatVersion != CompactedVersion {
		return nil, fmt.Errorf("%w: store speaks v%d/v%d, header says v%d",
			ErrFormatVersion, FormatVersion, CompactedVersion, header.FormatVersion)
	}
	if header.SessionID == "" {
		header.SessionID = newID()
	}
	if header.CreatedAt.IsZero() {
		header.CreatedAt = time.Now()
	}
	sess := newMemSession(header, s.reg, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[header.SessionID] != nil {
		return nil, fmt.Errorf("%w: id %s", ErrSessionExists, header.SessionID)
	}
	s.sessions[header.SessionID] = sess
	return sess, nil
}

// Open 实现 SessionStore：内存实现里这就是冷恢复入口（没有独立 Recover
// 方法）。发现未闭合 turn/step 或 unpaired ToolCall 时合成闭合事件真实
// Append 写回日志再返回；已闭合则原样返回同一实例。
//
// 单写者：恢复临界区用 try-lock 互斥——第二写者立即得到 ErrWriterBusy，
// 不会出现两个 goroutine 同时补写（否则同一现场会被补出双份合成事件）。
// 正常顺序场景（宿主每回合 Open 一次）不受影响：前一次恢复完成后锁即释放。
func (s *memStore) Open(ctx context.Context, id string) (Session, error) {
	s.mu.Lock()
	sess := s.sessions[id]
	s.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("%w: id %s", ErrSessionNotFound, id)
	}
	return sess.reopen()
}

// List 实现 SessionStore：CreatedAt 降序 + SessionID tiebreak 稳定排序 +
// 游标分页，不静默截断。
func (s *memStore) List(ctx context.Context, filter SessionFilter) ([]SessionHeader, string, error) {
	s.mu.Lock()
	headers := make([]SessionHeader, 0, len(s.sessions))
	for _, sess := range s.sessions {
		headers = append(headers, sess.Header())
	}
	s.mu.Unlock()
	return sortAndPageHeaders(headers, filter, s.pageSize)
}

// sortAndPageHeaders 按 CreatedAt 降序 + SessionID tiebreak 稳定排序并按
// 游标分页（内存版与 JSONL 版共用）。After 是上一页末尾的 SessionID；
// pageSize <= 0 表示不分页全量返回。After 指向的会话不存在 →
// ErrCursorStale：静默从头会重复返回，不做。
func sortAndPageHeaders(headers []SessionHeader, filter SessionFilter, pageSize int) ([]SessionHeader, string, error) {
	sort.Slice(headers, func(i, j int) bool {
		if !headers[i].CreatedAt.Equal(headers[j].CreatedAt) {
			return headers[i].CreatedAt.After(headers[j].CreatedAt)
		}
		return headers[i].SessionID < headers[j].SessionID
	})
	start := 0
	if filter.After != "" {
		found := false
		for i, h := range headers {
			if h.SessionID == filter.After {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("%w: session %s not found; reset pagination", ErrCursorStale, filter.After)
		}
	}
	if start >= len(headers) {
		return nil, "", nil
	}
	if pageSize <= 0 || len(headers)-start <= pageSize {
		return headers[start:], "", nil
	}
	page := headers[start : start+pageSize]
	return page, page[len(page)-1].SessionID, nil
}

// Delete 实现 SessionStore。会话从 store 移除并标记：Open → NotFound，
// 已持有的实例继续 Append/Flush → ErrDeleted（fail closed，不静默写进已
// 丢弃的日志）。deleted 置位持 session 写锁，与 Append/Flush 的锁内检查
// 构成 happens-before：不存在「已删会话仍写入成功」的时序。
func (s *memStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("%w: id %s", ErrSessionNotFound, id)
	}
	sess.mu.Lock()
	sess.deleted.Store(true)
	sess.mu.Unlock()
	return nil
}

// reopen 是 Open 的冷恢复临界区：try-lock → 扫描未闭合现场 → 合成事件
// 写回日志。全程持有 session 写锁，保证「扫描 → 补写」原子。
func (s *memSession) reopen() (*memSession, error) {
	if !s.recovering.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("%w: id %s", ErrWriterBusy, s.hdr.SessionID)
	}
	defer s.recovering.Store(false)

	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := scanIncomplete(s.events, s.reg)
	if err != nil {
		return nil, err // 未知 required 等 fail closed：拒绝 Open
	}
	if len(st.pendingCalls) == 0 && !st.openStep && !st.openTurn {
		return s, nil
	}
	for _, draft := range synthDrafts(st) {
		s.appendLocked(draft, false)
	}
	return s, nil
}

// newID 生成会话 ID（crypto/rand 16 字节 hex）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见（系统熵源不可用）；不静默降级为时间戳。
		panic(fmt.Sprintf("session: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// mustJSON 把 payload 编码为 json.RawMessage；载荷类型均由本包控制且可
// 无损 JSON，编码失败属于程序错误而非运行时数据问题。
func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("session: marshal payload %T: %v", v, err))
	}
	return data
}
