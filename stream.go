package pulse

import "fmt"

// 流式辅助件：`Produce` / `Consume` / `Tee`。
//
// 为什么需要这三个：Pulse 已经能用 `Key[<-chan T]` 传 channel，缺的从来不是
// 「再包一层 channel」，而是每个生产 / 消费节点都要手动协调的六件事——channel
// 的创建与发布、发送端的关闭责任、生产 / 消费循环、对 `rc.Context()` 的取消
// 响应、错误怎么回传到节点与整图、背压与 `WithMaxRunning` 的交互。
//
// 判据仍然是「开发者究竟在哪里容易写错」：手写这套形状时最典型的一处是
// `Set` 出 channel 就 `return`，把真正的发送留给无人观测的后台 goroutine——
// 节点已经 completed，错误回不到图上、取消也叫不醒它，`defer close` 还经常漏。

// StreamOption 配置流式辅助件（`Produce` / `Consume` / `Tee`）。
type StreamOption func(*streamConfig)

type streamConfig struct {
	buf              int
	aspects          []Aspect
	checkpointSource CheckpointStreamSource
	messageOptions   []MessageOption
}

// WithStreamAspects 给本流节点追加切面；多次配置按声明顺序追加，先声明的在外层。
// 图级与执行级切面仍在节点级切面之外，与 NewNode 的组合规则相同。
//
// 创建选项时复制切片，每次应用独立追加，选项可以复用；不深拷贝切面闭包的业务状态。
// Timeout 等不暂存写入的切面可用；Produce / Tee 仍拒绝 Retry 等写暂存，
// Consume 的整节点 Retry 也不能倒带或重放已消费的元素。
func WithStreamAspects(aspects ...Aspect) StreamOption {
	aspects = append([]Aspect(nil), aspects...)
	return func(c *streamConfig) {
		c.aspects = append(append([]Aspect(nil), c.aspects...), aspects...)
	}
}

// WithBuffer 让 `Produce` / `Tee` 创建的 channel 带缓冲（默认 0，即无缓冲）。
// n <= 0 不改变配置；Consume 不创建发送缓冲，传正数会在装配时被拒绝。
//
// 缓冲**不是**「名额不够」的解药：一条流的两端必须同时活着，缓冲只是把「没人
// 来读」的阻塞推到「缓冲写满」的时刻——实测同一份代码，缓冲 8 / 发 3 个跑得通，
// 缓冲 2 / 发 5 个照样卡死。它真正买到的是**削峰**：生产端一时快过消费端时，
// 双方不必立刻互相等。
func WithBuffer(n int) StreamOption {
	return func(c *streamConfig) {
		if n > 0 {
			c.buf = n
		}
	}
}

func checkNativeStreamOptions(id string, cfg streamConfig) error {
	if cfg.checkpointSource != (CheckpointStreamSource{}) || len(cfg.messageOptions) != 0 {
		return fmt.Errorf("pulse: stream node %q: WithStreamSource and WithMessageHandling require a managed Stream endpoint; use ProduceStream, ConsumeStream or TeeStream", id)
	}
	return nil
}

func newStreamConfig(opts []StreamOption) streamConfig {
	var c streamConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c
}

// checkStreamSlots 是流式糖在**装配期**的快速失败：名额明显不够就别往下走，
// 早点报给调用方。
//
// 但它**证明不了组合图安全**——糖在装配那一刻看不全整张图（上游生产端可能还没
// 装、下游消费端可能是后加的）。真正的判据在 `Start`（见 Graph.problemsStreamLocked）：
// 整张流图（流节点 + 流出口的读取者）要同时有名额，且每条流出口都得有人消费、
// **只喂一个下游**。两处一起才完整：这里给的是即时反馈，那里才是权威校验。
//
// 子图里还看不到**继承来的**名额：额度继承发生在 `build` 返回之后（见 runSub 的
// 「5b) 名额」），所以那一层的这次检查读到的是「没声明 = 不设限」——等于没查。接
// 着子图自己的 `Start` 就会跑 `problemsStreamLocked`（那时继承已经写完），拒的是那一
// 条报文；这里只补时序，不为「早一点报」去动继承次序。
func checkStreamSlots(g *Graph, id, kind string, need int) error {
	if c := g.declaredCapacity(); c > 0 && c < need {
		return fmt.Errorf("pulse: %s %q: WithMaxRunning(%d) cannot host this stream: "+
			"%d nodes must be live at the same time", kind, id, c, need)
	}
	return nil
}

