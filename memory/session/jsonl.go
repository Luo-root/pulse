package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// JSONL 落盘布局（设计 §12 P2-A2）：
//
//	{root}/{sessionID}/header.json    会话头（FormatVersion 不兼容拒绝加载）
//	{root}/{sessionID}/events.jsonl   事件日志，每行一条 EventEnvelope，append-only
//	{root}/{sessionID}/blobs/{sha256} 超限内联字节（内容寻址）
//	{root}/{sessionID}/lock           文件锁（O_EXCL 原子创建）
//
// JSONL 为明文：文件即密钥面、路径宿主拥有；P2-A 不做加密。
// 删除会话 = 删整个目录（会话不是 MemoryItem，不适用 Supersede/Revoke）。

// 默认 stale 锁阈值：持有者进程崩溃后留下的锁文件，超过该时长可被抢占。
const defaultLockStale = time.Hour

// jsonlOption 配置 JSONLStore。
type jsonlOption func(*JSONLStore)

// JSONLStale 设置 stale 锁阈值。
func JSONLStale(d time.Duration) jsonlOption {
	return func(s *JSONLStore) {
		if d > 0 {
			s.lockStale = d
		}
	}
}

// JSONLPageSize 设置 List 页大小；n <= 0 不分页（全量返回）。
func JSONLPageSize(n int) jsonlOption {
	return func(s *JSONLStore) { s.pageSize = n }
}

// JSONLRegistry 使用自定义事件注册表；nil 用内置最小族。
func JSONLRegistry(reg *Registry) jsonlOption {
	return func(s *JSONLStore) {
		if reg != nil {
			s.reg = reg
		}
	}
}

// NewJSONLStore 创建 JSONL 会话 store：root 不存在则创建。
func NewJSONLStore(root string, opts ...jsonlOption) (*JSONLStore, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: empty root", ErrPayloadInvalid)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("session: create root %s: %w", root, err)
	}
	s := &JSONLStore{
		root:      root,
		reg:       NewRegistry(),
		lockStale: defaultLockStale,
		open:      make(map[string]*jsonlSession),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// JSONLStore 是 SessionStore 的 JSONL 持久实现。同一进程内每个 SessionID
// 只有一个打开的 *jsonlSession（缓存 + 文件锁双重单写者）。
type JSONLStore struct {
	mu        sync.Mutex
	root      string
	reg       *Registry
	pageSize  int
	lockStale time.Duration
	open      map[string]*jsonlSession
}

// jsonlSession 是 Session 的 JSONL 实现：内存态复用 memSession（同一把
// 写锁、同一套校验链），每条 Append 同步落盘；Flush 才 fsync。
type jsonlSession struct {
	*memSession
	dir     string
	f       *os.File
	store   *JSONLStore
	release func() // 文件锁释放；Close 后置 nil（幂等）
}

func (s *jsonlSession) blobsDir() string { return filepath.Join(s.dir, "blobs") }

// Create 实现 SessionStore：header 归一规则与内存版一致（SessionID 空 →
// 生成；CreatedAt 零值取 now；FormatVersion 零值取本包版本，不兼容拒绝）。
// 同 ID 目录已存在 → ErrSessionExists（拒绝第二写者）。
func (s *JSONLStore) Create(ctx context.Context, header SessionHeader) (Session, error) {
	if header.FormatVersion == 0 {
		header.FormatVersion = FormatVersion
	}
	if header.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: store speaks v%d, header says v%d", ErrFormatVersion, FormatVersion, header.FormatVersion)
	}
	if header.SessionID == "" {
		header.SessionID = newID()
	}
	if header.CreatedAt.IsZero() {
		header.CreatedAt = time.Now()
	}
	return s.createSession(header, nil)
}

