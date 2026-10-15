package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// CheckpointStreamID 是持久逻辑流及读取组的身份。Scope 来自持久图作用域，
// Generation 由存储分配和核验；不能使用本次 ExecutionID 代替其中任何一项。
// 恢复可以提升 Generation 以隔离旧交付，但必须保留同一逻辑流的消息与进度。
type CheckpointStreamID struct {
	Scope, NodeID, Key, Group string
	Generation                uint64
}

// CheckpointStreamSource 声明业务来源，不尝试保存 Go 闭包或调用栈。
// Resumable 表示业务能按 StreamWriter.Position 返回的位置继续；没有此承诺的
// 已完成来源仍可重放可靠尾部，活跃来源则不能假装可以中途恢复。
type CheckpointStreamSource struct {
	ID        string
	Resumable bool
}

// CheckpointStreamDescriptor 是完整节点成果里的流引用，不携带本地 channel。
// Attempt 是实际写端的完整节点尝试，祖先 Sub 不能替换它或冒充写端终态。
// Element 为消息元素的真实 Go 类型；消息内容与读取进度由独立记录保存。
// Sub 历史引用的 Terminal 可为空；它不终结实际写端，只能认回可续读 active 或成功终态。
type CheckpointStreamDescriptor struct {
	ID          CheckpointStreamID
	Graph, Path string
	Attempt     string
	Element     reflect.Type
	Source      CheckpointStreamSource
	Capacity    int
	Terminal    StreamTerminal
	Stopped     bool
}

// CheckpointStreamMessage 是一条可靠消息，Sequence 从 1 起，重投不重新编号。
// Value 的编码、版本和深复制归 Durable；钩子确认前必须完成所需编码。
type CheckpointStreamMessage struct {
	Sequence uint64
	Value    any
}

// CheckpointStreamRecord 是 Open 返回的权威恢复结果。Pending 恰好包含仍有读取
// 义务的消息，按 Sequence 递增；已确认的缺口由 LastSequence、AckedThrough 和
// Pending 的补集表示，不能把最大确认序号当成连续前沿。消息总数不超过 Capacity。
// Resumed 区分新来源与认回的活跃来源；Cursor 是最后一次可靠追加的来源位置。
type CheckpointStreamRecord struct {
	Stream                     CheckpointStreamDescriptor
	LastSequence, AckedThrough uint64
	Pending                    []CheckpointStreamMessage
	Cursor                     any
	Resumed                    bool
}

// CheckpointStreamOpen 打开或认回流。Restore 非空时必须重建记录，即使来源已 completed。
// Site.Execution 只用于观察；稳定身份是 Scope/NodeID/Key/Group 和持久 Generation。
type CheckpointStreamOpen struct {
	Site                   Site
	Scope, Attempt, NodeID string
	Key                    CheckpointKey
	Group                  string
	Source                 CheckpointStreamSource
	Capacity               int
	Restore                *CheckpointStreamDescriptor
}

// CheckpointStreamAppend 可靠追加一条消息及其来源位置；确认后才允许本地交付。
// Attempt 必须与 Stream 的来源尝试一致且全部祖先有效。
type CheckpointStreamAppend struct {
	Site    Site
	Attempt string
	Stream  CheckpointStreamID
	Message CheckpointStreamMessage
	Cursor  any
}

// CheckpointStreamAcknowledgement 确认一个读取组中的消息。Core 另核验本地当前交付；
// Durable 必须核验 Generation 以及来源和处理尝试的全部祖先。来源自身已经 confirmed
// 是合法的（completed 来源仍有尾部）；failed 来源或 failed 祖先不合法。
type CheckpointStreamAcknowledgement struct {
	Stream   CheckpointStreamID
	Sequence uint64
}

// CheckpointState 是显式受管状态的一项；完整快照参与同一次消息提交。
// 它不包含任意闭包、引用对象的隐式改动或外部系统副作用。
type CheckpointState struct {
	Name  string
	Type  reflect.Type
	Value any
}

// CheckpointMessageAttemptRequest 在回调之前认回持久预算，重启不补充 Limit。
// 同一消息的预算按持久逻辑来源、组、序号、处理节点和父尝试识别，不按回调地址识别。
// 恢复提升 Generation 只隔离旧交付，不能给同一逻辑消息补充预算。
type CheckpointMessageAttemptRequest struct {
	Site                   Site
	Scope, Attempt, NodeID string
	Input                  CheckpointStreamAcknowledgement
	Limit                  int
}

// CheckpointMessageAttempt 携带持久尝试和上次已提交的显式状态快照。
// Exhausted 时不得进入业务回调；失败确认以后才允许打开下一次尝试。
type CheckpointMessageAttempt struct {
	Attempt CheckpointAttempt
	State   []CheckpointState
}

// CheckpointMessageCommit 在一份可靠事务中确认输入、保存显式状态、普通值成果及输出
// 消息。所有记录确认后 Core 才发布输出和本地 Ack；多输出不承诺同时可见。
// 手动 Ack 没有消息 Attempt，只含 Inputs；Stops 显式结束读取义务，不伪装逐条 Ack。
// Version、所有权和外部副作用的幂等/outbox 归 Durable，不承诺通用 exactly-once。
type CheckpointMessageCommit struct {
	Site           Site
	Scope, Attempt string
	Inputs         []CheckpointStreamAcknowledgement
	State          []CheckpointState
	// Values 仅含本完整节点帧尚未随消息确认的普通键，载荷是合并后生效的首写。
	// 已确认键在后续消息中省略；普通槽位仍待完整节点 Commit 后发布。
	Values  []CheckpointOutput
	Outputs []CheckpointStreamAppend
	Stops   []CheckpointStreamID
}

