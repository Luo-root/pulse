package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

func init() {
	recoveryCommands["temporal/worker"] = temporalWorkerCommand
	recoveryCommands["temporal/submit"] = temporalSubmitCommand
	recoveryCommands["temporal/history"] = temporalHistoryCommand
	recoveryCommands["temporal/signal"] = temporalSignalCommand
	recoveryCommands["temporal/result"] = temporalResultCommand
	recoveryCommands["temporal/cancel"] = temporalCancelCommand
}

// submit 把 RunID 当新 Workflow ID 的前缀；后续命令把它当 submit 返回的 workflow_id。
// worker 命令一直阻塞，首次前缀历史就是可处理任务的证据；强杀时没有 graceful Stop。
func temporalWorkerCommand(ctx context.Context, cfg Config) (any, error) {
	closeWorker, err := TemporalRecoveryWorker(ctx, cfg)
	if err != nil {
		return nil, err
	}
	<-ctx.Done()
	return nil, errors.Join(ctx.Err(), closeWorker())
}

func temporalControlClient(ctx context.Context, cfg Config) (client.Client, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("temporal: require real server address")
	}
	return client.DialContext(ctx, client.Options{HostPort: cfg.Address, Logger: temporalQuietLogger{}})
}

func temporalSubmitCommand(ctx context.Context, cfg Config) (any, error) {
	c, err := temporalControlClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	wr, err := StartTemporalRecovery(ctx, c, cfg)
	if err != nil {
		return nil, err
	}
	return map[string]any{"workflow_id": wr.GetID(), "run_id": wr.GetRunID()}, nil
}

func temporalHistoryCommand(ctx context.Context, cfg Config) (any, error) {
	c, err := temporalControlClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	evidence, err := WaitTemporalPrefix(ctx, c, cfg.RunID, "", cfg.CheckpointAt)
	if err != nil {
		return evidence, err
	}
	g, err := newGraph(cfg.Shape, cfg.Nodes)
	if err != nil {
		return evidence, err
	}
	want, _ := g.reference(cfg.Rounds)
	if len(evidence.Values) > len(want) {
		return evidence, fmt.Errorf("temporal prefix exceeds graph")
	}
	for id, value := range evidence.Values {
		if value != want[id] {
			return evidence, fmt.Errorf("temporal prefix n%d value %d want %d", id, value, want[id])
		}
	}
	return evidence, nil
}

func temporalSignalCommand(ctx context.Context, cfg Config) (any, error) {
	c, err := temporalControlClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	err = c.SignalWorkflow(ctx, cfg.RunID, "", TemporalRecoverySignal, nil)
	return map[string]any{"workflow_id": cfg.RunID, "signaled": err == nil}, err
}

func temporalResultCommand(ctx context.Context, cfg Config) (any, error) {
	c, err := temporalControlClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var value uint64
	if err := c.GetWorkflow(ctx, cfg.RunID, "").Get(ctx, &value); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), temporalCleanupTimeout)
		defer cancel()
		return nil, errors.Join(err, cancelTemporalRun(cleanupCtx, c, cfg.RunID, ""))
	}
	g, err := newGraph(cfg.Shape, cfg.Nodes)
	if err != nil {
		return nil, err
	}
	wantValues, want := g.reference(cfg.Rounds)
	if value != want {
		return nil, fmt.Errorf("temporal recovery checksum %d want %d", value, want)
	}
	evidence, err := readTemporalPrefix(ctx, c, cfg.RunID, "", cfg.Nodes, true)
	if err != nil {
		return evidence, err
	}
	if evidence.Scheduled != cfg.Nodes || evidence.Completed != cfg.Nodes || evidence.Pending != 0 {
		return evidence, fmt.Errorf("temporal recovery: incomplete final Activity history")
	}
	for id, got := range evidence.Values {
		if got != wantValues[id] {
			return evidence, fmt.Errorf("temporal recovery n%d value %d want %d", id, got, wantValues[id])
		}
	}
	return map[string]any{"workflow_id": cfg.RunID, "checksum": value, "scheduled": evidence.Scheduled,
		"completed": evidence.Completed, "pending": evidence.Pending, "last_event_id": evidence.LastEventID}, nil
}