// createSession 是 Create 与 Fork 的共同落盘路径：建目录、抢文件锁、写
// header 与 seed（Fork 的继承事件）行，返回持锁会话。
func (s *JSONLStore) createSession(header SessionHeader, seed []EventEnvelope) (*jsonlSession, error) {
	if !validSessionID(header.SessionID) {
		return nil, fmt.Errorf("%w: id %q must match [A-Za-z0-9_-]{1,128}", ErrInvalidSessionID, header.SessionID)
	}
	dir := filepath.Join(s.root, header.SessionID)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("%w: id %s", ErrSessionExists, header.SessionID)
		}
		return nil, fmt.Errorf("session: create dir: %w", err)
	}
	release, err := acquireSessionLock(filepath.Join(dir, "lock"), s.lockStale)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	fail := func(err error) (*jsonlSession, error) {
		release()
		os.RemoveAll(dir)
		return nil, err
	}
	headerPath := filepath.Join(dir, "header.json")
	hdrJSON, err := json.Marshal(header)
	if err != nil {
		return fail(fmt.Errorf("session: marshal header: %w", err))
	}
	if err := os.WriteFile(headerPath, hdrJSON, 0o644); err != nil {
		return fail(fmt.Errorf("session: write header: %w", err))
	}
	eventsPath := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fail(fmt.Errorf("session: create events.jsonl: %w", err))
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		f.Close()
		return fail(fmt.Errorf("session: create blobs dir: %w", err))
	}
	sess := &jsonlSession{
		memSession: &memSession{hdr: header, reg: s.reg},
		dir:        dir,
		f:          f,
		store:      s,
		release:    release,
	}
	// Fork seed：逐条落盘（保持原 Seq/Time；文件存引用形态，内存态持有
	// 还原形态）。
	for _, env := range seed {
		if isMessageEvent(env.Type) {
			encoded, err := encodeBlobs(env.Data, sess.blobsDir())
			if err != nil {
				f.Close()
				return fail(err)
			}
			env.Data = encoded
		}
		if err := sess.writeLineLocked(env); err != nil {
			f.Close()
			return fail(err)
		}
		sess.appendEnvelopeLocked(env)
	}
	s.mu.Lock()
	s.open[header.SessionID] = sess
	s.mu.Unlock()
	return sess, nil
}

// Open 实现 SessionStore：抢文件锁 → 读 header（版本校验）→ 载入事件日志
// （撕裂尾截断、blob 还原、seq 连续性校验）→ 冷恢复（合成事件写回日志）→
// 返回。同一进程重复 Open 返回缓存实例（内存互斥接手）；跨进程并发
// Open → ErrWriterBusy（持有者进程崩溃后按 stale 阈值可抢占）。
func (s *JSONLStore) Open(ctx context.Context, id string) (Session, error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("%w: id %q", ErrInvalidSessionID, id)
	}
	s.mu.Lock()
	if sess := s.open[id]; sess != nil {
		s.mu.Unlock()
		return sess, nil
	}
	s.mu.Unlock()

	dir := filepath.Join(s.root, id)
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("%w: id %s", ErrSessionNotFound, id)
	}
	release, err := acquireSessionLock(filepath.Join(dir, "lock"), s.lockStale)
	if err != nil {
		return nil, err
	}
	sess, err := s.loadOpened(dir, release)
	if err != nil {
		release()
		return nil, err
	}
	s.mu.Lock()
	s.open[id] = sess
	s.mu.Unlock()
	return sess, nil
}

// loadOpened 完成锁持有后的加载与冷恢复。任何失败都释放锁。
func (s *JSONLStore) loadOpened(dir string, release func()) (*jsonlSession, error) {
	fail := func(err error) (*jsonlSession, error) {
		release()
		return nil, err
	}
	hdrData, err := os.ReadFile(filepath.Join(dir, "header.json"))
	if err != nil {
		return fail(fmt.Errorf("session: read header: %w", err))
	}
	var header SessionHeader
	if err := json.Unmarshal(hdrData, &header); err != nil {
		return fail(fmt.Errorf("%w: header.json: %v", ErrCorruptLog, err))
	}
	if header.FormatVersion != FormatVersion {
		return fail(fmt.Errorf("%w: file v%d, store v%d（不猜测迁移）", ErrFormatVersion, header.FormatVersion, FormatVersion))
	}
	eventsPath := filepath.Join(dir, "events.jsonl")
	envs, truncOffset, err := loadEvents(eventsPath)
	if err != nil {
		return fail(err)
	}
	if truncOffset >= 0 {
		if err := os.Truncate(eventsPath, truncOffset); err != nil {
			return fail(fmt.Errorf("session: truncate torn tail: %w", err))
		}
	}
	// blob 还原：内存态持有还原形态（fold 出完整 Part；引用缺失 fail closed）。
	for i := range envs {
		if !isMessageEvent(envs[i].Type) {
			continue
		}
		restored, err := decodeBlobs(envs[i].Data, filepath.Join(dir, "blobs"))
		if err != nil {
			return fail(err)
		}
		envs[i].Data = restored
	}
	sess := &jsonlSession{
		memSession: &memSession{hdr: header, reg: s.reg},
		dir:        dir,
		store:      s,
		release:    release,
	}
	for _, env := range envs {
		sess.appendEnvelopeLocked(env)
	}
	// 冷恢复（与 A1 同口径）：扫描未闭合现场，合成事件真实写回日志。
	sess.mu.Lock()
	defer sess.mu.Unlock()
	st, err := scanIncomplete(sess.events, s.reg)
	if err != nil {
		return fail(err) // 未知 required 等 fail closed：拒绝 Open
	}
	f, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fail(fmt.Errorf("session: open events.jsonl for append: %w", err))
	}
	sess.f = f
	for _, draft := range synthDrafts(st) {
		if _, err := sess.appendSyntheticLocked(draft); err != nil {
			f.Close()
			return fail(err)
		}
	}
	return sess, nil
}