// CheckpointStreamFinish 记录实际完整 Aspect 链确定的写端终态。
// 成功终态随 CheckpointCommit.Streams 原子保存；真实失败使用 Finish 同步确认。
// 返回错误可能已写成，Core 不重试，不发布成功 EOF，由恢复读取权威事实。
type CheckpointStreamFinish struct {
	Site         Site
	Attempt      string
	Stream       CheckpointStreamID
	Terminal     StreamTerminal
	LastSequence uint64
	Cursor       any
}

// CheckpointStreamHooks 是可选增量可靠边界；启用后六项必须齐备。
// 遵守 CheckpointHooks 的同步、并发、panic 和失败锁存契约；不得在普通 Codec 中编码
// 本地端点。所有修改必须核验持久代际和祖先有效性，旧交付不能推进新代际。
type CheckpointStreamHooks struct {
	Open        func(context.Context, CheckpointStreamOpen) (CheckpointStreamRecord, error)
	Append      func(context.Context, CheckpointStreamAppend) error
	Commit      func(context.Context, CheckpointMessageCommit) error
	OpenMessage func(context.Context, CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error)
	FailMessage func(context.Context, string) error
	Finish      func(context.Context, CheckpointStreamFinish) error
}

func (h *CheckpointStreamHooks) complete() bool {
	return h != nil && h.Open != nil && h.Append != nil && h.Commit != nil &&
		h.OpenMessage != nil && h.FailMessage != nil && h.Finish != nil
}

// WithStreamSource 为 OpenStream 显式声明稳定来源及业务是否能按位置续读。
// 可续读来源使用 SendAt 同步保存消息与外部游标，恢复后从 Position 继续。
// 非可续读来源也会保存消息内容，可恢复 completed 来源的未确认尾部。
func WithStreamSource(id string, resumable bool) StreamOption {
	return func(c *streamConfig) { c.checkpointSource = CheckpointStreamSource{ID: id, Resumable: resumable} }
}

// StreamPosition 是最近一次可靠接纳的位置；Cursor 不深复制。
type StreamPosition struct {
	Sequence uint64
	Cursor   any
}

// Position 返回当前来源位置。恢复时包含 Open 返回的权威游标，不靠闭包计数续读。
func (w *StreamWriter[T]) Position() StreamPosition {
	if w == nil || w.state == nil {
		return StreamPosition{}
	}
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	position := StreamPosition{Sequence: w.state.last}
	if checkpoint := w.state.checkpoint; checkpoint != nil {
		position.Cursor = checkpoint.cursor
	}
	return position
}

// SendAt 在检查点模式原子保存消息及其来源位置，确认后才交付；本地模式等同 Send，
// 不保存可恢复游标。它用于渐进来源，不用于有限消息回调（后者一次提交全部成果）。
func (w *StreamWriter[T]) SendAt(value T, cursor any) error {
	if w == nil || w.state == nil {
		return fmt.Errorf("pulse: SendAt: unopened stream writer")
	}
	if w.rc.message != nil {
		return w.rc.message.fail(fmt.Errorf("pulse: SendAt cannot advance a source cursor inside HandleMessage"))
	}
	if w.state.checkpoint == nil {
		return w.Send(value)
	}
	return w.sendCheckpointValue(value, cursor)
}

// 由端点提供泛型 factory。这里只认包内标记，普通 channel 和藏在普通值里的端点
// 仍走递归拒绝；applyRestoreLocked 只建本地结构，不在装配锁内调用可靠钩子。
type checkpointStreamFactory interface {
	pulseRestoreCheckpoint(*Execution, CheckpointStreamDescriptor, *checkpointStreamCache) any
}

type checkpointEndpoint interface {
	pulseCheckpointDescriptor() *CheckpointStreamDescriptor
	pulseEnsureCheckpoint() error
}

// 树缓存共用逻辑来源的一份端点。reference 在 Open 前累积约束（不等于授权）；
// opened 后每条新引用对已安装的权威状态核验。字段由 checkpointFailure.mu 保护。
type checkpointStreamCache struct {
	endpoint     checkpointEndpoint
	reference    CheckpointStreamDescriptor
	hasReference bool
	opened       bool
	resumed      bool
}

func checkpointStreamLogicalID(id CheckpointStreamID) CheckpointStreamID {
	id.Generation = 0 // 仅包内索引；公开流身份及交付仍保留权威代际。
	return id
}

type checkpointStream struct {
	exec       *Execution
	descriptor CheckpointStreamDescriptor
	cursor     any // 由所属 managedStream.mu 保护
	restore    bool
	request    *CheckpointStreamOpen // 实际写端先创建时的首次 Open；与恢复路径共用 once。
	cached     *checkpointStreamCache
	once       sync.Once
	err        error
}

func checkCheckpointDeclaration(config *checkpointConfig, ref keyRef) error {
	if isManagedStreamType(ref.typ) {
		if !config.hooks.StreamHooks.complete() {
			return fmt.Errorf("checkpoint stream key %q requires complete StreamHooks", ref.name)
		}
		if checkpointKey(ref).Element == nil {
			return fmt.Errorf("checkpoint stream key %q must use a Stream value endpoint", ref.name)
		}
		if checkpointChannelType(checkpointKey(ref).Element, nil) {
			return fmt.Errorf("checkpoint stream key %q element contains a channel", ref.name)
		}
		return nil
	}
	if checkpointChannelType(ref.typ, nil) {
		return fmt.Errorf("checkpoint mode does not support channel key %q", ref.name)
	}
	return nil
}

