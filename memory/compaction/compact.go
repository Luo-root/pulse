package compaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/session"
)

// mustJSON 序列化载荷（本包构造的类型均可无损 JSON，失败即程序错误）。
func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("compaction: marshal payload %T: %v", v, err))
	}
	return data
}

// Options 配置一次压缩。
type Options struct {
	// Engine 产出摘要（必填）：LLMSummarizer 或 DeterministicSummarizer。
	Engine Engine
	// Meter 估算 token（nil 用 CharMeter）——写进 summarized 的审计依据
	// 与 Pressure 判定共用。
	Meter Meter
	// ModelName 记进审计（summarized payload 的 Model 字段）。
	ModelName string
	// Window 是选区（fold 后 surface 的 0-based 消息下标，含端点）；
	// nil = 全量。窗口切在 tool 组中间会被预检拒绝（整组移动，§9.3）。
	Window *[2]int
}

// Report 是一次压缩的回执（审计字段与日志事件一一对应）。
type Report struct {
	CompactionID  string
	Replaced      []uint64 // 被替代窗口的 source event seqs（checkpoint.Replaced）
	CheckpointSeq uint64
	InputTokens   int
	OutputTokens  int
}

// Compact 执行 §9.1 压缩事务：选区 → started（锁）→ Summarize →
// summarized → checkpoint（SurfaceReplace）→ ended。raw log 只增不减；
// Summarize 失败时 started 已落盘、无 checkpoint、ended 不写——未闭合
// compaction 留在日志里（恢复不假装完成）。
//
// 手动入口；压力触发先用 Pressure 判定再调用本函数。overflow 的请求级
// retry 编排归装配层。
func Compact(ctx context.Context, sess session.Session, opts Options) (Report, error) {
	if opts.Engine == nil {
		return Report{}, fmt.Errorf("compaction: engine is required")
	}
	events, err := sess.Events(ctx, 0)
	if err != nil {
		return Report{}, err
	}
	msgs, sources, err := session.FoldTrace(events, sess.Registry())
	if err != nil {
		return Report{}, err
	}
	start, end := 0, len(msgs)-1
	if opts.Window != nil {
		start, end = opts.Window[0], opts.Window[1]
	}
	if len(msgs) == 0 || start < 0 || end < start || end >= len(msgs) {
		return Report{}, fmt.Errorf("compaction: invalid window [%d,%d] over %d surface nodes", start, end, len(msgs))
	}
	window := msgs[start : end+1]
	windowSources := sources[start : end+1]
	// 预检：Replace 不新增 pairing 孤儿。用空 replacement 预检是保守正确
	// 的——压缩摘要（RoleUser，无 result）不保留任何窗口内配对；预检失败
	// 时**不落任何事件**（事务还没开始）。fold 重放时复核同一口径。
	if err := session.ValidateReplace(msgs[:start], window, nil, msgs[end+1:]); err != nil {
		return Report{}, fmt.Errorf("compaction: %w", err)
	}

	id := newID()
	// 1. started：事务锁。
	if _, err := sess.Append(ctx, session.EventDraft{
		Type: session.EventCompactionStarted,
		Data: mustJSON(session.CompactionStatusPayload{
			ID:         id,
			Model:      opts.ModelName,
			SourceRefs: cloneSeqs(windowSources),
		}),
	}); err != nil {
		return Report{}, fmt.Errorf("compaction %s: append started: %w", id, err)
	}
	// 2–3. summarize；失败即停在未闭合状态（审计可见，不假装完成）。
	res, err := opts.Engine.Summarize(ctx, SummarizeInput{Messages: window})
	if err != nil {
		return Report{}, fmt.Errorf("compaction %s: %w", id, err)
	}
	inTok := res.Usage.InputTokens
	outTok := res.Usage.OutputTokens
	if meter := opts.Meter; meter != nil && inTok == 0 {
		inTok = meter.Tokens(window)
	}
	// 4. summarized：记录摘要模型、usage 与来源。
	if _, err := sess.Append(ctx, session.EventDraft{
		Type: session.EventCompactionSummarized,
		Data: mustJSON(session.CompactionStatusPayload{
			ID:           id,
			Model:        res.Model,
			InputTokens:  inTok,
			OutputTokens: outTok,
			SourceRefs:   cloneSeqs(windowSources),
		}),
	}); err != nil {
		return Report{}, fmt.Errorf("compaction %s: append summarized: %w", id, err)
	}
	// 5. checkpoint：SurfaceReplace 替代窗口；载荷节点集来自 Summarize
	// （RoleUser 稳定前缀），Replaced 是完整 source refs。
	checkpointEnv, err := sess.Append(ctx, session.EventDraft{
		Type: session.EventCompactionCheckpoint,
		Data: mustJSON(session.CompactionCheckpointPayload{
			Messages: []llm.Message{res.Summary},
			Replaced: cloneSeqs(windowSources),
		}),
		Surface: &session.SurfaceIntent{
			Op:      session.SurfaceReplace,
			Start:   start,
			End:     end,
			Sources: cloneSeqs(windowSources),
		},
	})
	if err != nil {
		return Report{}, fmt.Errorf("compaction %s: append checkpoint: %w", id, err)
	}
	// 6. ended：收口。
	if _, err := sess.Append(ctx, session.EventDraft{
		Type: session.EventCompactionEnded,
		Data: mustJSON(session.CompactionStatusPayload{
			ID:         id,
			Reason:     "completed",
			SourceRefs: cloneSeqs(windowSources),
		}),
	}); err != nil {
		return Report{}, fmt.Errorf("compaction %s: append ended: %w", id, err)
	}
	return Report{
		CompactionID:  id,
		Replaced:      cloneSeqs(windowSources),
		CheckpointSeq: checkpointEnv.Seq,
		InputTokens:   inTok,
		OutputTokens:  outTok,
	}, nil
}