// Produce 是流式生产的语法糖：channel 的创建、发布、关闭与取消全收进一次调用，
// 节点**活着发完**——工作待在这次 `Run` 里，返回才算完成。
//
// send 不把 channel 交给调用方，关闭只发生在节点返回时的 `defer`：成功、出错、
// 取消、panic 都走它，所以不存在双次 close。send 本身在「发出去」与
// `rc.Context().Done()` 上 select，取消能从堵住的发送里出来。
//
//   - fn 返回的 error 就是节点错误，走现有的首错取消；不要另开 error channel。
//   - 忽略 `send` 返回的 error 等于不响应取消——与引擎里其它不看 ctx 的 `Run`
//     一样，糖不替调用方兜这个。
//   - **空流**（一次都没 send 就正常返回）：下游看到关闭、零次回调、整轮成功。
//     注意它走的是「值到了」这条路（槽位里放的是 channel），**不是跳过**；想要
//     「没有值」的语义就别 `Set` 那条 channel。
//   - **名额**：生产端与消费端必须同时活着。装配期先做一次快速检查
//     （`WithMaxRunning ≥ 2`），`Start` 再按**整张流图**复核——流节点与**每条
//     流出口的读取者**都要同时有名额（实测：`WithMaxRunning(2)` 下
//     `Produce → Tee → Consume` 装配全过、运行期死锁；把消费端换成手写的
//     `NewNode` 一样死锁——读取者同样得占一个名额才读得到），且每条出口都得有
//     人消费、**只喂一个下游**。代价是禁掉「小流 + 大缓冲」这类能凑合跑的
//     配置——那种配置的成败取决于数据量，不该被依赖。
//
// WithStreamAspects 可只为本节点配置 Timeout、计时或包装。Timeout 到期会取消
// 发送并等待内层退出；即使下游读到 channel 关闭，本轮仍是失败，不把截断当成功。
// **写入暂存类切面**（Retry 是典型）仍在运行期拒绝：channel 要等尝试提交才发布，
// 但消费者需要在生产者运行时拿到它，双方会互等（见 checkChannelVisibleNow）。
// 重试还会重开一条流，已经发出去的值不能收回。
//
// description 原样保留为节点说明，空串合法。
func Produce[T any](g *Graph, id, description string, out Key[<-chan T],
	fn func(rc *RunCtx, send func(T) error) error, opts ...StreamOption) error {
	if g == nil {
		return fmt.Errorf("pulse: Produce %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Produce: empty node id")
	}
	if fn == nil {
		return fmt.Errorf("pulse: Produce %q: nil fn", id)
	}
	if err := checkStreamSlots(g, id, "Produce", 2); err != nil {
		return err
	}
	cfg := newStreamConfig(opts)
	if err := checkNativeStreamOptions(id, cfg); err != nil {
		return err
	}
	n := NewNode(id, description, nil, Provides(out), func(rc *RunCtx) error {
		if err := checkChannelVisibleNow(rc, "Produce", id, out.asRef()); err != nil {
			return err
		}
		ch := make(chan T, cfg.buf)
		defer close(ch)
		if err := Set(rc, out, ch); err != nil {
			return err
		}
		return fn(rc, func(v T) error {
			select {
			case ch <- v:
				// 发成功了也要复查 ctx：「发得出去」与「已取消」同时就绪时 select
				// 随机挑（与 acquire 的复查同一条纪律）。少了这一眼，取消之后还会
				// 继续发，这一轮也可能把取消报成成功。
				return rc.Context().Err()
			case <-rc.Context().Done():
				return rc.Context().Err()
			}
		})
	}, cfg.aspects...)
	n.streamKind = "produce"
	return g.Add(n)
}

// checkChannelVisibleNow 挡住「把一条 channel 写进一次尝试的写暂存」——那条路一定跑不通，
// 而且要**当场报**，不能让它挂成一句看不出病因的超时。
//
// `Produce` / `Tee` 的形状都是**先 `Set` 一条 channel、再往它里面发值**：有尝试边界时那条
// channel 只有等这次尝试**提交之后**才发布，而消费端在拿到它之前只能等着——发送方与接收方
// 于是互等，整轮挂到 ctx 到期（实测：图级 `Retry` 套在 `Produce` 上，3s 到期时
// `produced=1, consumed_sum=0`）。
//
// 图级、执行级与节点级（WithStreamAspects）都可能带来写暂存。Retry 给每次尝试
// 各开一条写暂存；判据直接看「有没有写暂存」，不去认切面的身份：Aspect 是裸函数
// 类型，装配期认不出来。重试会重开一条流，而已经发出去的值收不回来。
func checkChannelVisibleNow(rc *RunCtx, sugar, nodeID string, out keyRef) error {
	if rc.writes == nil {
		return nil
	}
	return fmt.Errorf("pulse: %s %q: this stream node is running inside a per-attempt write set "+
		"(a Retry or another write-staging aspect is on its layer): the channel it writes (%q) "+
		"would only be published once that attempt commits, but its consumer needs the channel "+
		"while this node is still running — sender and receiver wait for each other and the graph "+
		"hangs until the ctx expires. Take the write-staging aspect off this layer (WithAspects / "+
		"WithExecAspects / WithStreamAspects / the document-level declaration): a stream producer cannot be retried — "+
		"a retry restarts the stream, and values already sent cannot be taken back",
		sugar, nodeID, out.name)
}

