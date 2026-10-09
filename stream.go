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

// StreamOption 配置流式辅助件（`Produce` / `Tee`）创建 channel 的方式。
type StreamOption func(*streamConfig)

type streamConfig struct{ buf int }

// WithBuffer 让 `Produce` / `Tee` 创建的 channel 带缓冲（默认 0，即无缓冲）。
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
// 装、下游消费端可能是后加的）。真正的判据在 `Start`（见 Graph.checkStreamLocked）：
// 全部流节点要同时有名额，且每条流出口都得有人消费。两处一起才完整：这里给的是
// 即时反馈，那里才是权威校验。
func checkStreamSlots(g *Graph, id, kind string, need int) error {
	if g.maxRun > 0 && g.maxRun < need {
		return fmt.Errorf("pulse: %s %q: WithMaxRunning(%d) cannot host this stream: "+
			"%d nodes must be live at the same time", kind, id, g.maxRun, need)
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
//     （`WithMaxRunning ≥ 2`），`Start` 再按**整张图**复核——全部流节点要同时有
//     名额（实测：`WithMaxRunning(2)` 下 `Produce → Tee → Consume` 三个流节点
//     会装配全过、运行期死锁），且每条出口都得有人消费。代价是禁掉「小流 +
//     大缓冲」这类能凑合跑的配置——那种配置的成败取决于数据量，不该被依赖。
//
// 生产端不开切面：`Retry` 会撞上「已发布的槽」（幂等首写 → 第二次 attempt 直接
// `ErrConflict`），`Timeout` 与「节点活着发完」冲突（它只把流掐断，消费端看到的
// 只是「提前关闭」）。两个切面在这里都没有干净语义。
func Produce[T any](g *Graph, id string, out Key[<-chan T],
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
	n := NewNode(id, nil, Provides(out), func(rc *RunCtx) error {
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
	})
	n.streamKind = "produce"
	return g.Add(n)
}

// Consume 是流式消费的语法糖：循环、取消、提前退出都收进一次调用。
//
// 循环是 **select**（收到值 / `rc.Context().Done()`），**不是 `for range`**——
// 实测的差别不在「会不会挂住」（生产端守规矩时 channel 会被关掉，下游不挂），
// 而在取消之后：`for range` 会把缓冲区里剩下的值**继续算完**，并且整轮**报成功**
// （没有任何节点看见取消，`Run()` 返回 nil）；select 则在取消那一刻退出并返回
// 取消原因。
//
//   - fn 返回 error → 节点错误，首错取消整图。
//   - 输入以**跳过**到达（上游没 `Set` 那条 channel）→ 本节点随之跳过、fn 不执行：
//     跳过是到达，不是失败。
//   - channel 正常关闭后返回 nil 是合法的：流上的错误记在生产节点上。
//
// 消费端也不开切面：`Retry` 对流的语义说不通——流的位置不可回退，重试只会拿到
// 后半段。（要限时就把 ctx 交给调用方，或在上游节点上装 `Timeout`。）
func Consume[T any](g *Graph, id string, in Key[<-chan T],
	fn func(rc *RunCtx, v T) error) error {
	if g == nil {
		return fmt.Errorf("pulse: Consume %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Consume: empty node id")
	}
	if fn == nil {
		return fmt.Errorf("pulse: Consume %q: nil fn", id)
	}
	n := NewNode(id, Requires(in), nil, func(rc *RunCtx) error {
		ch, err := Get(rc, in)
		if err != nil {
			return err // 跳过到达 → 本节点跳过（级联），不是失败
		}
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
					return nil
				}
				if err := fn(rc, v); err != nil {
					return err
				}
			}
		}
	})
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
//     `WithMaxRunning ≥ N+1`，`Start` 再按整张图复核（同 `Produce`，含上游）。
//   - **每条出口都得有人要**：`outs` 里任何一条没有下游 `Requires`，`Start` 就
//     拒绝——发送端会永久堵在那条无缓冲出口上（缓冲只把死锁推到缓冲写满）。
//   - **空流**：一次都没发、全部出口正常关闭 → 下游零次回调、整轮成功（与
//     `Produce` 同一条口径：空流是「值到了」，不是「跳过」）。
//   - 一条出口只能给一个下游；要再分一层就再接一个 `Tee`。
func Tee[T any](g *Graph, id string, in Key[<-chan T], outs []Key[<-chan T],
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
	cfg := newStreamConfig(opts)
	n := NewNode(id, Requires(in), Provides(outs...), func(rc *RunCtx) error {
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
	})
	n.streamKind = "tee"
	return g.Add(n)
}