func temporalCancelCommand(_ context.Context, cfg Config) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), temporalCleanupTimeout)
	defer cancel()
	c, err := temporalControlClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	err = cancelTemporalRun(ctx, c, cfg.RunID, "")
	return map[string]any{"workflow_id": cfg.RunID, "canceled_or_terminal": err == nil}, err
}

// TemporalRecoveryWorker 供宿主的独立 worker 子进程使用。返回时已连接并开始 poll。
// 父进程应在 WaitTemporalPrefix 成功后直接 Kill 该子进程；close 仅供正常退出收尾，
// 不能把调用 close（SDK graceful Stop）当作进程 crash 恢复的证据。
func TemporalRecoveryWorker(ctx context.Context, cfg Config) (close func() error, err error) {
	g, err := newGraph(cfg.Shape, cfg.Nodes)
	if err != nil {
		return nil, err
	}
	r, err := buildTemporal(ctx, g, cfg, nil)
	if err != nil {
		return nil, err
	}
	return r.close, nil
}

// StartTemporalRecovery 由父进程持有的独立 client 提交。RunID 仍是前缀，每次生成新 ID。
// 替换 worker 后用 wr.GetID()/GetRunID() 和 TemporalRecoverySignal 发送任意 signal payload，
// 再 wr.Get(ctx, &checksum)。SQLite 服务的进程、目录与生命周期完全由宿主管理。
func StartTemporalRecovery(ctx context.Context, c client.Client, cfg Config) (client.WorkflowRun, error) {
	g, err := newGraph(cfg.Shape, cfg.Nodes)
	if err != nil {
		return nil, err
	}
	if cfg.Queue == "" || cfg.CheckpointAt < 1 || cfg.CheckpointAt > len(g.names) {
		return nil, fmt.Errorf("temporal recovery: require queue and checkpoint in [1,%d]", len(g.names))
	}
	in := TemporalWorkflowInput{
		Predecessors: g.predecessors, Terminals: g.terminals, Rounds: cfg.Rounds,
		Parallelism: cfg.Parallelism, GateAt: cfg.CheckpointAt,
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	prefix, err := temporalIDPrefix(cfg.RunID)
	if err != nil {
		return nil, err
	}
	id := prefix + "/recovery"
	wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: cfg.Queue}, TemporalWorkflowName, in)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), temporalCleanupTimeout)
		defer cancel()
		return nil, errors.Join(err, cancelTemporalRun(cleanupCtx, c, id, ""))
	}
	return wr, nil
}

// TemporalPrefixEvidence 只来自独立服务端历史；Values 按节点下标排列。
// Ready 时 Scheduled=Completed=prefix，Pending=0。LastEventID 保留复查位置。
type TemporalPrefixEvidence struct {
	WorkflowID  string   `json:"workflow_id"`
	RunID       string   `json:"run_id"`
	Scheduled   int      `json:"scheduled"`
	Completed   int      `json:"completed"`
	Pending     int      `json:"pending"`
	LastEventID int64    `json:"last_event_id"`
	Values      []uint64 `json:"values"`
}

// WaitTemporalPrefix 不读 worker 内存、日志、探针或 ready 文件。它读取服务端全量历史快照，
// 证实恰好 prefix 个常规 Activity 已确认完成，没有后缀或在途 Activity，才允许父进程强杀。
// 历史确认不是任意 crash 的恰好一次保证：此协议特意选择没有在途 Activity 的退出边界。
func WaitTemporalPrefix(ctx context.Context, c client.Client, workflowID, runID string, prefix int) (TemporalPrefixEvidence, error) {
	if prefix < 1 {
		return TemporalPrefixEvidence{}, fmt.Errorf("temporal recovery: prefix must be positive")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		evidence, err := readTemporalPrefix(ctx, c, workflowID, runID, prefix, false)
		var missing *serviceerror.NotFound
		if err != nil && !errors.As(err, &missing) {
			return evidence, err
		}
		if err == nil && evidence.Completed == prefix && evidence.Pending == 0 {
			return evidence, nil
		}
		select {
		case <-ctx.Done():
			return evidence, ctx.Err()
		case <-ticker.C:
		}
	}
}