// List 实现 SessionStore：扫描各会话的 header.json，CreatedAt 降序 +
// SessionID tiebreak + 游标分页。无法解析的目录（并发创建中/无关目录）
// 跳过——注意：header.json 损坏的会话会从列表消失，宿主在该会话上 Open
// 时才会收到 ErrCorruptLog；游标失效 → ErrCursorStale。
func (s *JSONLStore) List(ctx context.Context, filter SessionFilter) ([]SessionHeader, string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, "", fmt.Errorf("session: read root: %w", err)
	}
	headers := make([]SessionHeader, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, e.Name(), "header.json"))
		if err != nil {
			continue
		}
		var h SessionHeader
		if json.Unmarshal(data, &h) != nil || h.SessionID == "" {
			continue
		}
		headers = append(headers, h)
	}
	return sortAndPageHeaders(headers, filter, s.pageSize)
}

// Delete 实现 SessionStore：删整个会话目录。已打开实例先置 deleted（锁内）
// 并释放文件句柄与锁，阻止继续写入。跨进程：单纯句柄写入在 Unix/Windows
// 都会静默成功，因此 writeLineLocked 每次 append 前校验会话目录仍在——
// 删除后对方进程的 Append 收到 ErrDeleted（真 fail closed），不存在
// 「报成功但数据进 void」。
func (s *JSONLStore) Delete(ctx context.Context, id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("%w: id %q", ErrInvalidSessionID, id)
	}
	s.mu.Lock()
	sess := s.open[id]
	delete(s.open, id)
	s.mu.Unlock()
	dir := filepath.Join(s.root, id)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%w: id %s", ErrSessionNotFound, id)
	}
	if sess != nil {
		sess.mu.Lock()
		sess.deleted.Store(true)
		sess.mu.Unlock()
		sess.closeHandles()
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("session: remove session dir: %w", err)
	}
	return nil
}

// Append 实现 Session：校验链与内存版同源（prepareAppend），通过后先
// 落盘（blob 引用替换）再更新内存态——文件写失败时内存不留半条。
func (s *jsonlSession) Append(ctx context.Context, draft EventDraft) (EventEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ignorable, err := s.prepareAppend(draft)
	if err != nil {
		return EventEnvelope{}, err
	}
	env, err := s.buildEnvelopeLocked(draft.Type, draft.Data, ignorable, draft.Surface)
	if err != nil {
		return EventEnvelope{}, err
	}
	if err := s.writeLineLocked(env); err != nil {
		return EventEnvelope{}, err
	}
	return s.appendEnvelopeLocked(env), nil
}

// Flush 实现 Session：把已写入事件刷到磁盘（f.Sync）。崩溃只保证 Flush
// 点之前；内存版的「成功空操作」语义在这里兑现为真耐久。Flush 兼作文件
// 锁心跳（touch 锁文件 mtime）——长命会话只要定期 Flush 就不会被 stale
// 抢占；从不 Flush 的会话持锁超过阈值可能被另一进程接管，宿主须知。
func (s *jsonlSession) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted.Load() {
		return ErrDeleted
	}
	if s.f == nil {
		return ErrSessionClosed
	}
	now := time.Now()
	// best-effort：touch 失败（如锁文件已被抢占者删除）不阻塞本次 Sync；
	// 后续 append 会在 writeLineLocked 的目录校验处 fail closed。
	_ = os.Chtimes(filepath.Join(s.dir, "lock"), now, now)
	return s.f.Sync()
}

