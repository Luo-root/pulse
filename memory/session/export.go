package session

// 会话导出/导入（#152）：把事件日志变成自包含的可移植产物，并在导入端
// 保真还原（原 Seq/Time/Ignorable/Surface 逐字段保留）。
//
// 导出格式与 JSONL 会话文件同构：首行 header（FormatVersion 闸门），后续
// 每行一个 EventEnvelope；blob: 引用内联回原始字节（逆 encodeBlobs），
// 产物自包含——接收端不需要原 blobs 目录。
//
// 导入的保真路径是可选能力接口 Seeder（CreateSeeded：复用 Fork 的 seed
// 机制——「外部提供 seq 对齐的日志」是既有概念），不支持 Seeder 的
// store 返回 ErrSeedUnsupported，**绝不静默重编号**——checkpoint.Replaced、
// SourceRefs.Seq、SeedLength 都指向 Seq，重编号等于静默丢溯源。
// 校验 fail closed：Seq 连续性、注册表口径（unknown required 拒绝）、
// tool pairing、未闭合 turn/step——任何一项不过整单拒绝。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrSeedUnsupported：目标 store 未实现 Seeder——保真导入不可用。
var ErrSeedUnsupported = errors.New("session: store does not support seeded import")

// maxExportLine 是单行信封的解析上限（blob 内联后图片/媒体行可能很大）。
const maxExportLine = 1 << 28 // 256MiB

// Seeder 是支持「按原 Seq/Time 种子建会话」的 SessionStore 可选能力。
// 实现必须：保留信封的 Seq/Time/Ignorable/Surface；跑 validateSeedEnvs
// 同口径校验；header 版本闸门与 Create 相同（v1/v2 皆收，导入可携
// CompactedVersion）。
type Seeder interface {
	CreateSeeded(ctx context.Context, header SessionHeader, envs []EventEnvelope) (Session, error)
}

// validateSeedEnvs 是 seed 导入的共享校验链（memStore/JSONLStore 同口径，
// fail closed）：Seq 从 1 起严格连续；每个信封过注册表口径（codec 校验、
// Replace 仅 checkpoint、unknown required 拒绝）；tool 配对完整；无未闭合
// turn/step。
func validateSeedEnvs(reg *Registry, envs []EventEnvelope) error {
	probe := &memSession{reg: reg} // 只用 prepareAppend 的校验逻辑，不触共享状态
	for i, env := range envs {
		if env.Seq != uint64(i+1) {
			return fmt.Errorf("%w: seed seq %d at position %d（必须从 1 起连续）", ErrCorruptLog, env.Seq, i+1)
		}
		if _, err := probe.prepareAppend(EventDraft{
			Type:      env.Type,
			Data:      env.Data,
			Surface:   env.Surface,
			Ignorable: env.Ignorable,
		}); err != nil {
			return fmt.Errorf("seed seq %d: %w", env.Seq, err)
		}
	}
	pending, err := pendingToolCalls(envs, reg)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: unpaired %v", ErrForkSplitToolGroup, pending)
	}
	st, err := scanIncomplete(envs, reg)
	if err != nil {
		return err
	}
	if st.openTurn || st.openStep || len(st.pendingCalls) > 0 {
		return fmt.Errorf("%w: unclosed turn=%v step=%v pending=%v（导出流必须来自闭合会话）",
			ErrCorruptLog, st.openTurn, st.openStep, st.pendingCalls)
	}
	return nil
}