func readTemporalPrefix(ctx context.Context, c client.Client, workflowID, runID string, prefix int, allowCompleted bool) (TemporalPrefixEvidence, error) {
	evidence := TemporalPrefixEvidence{WorkflowID: workflowID, RunID: runID, Values: make([]uint64, prefix)}
	scheduled := make(map[int64]int, prefix)
	seen, completed := make([]bool, prefix), make([]bool, prefix)
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return evidence, fmt.Errorf("temporal prefix history: %w", err)
		}
		evidence.LastEventID = event.GetEventId()
		if attrs := event.GetWorkflowExecutionStartedEventAttributes(); attrs != nil {
			if evidence.RunID == "" {
				evidence.RunID = attrs.GetOriginalExecutionRunId()
			}
			var input TemporalWorkflowInput
			if err := converter.GetDefaultDataConverter().FromPayloads(attrs.GetInput(), &input); err != nil {
				return evidence, fmt.Errorf("temporal prefix workflow input: %w", err)
			}
			if attrs.GetWorkflowType().GetName() != TemporalWorkflowName || (!allowCompleted && input.GateAt != prefix) || (allowCompleted && len(input.Predecessors) != prefix) {
				return evidence, fmt.Errorf("temporal prefix: Workflow type, gate or node count does not match history request")
			}
		}
		if attrs := event.GetWorkflowExecutionSignaledEventAttributes(); attrs != nil && !allowCompleted && attrs.GetSignalName() == TemporalRecoverySignal {
			return evidence, fmt.Errorf("temporal prefix: recovery signal already received; gate no longer holds")
		}
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			if attrs.GetActivityType().GetName() != TemporalActivityName || attrs.GetRetryPolicy().GetMaximumAttempts() != 1 {
				return evidence, fmt.Errorf("temporal prefix: not a regular single-attempt benchmark Activity")
			}
			id, err := strconv.Atoi(trimTemporalNodePrefix(attrs.GetActivityId()))
			if err != nil || id < 0 || id >= prefix || attrs.GetActivityId() != "n"+strconv.Itoa(id) {
				return evidence, fmt.Errorf("temporal prefix: unexpected scheduled Activity %q", attrs.GetActivityId())
			}
			if seen[id] {
				return evidence, fmt.Errorf("temporal prefix: Activity n%d scheduled twice", id)
			}
			seen[id], scheduled[event.GetEventId()] = true, id
			evidence.Scheduled++
			evidence.Pending++
		}
		if attrs := event.GetActivityTaskCompletedEventAttributes(); attrs != nil {
			id, ok := scheduled[attrs.GetScheduledEventId()]
			if !ok || completed[id] {
				return evidence, fmt.Errorf("temporal prefix: unexpected/duplicate completion for scheduled event %d", attrs.GetScheduledEventId())
			}
			if err := converter.GetDefaultDataConverter().FromPayloads(attrs.GetResult(), &evidence.Values[id]); err != nil {
				return evidence, fmt.Errorf("temporal prefix result n%d: %w", id, err)
			}
			completed[id] = true
			evidence.Completed++
			evidence.Pending--
		}
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
			if !allowCompleted {
				return evidence, fmt.Errorf("temporal prefix: Workflow ended before recovery signal")
			}
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED, enumspb.EVENT_TYPE_ACTIVITY_TASK_TIMED_OUT, enumspb.EVENT_TYPE_ACTIVITY_TASK_CANCELED,
			enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED,
			enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED,
			enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW:
			return evidence, fmt.Errorf("temporal prefix: unexpected terminal event %s at %d", event.GetEventType(), event.GetEventId())
		}
	}
	return evidence, nil
}

func trimTemporalNodePrefix(id string) string {
	if len(id) > 0 && id[0] == 'n' {
		return id[1:]
	}
	return ""
}