func checkpointKey(ref keyRef) CheckpointKey {
	key := CheckpointKey{Name: ref.name, Type: ref.typ}
	if isManagedStreamType(ref.typ) && ref.typ.Kind() == reflect.Struct {
		key.Element = reflect.Zero(ref.typ).Interface().(managedStreamDeclaration).pulseStreamElement()
	}
	return key
}

func checkpointOutput(ref keyRef, state slotState, value any) CheckpointOutput {
	output := CheckpointOutput{Key: checkpointKey(ref), State: publicSlotState(state), Value: value}
	if endpoint, ok := value.(checkpointEndpoint); ok && output.Key.Element != nil {
		output.Stream, output.Value = endpoint.pulseCheckpointDescriptor(), nil
	}
	return output
}

func checkCheckpointStreamDescriptor(key CheckpointKey, descriptor CheckpointStreamDescriptor) error {
	element := checkpointKey(keyRef{name: key.Name, typ: key.Type}).Element
	id := descriptor.ID
	if id.Scope == "" || id.NodeID == "" || id.Key == "" || id.Group == "" || id.Generation == 0 ||
		descriptor.Attempt == "" || descriptor.Graph == "" || descriptor.Source.ID == "" || descriptor.Capacity < 1 ||
		element == nil || descriptor.Element != element || (key.Element != nil && key.Element != element) {
		return fmt.Errorf("checkpoint stream %q has an invalid identity, source or element type", key.Name)
	}
	if descriptor.Terminal != "" && descriptor.Terminal != StreamCompleted && descriptor.Terminal != StreamStopped &&
		descriptor.Terminal != StreamFailed && descriptor.Terminal != StreamCanceled {
		return fmt.Errorf("checkpoint stream %q has an invalid terminal", key.Name)
	}
	if descriptor.Terminal == StreamStopped && !descriptor.Stopped {
		return fmt.Errorf("checkpoint stream %q has a stopped writer without a stopped reading group", key.Name)
	}
	if checkpointChannelType(element, nil) {
		return fmt.Errorf("checkpoint stream %q element contains a channel", key.Name)
	}
	return nil
}

// 历史 Sub 别名可能早于写端完成或读取组 Stop；这些快照仍引用同一来源。
// 稳定来源（含代际）必须完全一致，已知写端终态不能互相矛盾；Stop 只向前累积。
func mergeCheckpointStreamReference(previous, next CheckpointStreamDescriptor) (CheckpointStreamDescriptor, error) {
	left, right := previous, next
	left.Terminal, right.Terminal = "", ""
	left.Stopped, right.Stopped = false, false
	if left != right || (previous.Terminal != "" && next.Terminal != "" && previous.Terminal != next.Terminal) {
		return previous, fmt.Errorf("pulse: restore stream %q: aliases contain conflicting source descriptors", previous.ID.Key)
	}
	if previous.Terminal == "" {
		previous.Terminal = next.Terminal
	}
	previous.Stopped = previous.Stopped || next.Stopped
	return previous, nil
}

// 未完成 Open 的跨作用域引用只增加待权威确认的约束；不能自行提升端点代际。
func mergeCheckpointStreamConstraints(previous, next CheckpointStreamDescriptor) (CheckpointStreamDescriptor, error) {
	generation := max(previous.ID.Generation, next.ID.Generation)
	previous.ID.Generation, next.ID.Generation = generation, generation
	return mergeCheckpointStreamReference(previous, next)
}

func sameCheckpointStreamSource(left, right CheckpointStreamDescriptor) bool {
	left.ID.Generation, right.ID.Generation = 0, 0
	left.Terminal, right.Terminal = "", ""
	left.Stopped, right.Stopped = false, false
	return left == right
}

// 历史空终态只引用来源身份，允许认回可续读的 active 来源；已知写端终态不能回退。
// 是否 Resumed 是权威 Open 的事实，热缓存同样核验，不能借缓存冒充一次恢复。
func checkCheckpointStreamReference(authoritative, reference CheckpointStreamDescriptor, resumed bool) error {
	if !sameCheckpointStreamSource(authoritative, reference) {
		return fmt.Errorf("checkpoint stream %q reference names a different persistent source", reference.ID.Key)
	}
	active := authoritative.Terminal == ""
	if !resumed || (active && (!authoritative.Source.Resumable || reference.Terminal != "")) ||
		(!active && authoritative.Terminal != StreamCompleted && authoritative.Terminal != StreamStopped) ||
		reference.ID.Generation > authoritative.ID.Generation ||
		(reference.Terminal != "" && reference.Terminal != authoritative.Terminal) ||
		(reference.Stopped && !authoritative.Stopped) {
		return fmt.Errorf("checkpoint stream %q reference contradicts authoritative completion or progress", reference.ID.Key)
	}
	return nil
}