// Fork 实现 Session：切点校验与内存版同口径（组中间拒绝），子会话连同
// seed 事件一起落盘（引用形态），血缘写进子 header。
func (s *jsonlSession) Fork(ctx context.Context, atSeq uint64) (Session, error) {
	s.mu.Lock()
	if s.deleted.Load() {
		s.mu.Unlock()
		return nil, ErrDeleted
	}
	if atSeq == 0 || atSeq > s.seq {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: atSeq=%d max=%d", ErrForkBadAt, atSeq, s.seq)
	}
	seed := make([]EventEnvelope, atSeq)
	copy(seed, s.events[:atSeq])
	s.mu.Unlock()

	// 组边界校验在锁外做（纯扫描）：pending 非空即切在组内。
	pending, err := pendingToolCalls(seed, s.reg)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%w: atSeq=%d unpaired=%v", ErrForkSplitToolGroup, atSeq, pending)
	}
	hdr := s.hdr // header 构造后不可变，可无锁读
	hdr.SessionID = newID()
	hdr.CreatedAt = time.Now()
	hdr.ParentSessionID = s.hdr.SessionID
	hdr.SeedLength = atSeq
	hdr.DelegationDepth++
	return s.store.createSession(hdr, seed)
}

// Close 释放文件锁并关闭日志句柄（Session 接口之外，文件实现特有的
// 资源释放：调用方对返回的 Session 做类型断言）。正常路径用完即 Close，
// 避免依赖 stale 超时；崩溃残留锁由 stale 阈值抢占。幂等。
func (s *jsonlSession) Close() error {
	s.store.mu.Lock()
	delete(s.store.open, s.hdr.SessionID)
	s.store.mu.Unlock()
	s.closeHandles()
	return nil
}

func (s *jsonlSession) closeHandles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

// appendSyntheticLocked 是冷恢复路径的合成写：构造信封 + 落盘 + 内存追加，
// 全程持有 s.mu（loadOpened 持锁调用）。data 为还原形态（合成 payload
// 无内联字节，encodeBlobs 恒为原样）。
func (s *jsonlSession) appendSyntheticLocked(draft EventDraft) (EventEnvelope, error) {
	env, err := s.buildEnvelopeLocked(draft.Type, draft.Data, false, draft.Surface)
	if err != nil {
		return EventEnvelope{}, err
	}
	if err := s.writeLineLocked(env); err != nil {
		return EventEnvelope{}, err
	}
	return s.appendEnvelopeLocked(env), nil
}

// buildEnvelopeLocked 构造完整信封：Seq 取当前最大 +1；message 类型做
// blob 引用替换（文件存引用形态）。
func (s *jsonlSession) buildEnvelopeLocked(typ EventType, data json.RawMessage, ignorable bool, surface *SurfaceIntent) (EventEnvelope, error) {
	if isMessageEvent(typ) {
		encoded, err := encodeBlobs(data, s.blobsDir())
		if err != nil {
			return EventEnvelope{}, err
		}
		data = encoded
	}
	return EventEnvelope{
		Seq:       s.seq + 1,
		Time:      time.Now(),
		Type:      typ,
		Data:      data,
		Ignorable: ignorable,
		Surface:   surface,
	}, nil
}

// writeLineLocked 把信封追加为一行 JSON（行尾换行；完整的行 = 成功
// append 的判定标准：无换行的尾行视为撕裂，恢复时丢弃）。
//
// 跨进程 Delete 防护：Unix 写已 unlink 的句柄、Windows 借
// FILE_SHARE_DELETE 删除打开中的文件，都会让句柄写入**静默成功**
// （数据进 void）——因此每次 append 前校验会话目录仍在，删除后 fail
// closed 返回 ErrDeleted，不允许「报成功但丢数据」。
func (s *jsonlSession) writeLineLocked(env EventEnvelope) error {
	if _, err := os.Stat(s.dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: session dir removed by another process", ErrDeleted)
		}
		return fmt.Errorf("session: stat session dir: %w", err)
	}
	if s.f == nil {
		return ErrSessionClosed
	}
	line, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("%w: marshal envelope: %v", ErrCorruptLog, err)
	}
	line = append(line, '\n')
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("session: append jsonl: %w", err)
	}
	return nil
}