// PruneResults 对会话中所有超预算的 tool result 节点做 §9.2 deterministic
// pruning：每个节点一次独立 checkpoint Replace（窗口 = 单节点，替代节点
// 为 head+marker+tail 形态，Replaced 记录原 result 的 source seq）。原
// result 事件完整保留在 raw log。确定性操作，无 summarize 步骤。
// 返回发生裁剪的节点数与其 checkpoint seq 列表。
func PruneResults(ctx context.Context, sess session.Session, opts PruneOptions) (int, []uint64, error) {
	events, err := sess.Events(ctx, 0)
	if err != nil {
		return 0, nil, err
	}
	msgs, sources, err := session.FoldTrace(events, sess.Registry())
	if err != nil {
		return 0, nil, err
	}
	// 先收集再替换：Replace 会改变后续节点的下标，按从后往前的顺序执行
	// 保证每次窗口坐标仍有效（Replace 窗口 [i,i] 替换为 1 条，长度不变）。
	oversized := OversizedToolNodes(msgs, opts)
	var checkpoints []uint64
	for _, idx := range oversized {
		pruned, changed := PruneResult(msgs[idx], opts)
		if !changed {
			continue
		}
		// 单节点窗口：call 在窗口外（idx-1 的 assistant），替代节点保留
		// 同 ToolCallID → 配对仍成立（ValidateReplace 的「不新增孤儿」口径）。
		replacement := []*llm.Message{pruned}
		if err := session.ValidateReplace(msgs[:idx], msgs[idx:idx+1], replacement, msgs[idx+1:]); err != nil {
			return len(checkpoints), checkpoints, fmt.Errorf("prune node %d: %w", idx, err)
		}
		src := sources[idx : idx+1]
		env, err := sess.Append(ctx, session.EventDraft{
			Type: session.EventCompactionCheckpoint,
			Data: mustJSON(session.CompactionCheckpointPayload{
				Messages: []llm.Message{*pruned},
				Replaced: cloneSeqs(src),
			}),
			Surface: &session.SurfaceIntent{
				Op:      session.SurfaceReplace,
				Start:   idx,
				End:     idx,
				Sources: cloneSeqs(src),
			},
		})
		if err != nil {
			return len(checkpoints), checkpoints, fmt.Errorf("prune node %d: %w", idx, err)
		}
		checkpoints = append(checkpoints, env.Seq)
	}
	return len(checkpoints), checkpoints, nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("compaction: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func cloneSeqs(src []uint64) []uint64 {
	out := make([]uint64, len(src))
	copy(out, src)
	return out
}