// Consume 是流式消费的语法糖：循环、取消、提前退出都收进一次调用。
//
// 循环是 **select**（收到值 / `rc.Context().Done()`），**不是 `for range`**——
// 实测的差别不在「会不会挂住」（生产端守规矩时 channel 会被关掉，下游不挂），
// 而在取消之后：`for range` 会把缓冲区里剩下的值**继续算完**，并且整轮**报成功**
// （没有任何节点看见取消，`Run()` 返回 nil）；select 则在取消那一刻退出并返回
// 取消原因。
//
//   - fn 返回真实 error → 节点错误，首错取消整图。
//   - fn 返回 NoValue / ErrSkipped → 不再调用 fn，继续按 Context 排空 channel，
//     正常关闭后本节点保持 skipped；不会让有限生产者因无人接收而挂住。
//     这表示整消费节点跳过，与 ConsumeStream 的单条消息跳过不同。
//   - 输入以**跳过**到达（上游没 `Set` 那条 channel）→ 本节点随之跳过、fn 不执行：
//     跳过是到达，不是失败。
//   - channel 正常关闭后返回 nil 是合法的：流上的错误记在生产节点上。
//
// WithStreamAspects 可只为本节点配置 Timeout、计时或包装，覆盖等输入与消费整段。
// Timeout 到期取消接收并等待回调退出，回调内的阻塞也应响应 rc.Context()。
// 整节点 Retry 不会被生产端的写暂存检查拒绝，但流的位置不可回退，重试只会继续读
// 后半段，不能重放已消费元素。Consume 不创建发送缓冲，正数 WithBuffer 在装配时拒绝。
//
// description 原样保留为节点说明，空串合法。
func Consume[T any](g *Graph, id, description string, in Key[<-chan T],
	fn func(rc *RunCtx, v T) error, opts ...StreamOption) error {
	if g == nil {
		return fmt.Errorf("pulse: Consume %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Consume: empty node id")
	}
	if fn == nil {
		return fmt.Errorf("pulse: Consume %q: nil fn", id)
	}
	cfg := newStreamConfig(opts)
	if err := checkNativeStreamOptions(id, cfg); err != nil {
		return err
	}
	if cfg.buf > 0 {
		return fmt.Errorf("pulse: Consume %q: WithBuffer(%d) is only supported by Produce / Tee; Consume does not create a channel", id, cfg.buf)
	}
	n := NewNode(id, description, Requires(in), nil, func(rc *RunCtx) error {
		ch, err := Get(rc, in)
		if err != nil {
			return err // 跳过到达 → 本节点跳过（级联），不是失败
		}
		var skipped error
		for {
			select {
			case <-rc.Context().Done():
				return rc.Context().Err()
			case v, ok := <-ch:
				// 进了 channel 分支先复查 ctx：两个分支同时就绪时 select 随机挑
				// （与 acquire 的复查同一条纪律）。少了这一眼，「channel 已关闭 +
				// 同刻被取消」会走成 `return nil`——把取消报成成功；有缓冲时还会
				// 在取消之后继续调 fn。
				if err := rc.Context().Err(); err != nil {
					return err
				}
				if !ok {
					return skipped
				}
				if skipped != nil {
					continue // 正常跳过后只排空，不再调用业务；原生发送端没有 Stop 协议。
				}
				if err := fn(rc, v); err != nil {
					if !isSkipped(err) {
						return err
					}
					skipped = err
				}
			}
		}
	}, cfg.aspects...)
	n.streamKind = "consume"
	return g.Add(n)
}