func checkCheckpointStreamRecord(request CheckpointStreamOpen, record CheckpointStreamRecord) error {
	descriptor := record.Stream
	if err := checkCheckpointStreamDescriptor(request.Key, descriptor); err != nil {
		return err
	}
	id := descriptor.ID
	if id.Scope != request.Scope || id.NodeID != request.NodeID || id.Key != request.Key.Name || id.Group != request.Group ||
		descriptor.Attempt != request.Attempt || descriptor.Graph != request.Site.Graph || descriptor.Path != request.Site.Path ||
		descriptor.Source != request.Source || descriptor.Capacity != request.Capacity {
		return fmt.Errorf("stream Open returned a different persistent source")
	}
	if history := request.Restore; history != nil {
		if err := checkCheckpointStreamReference(descriptor, *history, record.Resumed); err != nil {
			return fmt.Errorf("stream Open did not restore the source reference: %w", err)
		}
	}
	if record.Resumed && descriptor.Terminal == "" && !descriptor.Source.Resumable {
		return fmt.Errorf("stream source %q cannot resume an active source; declare a replayable source and continue from Position", descriptor.Source.ID)
	}
	if descriptor.Terminal == StreamFailed || descriptor.Terminal == StreamCanceled {
		return fmt.Errorf("stream Open returned a failed or canceled source")
	}
	if record.AckedThrough > record.LastSequence || len(record.Pending) > descriptor.Capacity ||
		(descriptor.Stopped && len(record.Pending) != 0) {
		return fmt.Errorf("stream Open returned invalid confirmation progress or capacity")
	}
	if !descriptor.Stopped && ((len(record.Pending) == 0 && record.AckedThrough != record.LastSequence) ||
		(len(record.Pending) != 0 && record.Pending[0].Sequence != record.AckedThrough+1)) {
		return fmt.Errorf("stream Open returned a non-contiguous confirmation frontier")
	}
	if !descriptor.Stopped && ((len(record.Pending) == 0 && record.AckedThrough != record.LastSequence) ||
		(len(record.Pending) != 0 && record.Pending[0].Sequence != record.AckedThrough+1)) {
		return fmt.Errorf("stream Open returned a confirmation frontier inconsistent with its pending messages")
	}
	previous := record.AckedThrough
	for _, message := range record.Pending {
		if message.Sequence <= previous || message.Sequence > record.LastSequence {
			return fmt.Errorf("stream Open returned duplicate or unordered pending messages")
		}
		previous = message.Sequence
		if err := checkCheckpointPayload(descriptor.Element, message.Value); err != nil {
			return err
		}
	}
	return checkCheckpointCursor(record.Cursor)
}

func checkCheckpointPayload(typ reflect.Type, value any) error {
	return checkCheckpointOutput(CheckpointOutput{Key: CheckpointKey{Name: "payload", Type: typ}, State: SlotStateReady, Value: value})
}

func checkCheckpointCursor(cursor any) error {
	if cursor != nil {
		return checkCheckpointPayload(reflect.TypeOf(cursor), cursor)
	}
	return nil
}

func (e *Execution) openCheckpointStream(ctx context.Context, request CheckpointStreamOpen) (record CheckpointStreamRecord, err error) {
	err = e.callCheckpoint("open stream", func() error {
		var err error
		record, err = e.checkpoint.hooks.StreamHooks.Open(ctx, request)
		if err != nil {
			return err
		}
		return checkCheckpointStreamRecord(request, record)
	})
	return record, err
}