// loadEvents 逐行读事件日志。返回事件序列与撕裂截断点：
//
//   - 尾部撕裂（无换行的最后一行，无论内容是否恰好合法）→ 只丢该碎片，
//     返回截断 offset，不回退更早的合法事件（§9.3）——完整的行（含换行）
//     才是「已成功 append」的判定标准；
//   - 中间的坏行（完整但非法）→ ErrCorruptLog（fail closed，不静默跳过）；
//   - Seq 断链 → ErrCorruptLog。
func loadEvents(path string) ([]EventEnvelope, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("session: open events.jsonl: %w", err)
	}
	defer f.Close()
	var (
		envs      []EventEnvelope
		offset    int64
		expectSeq = uint64(1)
	)
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadString('\n')
		complete := strings.HasSuffix(line, "\n")
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			var env EventEnvelope
			if json.Unmarshal([]byte(trimmed), &env) != nil || env.Seq == 0 || env.Type == "" {
				if complete {
					// 完整但非法的行在日志中部：不是撕裂，是损坏——拒绝加载。
					return nil, 0, fmt.Errorf("%w: corrupt line at offset %d", ErrCorruptLog, offset)
				}
				// 无换行的非法尾行 = 写入中途崩溃的撕裂碎片：丢弃。
				return envs, offset, nil
			}
			if !complete {
				// 合法但无换行：append 未完成（成功 = 完整行），视为撕裂丢弃。
				return envs, offset, nil
			}
			if env.Seq != expectSeq {
				return nil, 0, fmt.Errorf("%w: seq gap at offset %d: got %d want %d", ErrCorruptLog, offset, env.Seq, expectSeq)
			}
			expectSeq++
			envs = append(envs, env)
		}
		offset += int64(len(line))
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, 0, fmt.Errorf("session: read events.jsonl: %w", readErr)
		}
	}
	// 完整读完（每行都有换行）：无需截断。
	return envs, -1, nil
}

// acquireSessionLock 用 O_CREATE|O_EXCL 原子创建锁文件（单写者的文件锁
// 兜底）。锁被持有且未过期 → ErrWriterBusy（fail-fast，不阻塞等待）；
// 残留锁超过 stale 阈值 → 抢占（删除重建）。返回的 release 删除锁文件，
// 幂等。
func acquireSessionLock(path string, stale time.Duration) (func(), error) {
	release := func() { os.Remove(path) }
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			info, _ := json.Marshal(lockInfo{PID: os.Getpid(), AcquiredAt: time.Now()})
			f.Write(info)
			f.Close()
			return release, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("session: create lock: %w", err)
		}
		if isStaleLock(path, stale) {
			os.Remove(path)
			continue // 抢占一次；仍抢不到则放弃（不与抢占者竞争）
		}
		return nil, fmt.Errorf("%w: lock %s held by another writer", ErrWriterBusy, path)
	}
	return nil, fmt.Errorf("%w: lock %s contended", ErrWriterBusy, path)
}

// lockInfo 是锁文件内容：holder 与获取时间（stale 判定依据）。
type lockInfo struct {
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

// isStaleLock 只按时间戳判定：锁文件 mtime 距今超过 stale 阈值视为持有者
// 已崩溃（进程崩溃不会清理锁文件）。同进程的互斥由 store 的 open 缓存层
// 保证——缓存命中根本不会走到文件锁，因此这里不存在「本进程合法锁被误
// 抢占」的路径；跨进程持有者 PID 探活不可移植（Windows 无 POSIX
// kill(pid,0) 等价物），不做。
func isStaleLock(path string, stale time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) > stale
}

// validSessionID 校验会话 ID 可安全用作目录名：1..128 字符，仅
// [A-Za-z0-9_-]——防路径穿越（ID 来自调用方，fail closed）。
func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func isMessageEvent(t EventType) bool {
	return t == EventMessageUser || t == EventMessageAssistant
}