// Tee 是广播（复制）的语法糖：一根流复制给 N 个下游，每个下游都拿到**完整、
// 同序**的同一份数据。「广播到哪几个」= 显式写出来的 `outs` 列表，一条对应一个
// 下游；没列进去的接不到（它连数据来源都没有，`Start()` 就会拒）。
//
// 为什么「要复制」的只有流这一类（实测过四种分发形态）：
//
//   - 普通值：一个节点 `Provides`，N 个下游各自 `Requires` 同一个 Key —— 3 个
//     下游都拿到完整值（**本来就是广播**，不需要任何新件）；
//   - 一组值（slice 当一个值）：同上，3 个下游都拿到完整的 3 个元素；
//   - 一条 channel：同一个 Key 给 3 个下游，6 个值被**瓜分**（合计 6，不是 18）；
//   - 复制成 3 条 channel：3 个下游各读一条，合计 18 —— 这才是广播。
//
// 所以：「几个工人抢一条流、每个数据只做一次」用 `FanOut`，「每个下游都拿到
// 全部」用 `Tee`，两者不要互相替代。
//
//   - **慢下游会拖住快的**：Tee 依次往每条出口发，卡在最慢的那条上（背压按最慢
//     的算）。这是固有权衡——想让慢下游不拖累别人，得自己给它 `WithBuffer`，或
//     让它自己丢。
//   - **名额**：Tee 与它的 N 个下游要同时活着 ⇒ 装配期快速检查
//     `WithMaxRunning ≥ N+1`，`Start` 再按整张流图复核（同 `Produce`，含上游与
//     每个下游）。
//   - **每条出口都得有人要**：`outs` 里任何一条没有下游 `Requires`，`Start` 就
//     拒绝——发送端会永久堵在那条无缓冲出口上（缓冲只把死锁推到缓冲写满）。
//   - **空流**：一次都没发、全部出口正常关闭 → 下游零次回调、整轮成功（与
//     `Produce` 同一条口径：空流是「值到了」，不是「跳过」）。
//   - **一条出口只喂一个下游**：第二个读取者会在**静默**里和第一个瓜分值，而
//     Tee 承诺的是「每个下游拿到完整同序的数据」，所以 `Start` 直接拒。要再分
//     一层就再接一个 `Tee`；要「抢」（每个数据只做一次）就用 `FanOut`——同一组
//     worker 共读一条出口是显式声明，不算多消费者。
//   - **写入暂存类的切面（`Retry`）照拒**：`Tee` 同样是**先 `Set` 通道、再往里发**
//     的生产端，有尝试边界时通道只有等这次尝试提交才发布，与消费端互等（同
//     `Produce`，见 checkChannelVisibleNow）。这一条是运行期判的——判据是「有没有
//     写暂存」，因为 `Aspect` 是裸函数类型，装配期认不出哪个切面开了边界。
//
// WithStreamAspects 把切面接入本节点的同一条执行链；Timeout 覆盖等输入、接收与发送，
// 到期取消受管收发并等待内层退出，整轮保留失败。
//
// description 原样保留为节点说明，空串合法。outs 在装配时复制，声明与发送
// 共用这份内部接线；调用方后续修改切片不改变广播出口。
func Tee[T any](g *Graph, id, description string, in Key[<-chan T], outs []Key[<-chan T],
	opts ...StreamOption) error {
	if g == nil {
		return fmt.Errorf("pulse: Tee %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Tee: empty node id")
	}
	if len(outs) == 0 {
		return fmt.Errorf("pulse: Tee %q: no outputs", id)
	}
	if err := checkStreamSlots(g, id, "Tee", len(outs)+1); err != nil {
		return err
	}
	outs = append([]Key[<-chan T](nil), outs...)
	cfg := newStreamConfig(opts)
	if err := checkNativeStreamOptions(id, cfg); err != nil {
		return err
	}
	n := NewNode(id, description, Requires(in), Provides(outs...), func(rc *RunCtx) error {
		src, err := Get(rc, in)
		if err != nil {
			return err // 上游跳过 → 整批一起跳过（级联）
		}
		chans := make([]chan T, len(outs))
		for i := range chans {
			chans[i] = make(chan T, cfg.buf)
		}
		defer func() {
			for _, ch := range chans {
				close(ch)
			}
		}()
		for i, out := range outs {
			if err := checkChannelVisibleNow(rc, "Tee", id, out.asRef()); err != nil {
				return err
			}
			if err := Set(rc, out, chans[i]); err != nil {
				return err
			}
		}
		for {
			select {
			case <-rc.Context().Done():
				return rc.Context().Err()
			case v, ok := <-src:
				if err := rc.Context().Err(); err != nil { // 同 Consume：进了分支先复查
					return err
				}
				if !ok {
					return nil
				}
				for _, ch := range chans {
					select {
					case ch <- v:
						if err := rc.Context().Err(); err != nil {
							return err
						}
					case <-rc.Context().Done():
						return rc.Context().Err()
					}
				}
			}
		}
	}, cfg.aspects...)
	n.streamKind = "tee"
	return g.Add(n)
}