// openManagedStreamCheckpoint 在端点发布前调用；本地模式返回 nil，仍走原实现。
func openManagedStreamCheckpoint[T any](rc *RunCtx, ref keyRef, cfg streamConfig) (*managedStream[T], error) {
	if rc.exec.checkpoint == nil {
		return nil, nil
	}
	if !rc.exec.checkpoint.hooks.StreamHooks.complete() || cfg.checkpointSource.ID == "" {
		return nil, rc.exec.stopCheckpoint("open stream", fmt.Errorf("OpenStream %q requires complete StreamHooks and WithStreamSource", ref.name))
	}
	request := CheckpointStreamOpen{Site: rc.exec.site, Scope: rc.exec.checkpoint.scope, Attempt: rc.checkpointAttempt.Scope,
		NodeID: rc.NodeID(), Key: checkpointKey(ref), Group: "default", Source: cfg.checkpointSource, Capacity: cfg.buf}
	declaration := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: request.Scope, NodeID: request.NodeID, Key: ref.name, Group: request.Group},
		Graph: request.Site.Graph, Path: request.Site.Path, Attempt: request.Attempt, Element: request.Key.Element,
		Source: request.Source, Capacity: request.Capacity,
	}
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	logical := checkpointStreamLogicalID(declaration.ID)
	cached := failure.streams[logical]
	var s *managedStream[T]
	var err error
	if cached != nil {
		endpoint, ok := cached.endpoint.(Stream[T])
		if !ok || !sameCheckpointStreamSource(*cached.endpoint.pulseCheckpointDescriptor(), declaration) {
			err = fmt.Errorf("OpenStream %q does not match the restored persistent source", ref.name)
		} else {
			s = endpoint.state
		}
	} else {
		cached = &checkpointStreamCache{}
		s = checkpointManagedStream[T](cfg.buf)
		s.checkpoint = &checkpointStream{exec: rc.exec, descriptor: declaration, request: &request, cached: cached}
		cached.endpoint = Stream[T]{state: s}
		if failure.streams == nil {
			failure.streams = make(map[CheckpointStreamID]*checkpointStreamCache)
		}
		failure.streams[logical] = cached
	}
	failure.mu.Unlock()
	if err != nil {
		return nil, rc.exec.stopCheckpoint("open stream", err)
	}
	// 与历史别名共用一次 Open 和一次尾部安装；等待时不持树锁或状态锁。
	if err := s.ensureCheckpoint(rc.ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func checkpointManagedStream[T any](capacity int) *managedStream[T] {
	return &managedStream[T]{queue: make(chan Delivery[T], capacity), changed: make(chan struct{}),
		done: make(chan struct{}), capacity: capacity, pending: make(map[uint64]*managedMessage[T])}
}

func (s *managedStream[T]) installCheckpointRecord(record CheckpointStreamRecord) error {
	if record.Stream.Element != reflect.TypeFor[T]() {
		return fmt.Errorf("checkpoint stream %q: element type does not match %s", record.Stream.ID.Key, reflect.TypeFor[T]())
	}
	// Open 已全量校验；安装前构造自己的消息容器，按声明类型赋值，保留业务引用。
	// 不原地改钩子的 Pending，也不让原始 any 的类型断言在持锁后才失败。
	messages := make([]*managedMessage[T], len(record.Pending))
	for i, saved := range record.Pending {
		assigned, ok := valueForType(reflect.ValueOf(saved.Value), record.Stream.Element)
		if !ok {
			return fmt.Errorf("checkpoint stream %q: payload type does not match %s", record.Stream.ID.Key, record.Stream.Element)
		}
		var value T
		if raw := assigned.Interface(); raw != nil { // nil 接口元素是合法值。
			value = raw.(T)
		}
		messages[i] = &managedMessage[T]{value: value, sequence: saved.Sequence}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last, s.frontier, s.stopped = record.LastSequence, record.AckedThrough, record.Stream.Stopped
	for _, message := range messages {
		s.pending[message.sequence] = message
		s.enqueueLocked(message)
	}
	// 持久写端终态与读取组 Stop 是独立事实；不能重新按当前 stopped 推导终态。
	if record.Stream.Terminal != "" {
		s.finished, s.terminal = true, record.Stream.Terminal
		close(s.done)
	}
	if s.stopped || (s.finished && len(s.pending) == 0) {
		s.closeQueueLocked()
	}
	s.signalLocked()
	return nil
}

func (Stream[T]) pulseRestoreCheckpoint(e *Execution, descriptor CheckpointStreamDescriptor, cached *checkpointStreamCache) any {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if endpoint, exists := e.checkpointStreams[descriptor.ID]; exists {
		return endpoint
	}
	s := checkpointManagedStream[T](descriptor.Capacity)
	s.checkpoint = &checkpointStream{exec: e, descriptor: descriptor, restore: true, cached: cached}
	endpoint := Stream[T]{state: s}
	if e.checkpointStreams == nil {
		e.checkpointStreams = make(map[CheckpointStreamID]any)
	}
	e.checkpointStreams[descriptor.ID] = endpoint
	return endpoint
}

func (s Stream[T]) pulseCheckpointDescriptor() *CheckpointStreamDescriptor {
	if s.state == nil || s.state.checkpoint == nil {
		return nil
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	descriptor := s.state.checkpoint.descriptor
	if s.state.finished {
		descriptor.Terminal = s.state.terminal
	}
	descriptor.Stopped = s.state.stopped
	return &descriptor
}

func (s Stream[T]) pulseEnsureCheckpoint() error { return s.state.ensureCheckpoint() }

// restoreCheckpointStreams 由 restored 节点的运行分支在锁外调用，早于完成事件。
// 已完成来源不重跑；历史 active 别名与续跑写端共享尾部和进度，不代替写端终结 Done。
func (e *Execution) restoreCheckpointStreams(n *Node) error {
	for _, output := range e.restored[n].Outputs {
		if output.Stream == nil {
			continue
		}
		_, value := e.slotOf(keyRef{name: output.Key.Name, typ: output.Key.Type}).snapshot()
		endpoint, ok := value.(checkpointEndpoint)
		if !ok {
			return e.stopCheckpoint("restore stream", fmt.Errorf("restored stream %q has no local endpoint factory", output.Key.Name))
		}
		if err := endpoint.pulseEnsureCheckpoint(); err != nil {
			return err
		}
	}
	return nil
}

// ensureCheckpoint 必须在 Receive/Messages/Stop 使用恢复端点之前调用，且不持流锁。
// 恢复别名和实际写端共用一次 Open；active 来源保留 Done，真正写端结束后才开放 EOF。
func (s *managedStream[T]) ensureCheckpoint(contexts ...context.Context) error {
	checkpoint := s.checkpoint
	if checkpoint == nil || (!checkpoint.restore && checkpoint.request == nil) {
		return nil
	}
	checkpoint.once.Do(func() {
		descriptor := checkpoint.descriptor
		site := checkpoint.exec.site
		site.Graph, site.Path = descriptor.Graph, descriptor.Path
		request := CheckpointStreamOpen{Site: site, Scope: descriptor.ID.Scope, Attempt: descriptor.Attempt,
			NodeID: descriptor.ID.NodeID, Key: CheckpointKey{Name: descriptor.ID.Key, Type: reflect.TypeFor[Stream[T]](), Element: descriptor.Element},
			Group: descriptor.ID.Group, Source: descriptor.Source, Capacity: descriptor.Capacity, Restore: &descriptor}
		if checkpoint.request != nil {
			request = *checkpoint.request
		}
		ctx := checkpoint.exec.checkpoint.failure.root.ctx
		if len(contexts) != 0 && contexts[0] != nil {
			ctx = contexts[0]
		}
		record, err := checkpoint.exec.openCheckpointStream(ctx, request)
		if err == nil {
			failure := checkpoint.exec.checkpoint.failure
			err = func() error {
				failure.mu.Lock()
				defer failure.mu.Unlock()
				if checkpoint.cached != nil && checkpoint.cached.hasReference {
					if err := checkCheckpointStreamReference(record.Stream, checkpoint.cached.reference, record.Resumed); err != nil {
						return err
					}
				}
				s.mu.Lock()
				checkpoint.descriptor, checkpoint.cursor = record.Stream, record.Cursor
				s.mu.Unlock()
				if err := s.installCheckpointRecord(record); err != nil {
					return err
				}
				if checkpoint.cached != nil {
					checkpoint.cached.opened, checkpoint.cached.resumed = true, record.Resumed
				}
				return nil
			}()
			if err != nil {
				err = checkpoint.exec.stopCheckpoint("restore stream", err)
			}
		}
		checkpoint.err = err
	})
	return checkpoint.err
}

// sendCheckpointValue 由 Send/SendAt 共用；钩子在流锁外，确认前不改序号、不入队。
// 写端串行；并发 Ack 只释放容量，不会抢走已经预检的位置。Stop 的可靠记录与本地
// 退出可能发生在确认之后，此时消息已经接纳，按显式 Stop 清理而不伪装发送失败。
func (w *StreamWriter[T]) sendCheckpointValue(value T, cursor any) error {
	if err := checkManagedRunCtx(w.rc, "Send"); err != nil {
		return w.state.recordFailure(err)
	}
	s := w.state
	if err := checkCheckpointPayload(reflect.TypeFor[T](), value); err != nil {
		return s.checkpoint.exec.stopCheckpoint("append stream", err)
	}
	if err := checkCheckpointCursor(cursor); err != nil {
		return s.checkpoint.exec.stopCheckpoint("append stream", err)
	}
	for {
		if err := w.rc.Context().Err(); err != nil {
			return s.recordFailure(err)
		}
		s.mu.Lock()
		changed, err := s.checkpointSendReadyLocked()
		s.mu.Unlock()
		if err != nil {
			return s.recordFailure(err)
		}
		if changed != nil {
			if err := waitMessageCapacity(w.rc, changed); err != nil {
				return s.recordFailure(err)
			}
			continue
		}
		// 等待操作门会让出额度，不能持流锁，也不能占着门等 Ack 释放容量。
		if err := s.operation.lock(w.rc); err != nil {
			return s.recordFailure(err)
		}
		s.mu.Lock()
		changed, err = s.checkpointSendReadyLocked()
		if canceled := w.rc.Context().Err(); canceled != nil {
			err = canceled
		}
		if changed != nil || err != nil {
			s.mu.Unlock()
			s.operation.unlock()
			if err != nil {
				return s.recordFailure(err)
			}
			if err := waitMessageCapacity(w.rc, changed); err != nil {
				return s.recordFailure(err)
			}
			continue
		}
		sequence, checkpoint := s.last+1, s.checkpoint
		append := CheckpointStreamAppend{Site: w.rc.exec.site, Attempt: checkpoint.descriptor.Attempt,
			Stream: checkpoint.descriptor.ID, Message: CheckpointStreamMessage{Sequence: sequence, Value: value}, Cursor: cursor}
		s.mu.Unlock()
		if err := w.rc.exec.callCheckpoint("append stream", func() error {
			return w.rc.exec.checkpoint.hooks.StreamHooks.Append(w.rc.Context(), append)
		}); err != nil {
			s.operation.unlock()
			return err
		}
		s.mu.Lock()
		s.last, checkpoint.cursor = sequence, cursor
		message := &managedMessage[T]{value: value, sequence: sequence}
		s.pending[sequence] = message
		s.enqueueLocked(message)
		s.mu.Unlock()
		s.operation.unlock()
		return nil
	}
}

func (s *managedStream[T]) checkpointSendReadyLocked() (<-chan struct{}, error) {
	if s.readerErr != nil {
		return nil, s.readerErr
	}
	if s.stopped {
		return nil, ErrStreamStopped
	}
	if s.finished {
		if s.err != nil {
			return nil, s.err
		}
		return nil, fmt.Errorf("pulse: Send: production already finished")
	}
	if s.last == ^uint64(0) {
		return nil, fmt.Errorf("pulse: Send: message sequence exhausted")
	}
	if len(s.pending) >= s.capacity {
		return s.changed, nil
	}
	return nil, nil
}

func (a *managedAck[T]) checkpointAcknowledgement() CheckpointStreamAcknowledgement {
	return CheckpointStreamAcknowledgement{Stream: a.stream.checkpoint.descriptor.ID, Sequence: a.message.sequence}
}

// checkpointAck 与 checkpointStop 只执行可靠边界，不改本地确认状态。调用者必须先核验
// 当前交付并排斥其并发 Nack/Stop，锁外调用，确认后再做原有本地状态变更。
func (a *managedAck[T]) checkpointAck(rc *RunCtx) error {
	checkpoint := a.stream.checkpoint
	if checkpoint == nil {
		return nil
	}
	ctx := checkpointActorContext(rc, checkpoint)
	if err := ctx.Err(); err != nil {
		return err
	}
	e := checkpoint.exec
	return e.commitCheckpointMessage(ctx, CheckpointMessageCommit{Site: e.site, Scope: checkpoint.descriptor.ID.Scope,
		Inputs: []CheckpointStreamAcknowledgement{a.checkpointAcknowledgement()}})
}

func (s *managedStream[T]) checkpointStop(rc *RunCtx) error {
	if s.checkpoint == nil {
		return nil
	}
	checkpoint := s.checkpoint
	ctx := checkpointActorContext(rc, checkpoint)
	if err := ctx.Err(); err != nil {
		return err
	}
	return checkpoint.exec.commitCheckpointMessage(ctx, CheckpointMessageCommit{Site: checkpoint.exec.site,
		Scope: checkpoint.descriptor.ID.Scope, Stops: []CheckpointStreamID{checkpoint.descriptor.ID}})
}

// 来源子执行的成功 Wait 会正常取消其 ctx，不能用它阻断父侧尾部确认。
// 宿主 Receive 的 RunCtx 只管理一次等待，返回时即取消；其交付确认使用执行 ctx。
func checkpointActorContext(rc *RunCtx, checkpoint *checkpointStream) context.Context {
	if rc != nil && rc.exec != nil {
		if rc.host {
			return rc.exec.ctx
		}
		return rc.ctx
	}
	return checkpoint.exec.checkpoint.failure.root.ctx
}

func (e *Execution) commitCheckpointMessage(ctx context.Context, commit CheckpointMessageCommit) error {
	return e.callCheckpoint("commit message", func() error { return e.checkpoint.hooks.StreamHooks.Commit(ctx, commit) })
}

func (s *managedStream[T]) checkpointFinish(err error) (CheckpointStreamFinish, bool) {
	if s.checkpoint == nil {
		return CheckpointStreamFinish{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	terminal := StreamCompleted
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		terminal = StreamCanceled
	case err != nil && !isSkipped(err):
		terminal = StreamFailed
	case s.stopped:
		terminal = StreamStopped
	}
	checkpoint := s.checkpoint
	return CheckpointStreamFinish{Site: checkpoint.exec.site, Attempt: checkpoint.descriptor.Attempt,
		Stream: checkpoint.descriptor.ID, Terminal: terminal, LastSequence: s.last, Cursor: checkpoint.cursor}, true
}

func checkpointFinishes(rc *RunCtx, err error) []CheckpointStreamFinish {
	if rc.streams == nil {
		return nil
	}
	rc.streams.mu.Lock()
	outputs := append([]streamLifecycle(nil), rc.streams.outputs...)
	rc.streams.mu.Unlock()
	var records []CheckpointStreamFinish
	for _, output := range outputs {
		if stream, ok := output.(interface {
			checkpointFinish(error) (CheckpointStreamFinish, bool)
		}); ok {
			if finish, present := stream.checkpointFinish(err); present {
				finish.Site = rc.exec.site // 缓存可能由历史别名创建，写端归属始终取实际拥有者。
				records = append(records, finish)
			}
		}
	}
	return records
}

// checkpointChainResult 先确定完整链及读取义务的结论，不开放写端终态。
// finishStreams 在可靠确认后做本地收尾；重复读取退出检查按端点幂等。
func checkpointChainResult(rc *RunCtx, err error) error {
	if rc.streams == nil {
		return err
	}
	rc.streams.mu.Lock()
	messageErr := rc.streams.messageErr
	outputs := append([]streamLifecycle(nil), rc.streams.outputs...)
	rc.streams.mu.Unlock()
	if err == nil || isSkipped(err) {
		if messageErr != nil {
			err = messageErr
		}
		for _, output := range outputs {
			if cause := output.failure(); cause != nil && (err == nil || isSkipped(err)) {
				err = cause
			}
		}
	}
	if !rc.node.sub {
		seen := make(map[managedReaderLifecycle]bool)
		for _, ref := range rc.node.requires {
			state, value := rc.exec.slotOf(ref).snapshot()
			reader, ok := value.(managedReaderLifecycle)
			if state == slotReady && ok && !seen[reader] {
				seen[reader] = true
				if cause := reader.pulseReaderFinished(rc, err); cause != nil && (err == nil || isSkipped(err)) {
					err = cause
				}
			}
		}
	}
	return err
}

// checkpointMessage 是 HandleMessage 一次持久尝试的状态视图，不与节点 Retry 共用预算。
type checkpointMessage struct {
	attempt CheckpointAttempt
	state   map[string]CheckpointState
}

func (rc *RunCtx) openCheckpointMessage(input CheckpointStreamAcknowledgement, limit int) (*checkpointMessage, error) {
	if rc.exec.checkpoint == nil {
		return nil, nil
	}
	var result CheckpointMessageAttempt
	if err := rc.exec.callCheckpoint("open message attempt", func() error {
		var err error
		result, err = rc.exec.checkpoint.hooks.StreamHooks.OpenMessage(rc.ctx, CheckpointMessageAttemptRequest{
			Site: rc.exec.site, Scope: rc.exec.checkpoint.scope, Attempt: rc.checkpointAttempt.Scope, NodeID: rc.NodeID(), Input: input, Limit: limit})
		if err != nil {
			return err
		}
		a := result.Attempt
		if a.Scope == "" || a.Scope == rc.checkpointAttempt.Scope || a.Index < 1 || a.Index > limit || (a.Exhausted && a.Index != limit) || len(a.Values) != 0 {
			return fmt.Errorf("invalid persistent message attempt cursor")
		}
		return checkCheckpointState(result.State)
	}); err != nil {
		return nil, err
	}
	message := &checkpointMessage{attempt: result.Attempt, state: make(map[string]CheckpointState, len(result.State))}
	for _, state := range result.State {
		value, _ := valueForType(reflect.ValueOf(state.Value), state.Type)
		state.Value = value.Interface()
		message.state[state.Name] = state
	}
	return message, nil
}

func (rc *RunCtx) failCheckpointMessage(message *checkpointMessage) error {
	if message == nil {
		return nil
	}
	return rc.exec.callCheckpoint("fail message attempt", func() error {
		return rc.exec.checkpoint.hooks.StreamHooks.FailMessage(rc.exec.ctx, message.attempt.Scope)
	})
}

func checkCheckpointState(states []CheckpointState) error {
	seen := make(map[string]bool, len(states))
	for _, state := range states {
		if state.Name == "" || seen[state.Name] {
			return fmt.Errorf("checkpoint state has an empty or duplicate name")
		}
		seen[state.Name] = true
		if err := checkCheckpointPayload(state.Type, state.Value); err != nil {
			return err
		}
	}
	return nil
}

// GetMessageState 读取 OpenMessage 认回的显式状态快照，仅可在持久 HandleMessage 回调中使用。
// 引用不深拷贝；需要改动时构造新值并调用 SetMessageState，失败尝试不保存此快照。
func GetMessageState[T any](rc *RunCtx, name string) (value T, present bool, err error) {
	if rc == nil || rc.message == nil || rc.message.checkpoint == nil || !rc.message.active {
		return value, false, fmt.Errorf("pulse: GetMessageState requires an active checkpoint message attempt")
	}
	state, present := rc.message.checkpoint.state[name]
	if !present {
		return value, false, nil
	}
	if state.Type != reflect.TypeFor[T]() {
		return value, false, rc.message.fail(fmt.Errorf("pulse: message state %q type mismatch", name))
	}
	if state.Value != nil {
		value = state.Value.(T)
	}
	return value, true, nil
}

// SetMessageState 显式替换本次消息尝试的一项状态；完整状态快照和输入确认、输出共同提交。
func SetMessageState[T any](rc *RunCtx, name string, value T) error {
	if rc == nil || rc.message == nil || rc.message.checkpoint == nil || !rc.message.active {
		return fmt.Errorf("pulse: SetMessageState requires an active checkpoint message attempt")
	}
	state := CheckpointState{Name: name, Type: reflect.TypeFor[T](), Value: value}
	if err := checkCheckpointState([]CheckpointState{state}); err != nil {
		return rc.message.fail(err)
	}
	if previous, present := rc.message.checkpoint.state[name]; present && previous.Type != state.Type {
		return rc.message.fail(fmt.Errorf("pulse: message state %q type mismatch", name))
	}
	rc.message.checkpoint.state[name] = state
	return nil
}

// checkpointCommit 只做可靠确认，调用方先完成容量/槽位/当前交付预检，且不持流锁；
// 必须防止同一交付的 Nack/Stop 插入确认和本地发布之间。失败不发布、不 Ack。
func (m *messageAttempt) checkpointCommit(rc *RunCtx, input CheckpointStreamAcknowledgement, outputs []CheckpointStreamAppend) error {
	if m.checkpoint == nil {
		return nil
	}
	commit := CheckpointMessageCommit{Site: rc.exec.site, Scope: rc.exec.checkpoint.scope,
		Attempt: m.checkpoint.attempt.Scope, Inputs: []CheckpointStreamAcknowledgement{input}, Outputs: outputs}
	for _, state := range m.checkpoint.state {
		commit.State = append(commit.State, state)
	}
	sort.Slice(commit.State, func(i, j int) bool { return commit.State[i].Name < commit.State[j].Name })
	m.writes.mu.Lock()
	var entries []stagedWrite
	for _, entry := range m.writes.staged {
		entries = append(entries, *entry)
	}
	m.writes.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ref.name < entries[j].ref.name })
	for _, entry := range entries {
		state, value := entry.slot.snapshot()
		if state == slotPending || rc.checkpointValueConfirmed(entry.ref.name) {
			continue
		}
		// 同状态合并是幂等首写：真正生效的值可能早已在完整节点帧中，
		// 不能把本消息里稍后的写保存为来源。相反状态已由整批预检拒绝。
		if target := m.writes.outer.peek(entry.ref); target != nil {
			if previous, first := target.snapshot(); previous != slotPending {
				state, value = previous, first
			}
		}
		output := checkpointOutput(entry.ref, state, value)
		if err := checkCheckpointOutput(output); err != nil {
			return rc.exec.stopCheckpoint("finalize message value", err)
		}
		commit.Values = append(commit.Values, output)
	}
	for _, output := range outputs {
		if err := checkCheckpointCursor(output.Cursor); err != nil {
			return rc.exec.stopCheckpoint("finalize message output", err)
		}
		if err := checkCheckpointCursor(output.Message.Value); err != nil {
			return rc.exec.stopCheckpoint("finalize message output", err)
		}
	}
	if err := rc.exec.commitCheckpointMessage(rc.ctx, commit); err != nil {
		return err
	}
	rc.confirmCheckpointValues(commit.Values)
	return nil
}

// checkpointAppends 在本地流锁下取序号/身份快照，不调用宿主代码；业务值不深复制。
func (b *messageOutput[T]) checkpointAppends(site Site) []CheckpointStreamAppend {
	if b.ignoreStopped && b.stream.stopped {
		return nil
	}
	checkpoint := b.stream.checkpoint
	if checkpoint == nil {
		return nil
	}
	out := make([]CheckpointStreamAppend, len(b.values))
	for i, value := range b.values {
		out[i] = CheckpointStreamAppend{Site: site, Attempt: checkpoint.descriptor.Attempt, Stream: checkpoint.descriptor.ID,
			Message: CheckpointStreamMessage{Sequence: b.stream.last + uint64(i) + 1, Value: value}}
	}
	return out
}