// ExportSession 把会话导出为自包含 JSONL 流：首行 header，后续每行一个
// 信封。blob: 引用内联回原始字节（JSONL 会话需 blobs 目录可读，缺失即
// 失败——不降级为空内容）；内存会话 payload 本就内联，原样写出。
// 产物可直接作为 ImportSession 的输入，也可原样充当 JSONL store 的会话
// 目录内容（events.jsonl 行格式同构；header 行即 header.json）。
func ExportSession(ctx context.Context, sess Session, w io.Writer) error {
	hdr, err := json.Marshal(sess.Header())
	if err != nil {
		return fmt.Errorf("session: marshal header: %w", err)
	}
	if _, err := w.Write(append(hdr, '\n')); err != nil {
		return fmt.Errorf("session: write header: %w", err)
	}
	envs, err := sess.Events(ctx, 0)
	if err != nil {
		return err
	}
	var blobsDir string
	if js, ok := sess.(*jsonlSession); ok {
		blobsDir = js.blobsDir()
	}
	bw := bufio.NewWriter(w)
	for _, env := range envs {
		if isMessageEvent(env.Type) && blobsDir != "" {
			inlined, err := decodeBlobs(env.Data, blobsDir)
			if err != nil {
				return err // blob 缺失/校验不过：fail closed，不静默丢字节
			}
			env.Data = inlined
		}
		line, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("session: marshal envelope seq %d: %w", env.Seq, err)
		}
		if _, err := bw.Write(append(line, '\n')); err != nil {
			return fmt.Errorf("session: write envelope: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("session: flush export: %w", err)
	}
	return nil
}

// ImportSession 从 ExportSession 的流恢复会话：优先走 store 的 Seeder
// 保真导入（原 Seq/Time 保留）；未实现 Seeder → ErrSeedUnsupported。
// 流校验与 seed 校验 fail closed（见 validateSeedEnvs）。
func ImportSession(ctx context.Context, store SessionStore, r io.Reader) (Session, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), maxExportLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("session: read header: %w", err)
		}
		return nil, fmt.Errorf("%w: empty stream", ErrCorruptLog)
	}
	var header SessionHeader
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return nil, fmt.Errorf("%w: header line: %v", ErrCorruptLog, err)
	}
	if header.FormatVersion != FormatVersion && header.FormatVersion != CompactedVersion {
		return nil, fmt.Errorf("%w: stream v%d, store speaks v%d/v%d（不猜测迁移）",
			ErrFormatVersion, header.FormatVersion, FormatVersion, CompactedVersion)
	}
	var envs []EventEnvelope
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env EventEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("%w: envelope line: %v", ErrCorruptLog, err)
		}
		envs = append(envs, env)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("session: read stream: %w", err)
	}
	seeder, ok := store.(Seeder)
	if !ok {
		return nil, ErrSeedUnsupported
	}
	return seeder.CreateSeeded(ctx, header, envs)
}

// CreateSeeded 实现 Seeder（内存版）：复用 appendEnvelopeLocked 的完整
// 信封写入路径（Seq 由调用方定）；存在性/版本闸门与 Create 同口径。
// 先查后写：常见撞车（重复导入同 ID）在写信封前 fail-fast；写入时双
// 检查兜 TOCTOU 窗口（分两段加锁，不与 memSession 方法形成锁序嵌套）。
func (s *memStore) CreateSeeded(ctx context.Context, header SessionHeader, envs []EventEnvelope) (Session, error) {
	header, err := normalizeImportHeader(header)
	if err != nil {
		return nil, err
	}
	if err := validateSeedEnvs(s.reg, envs); err != nil {
		return nil, err
	}
	s.mu.Lock()
	exists := s.sessions[header.SessionID] != nil
	s.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("%w: id %s", ErrSessionExists, header.SessionID)
	}
	sess := newMemSession(header, s.reg, s)
	sess.mu.Lock()
	for _, env := range envs {
		sess.appendEnvelopeLocked(env)
	}
	sess.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[header.SessionID] != nil {
		return nil, fmt.Errorf("%w: id %s", ErrSessionExists, header.SessionID)
	}
	s.sessions[header.SessionID] = sess
	return sess, nil
}

// CreateSeeded 实现 Seeder（JSONL 版）：createSession 即 Fork 的落盘路径
// ——seed 逐条 encodeBlobs（重建 blobs 文件）+ 原样写行（Seq/Time 保留）。
func (s *JSONLStore) CreateSeeded(ctx context.Context, header SessionHeader, envs []EventEnvelope) (Session, error) {
	header, err := normalizeImportHeader(header)
	if err != nil {
		return nil, err
	}
	if err := validateSeedEnvs(s.reg, envs); err != nil {
		return nil, err
	}
	return s.createSession(header, envs)
}

// normalizeImportHeader 是导入 header 的归一与版本闸门：v1/v2 皆收
// （镜像 Open——压缩过的会话 header 是 CompactedVersion）；零值兜底与
// Create 同口径。
func normalizeImportHeader(header SessionHeader) (SessionHeader, error) {
	if header.FormatVersion == 0 {
		header.FormatVersion = FormatVersion
	}
	if header.FormatVersion != FormatVersion && header.FormatVersion != CompactedVersion {
		return SessionHeader{}, fmt.Errorf("%w: stream v%d, store speaks v%d/v%d（不猜测迁移）",
			ErrFormatVersion, header.FormatVersion, FormatVersion, CompactedVersion)
	}
	if header.SessionID == "" {
		header.SessionID = newID()
	}
	if header.CreatedAt.IsZero() {
		header.CreatedAt = time.Now()
	}
	return header, nil
}
