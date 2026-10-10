package pulse

import (
	"fmt"
	"strconv"
)

// BatchItem 是 fan-in 的一路输入：**来自哪条 Key** + 它带来了什么。
//
// 只给值、不给来源时，「第 k 条值出自哪个槽」只能靠声明顺序去数——同类型多槽位
// 的顺序编译期锁不住（见 `Join` 的边界），值又长得像的时候数错也看不出来。
// 带上来源名以后，调用方可以**按来源 Key 取**（`Batch.Get`），不必记住 `ins` 的次序。
type BatchItem[T any] struct {
	// Key 是来源 Key 名（Key.Name()）。
	Key string
	// Value 是这一路带来的值；Present 为 false 时是 T 的零值。
	Value T
	// Present 为 false 表示这一路**以跳过到达**（到达了，只是没有值）。
	Present bool
}

// Batch 是 fan-in 读到的一束同类型输入：**每一条声明都在**，谁带了值、谁以跳过
// 到达一目了然。`Items` 按 `Join` 的 `ins` 声明顺序排。
//
// 「缺项在类型上可见」是刻意的：只给一束值会让宿主分不清「这一路没值」和
// 「这一路本来就不在」，而那正是 fan-in 最需要分辨的一件事（#265 的口径）。
// 缺项不是失败——它到达了，只是值为空；要「缺一条就别跑我」就显式
// `return b.WaitAll()`。
type Batch[T any] struct {
	// Items 按声明顺序列出**每一条**输入：缺项也在，Present=false。
	Items []BatchItem[T]
}

// Len 返回到达值的条数（不含缺项）。
func (b Batch[T]) Len() int {
	n := 0
	for _, it := range b.Items {
		if it.Present {
			n++
		}
	}
	return n
}

// Values 按声明顺序返回到达值的那些值（**每次调用新建切片**，改它不影响 Batch）。
func (b Batch[T]) Values() []T {
	out := make([]T, 0, len(b.Items))
	for _, it := range b.Items {
		if it.Present {
			out = append(out, it.Value)
		}
	}
	return out
}

// Missing 按声明顺序返回**以跳过到达**的输入名（Key.Name()）。
func (b Batch[T]) Missing() []string {
	out := make([]string, 0, len(b.Items))
	for _, it := range b.Items {
		if !it.Present {
			out = append(out, it.Key)
		}
	}
	return out
}

// Get 按**来源 Key** 取这一路的值。三种结果分得很开，为的就是不让「调错了」混进
// 「这一路没值」：
//
//   - 带值到达 → (值, nil)；
//   - **以跳过到达** → (零值, *SkipError)（errors.Is(err, ErrSkipped) 成立）：
//     跳过是到达，与 pulse.Get 同口径——直接 `return` 出去，本节点就以跳过收尾
//     （单路版的 WaitAll）；
//   - **不在这张清单里** → ErrUndeclared：传了一条本节点没声明的 Key，是写错了，
//     不是「这一路没值」。
//
// 收 `Key[T]` 而不是名字：调用方手里本来就有那个 Key 对象，复用它就不必再写一遍
// 名字（字符串写错一个字母只会静默变成零值），类型对不上编译期直接红。
func (b Batch[T]) Get(k Key[T]) (T, error) {
	var zero T
	for _, it := range b.Items {
		if it.Key != k.Name() {
			continue
		}
		if !it.Present {
			return zero, skipErr(k.Name())
		}
		return it.Value, nil
	}
	return zero, fmt.Errorf("%w: %s is not part of this batch", ErrUndeclared, k.asRef())
}

// WaitAll 是**显式的严格 fan-in 声明**：有缺项就返回 *SkipError
// （errors.Is(err, ErrSkipped) 成立）。直接把它 return 出去，本节点就以跳过
// 收尾（尚未发布的输出被跳过，已发布的槽位不回滚）——与 pulse.WaitAll 同口径，
// 只是这里的「谁没值」已经攒好了。
//
// 判据只看缺项：「还没到达」不会出现在 Batch 里——引擎的门没等到全部输入到达
// （就绪或跳过）不会放 fn 进来，所以这里不会把「还没到」误报成缺项。
//
// 默认策略相反：到几个收几个，缺项拦不住节点。
func (b Batch[T]) WaitAll() error {
	if m := b.Missing(); len(m) > 0 {
		return skipErr(m...)
	}
	return nil
}

// Keys 把若干**同类型** Key 收成一条声明，供 Join / FanOut 使用。
// 元素类型由 Go 推断并锁定；不同元素类型的多路输入用 Deps + NewNode。
func Keys[T any](ks ...Key[T]) []Key[T] { return ks }

// NoValue 是「这次调用没有值」的显式结果：把它当 error 返回，**整个节点以跳过
// 收尾**——不是失败、不取消整图，它的输出槽随之跳过（到达了，值为空），下游的
// Join 会在 `Batch.Missing()` 里看见它。
//
// 它**不等于**「跳过某一条输出」：NewNode 里手写的 `Skip(rc, key)` 只把那一条
// 输出槽标成跳过，节点自己照常返回 nil、终态是 completed；NoValue 是**节点级**
// 的终态声明（一条 *SkipError，errors.Is(err, ErrSkipped) 成立），fn 一返回它，
// 本节点就以 skipped 结束——已发布的输出不回滚，没发布的输出全部跳过。要
// 「只跳一条、其余照发」就用 Skip。
//
// keys 传了就进 SkipError.Keys（诊断与观测里列出「哪几条没值」），不传则以
// 空名单收尾。
func NoValue(keys ...string) error { return skipErr(keys...) }

// Join 是 fan-in 的语法糖：把 N 条**同类型**输入收成一束交给 fn。
//
// fn 拿到运行上下文（取消看 rc.Context()，与手写节点同一个）与那束 Batch。
// 语义与手写 NewNode **完全一致**（糖不改语义，只把名字收进签名）：
//
//   - 门是引擎默认的「到几个收几个」：只要有一条输入到了值，fn 就带着 Batch
//     进入执行；**全部输入都跳过时 fn 不执行**，本节点自己跳过；
//   - 缺项在类型上可见（`Batch.Missing()`），要严格就 `return b.WaitAll()`；
//   - fn 返回的 error 就是节点错误：首错取消整图，不会被改写成跳过；
//   - fn 返回 NoValue()（或任何 *SkipError）→ 本节点以跳过收尾，不是失败。
//
// 编译期锁住的是**元素类型**（ins 与 fn 的参数是同一个 T）；元素个数是运行期
// 长度，不参与类型检查。
//
// **同类型多槽位的顺序锁不住**：`Keys(a, b)` 与 `Keys(b, a)` 都编译（a、b 都是
// `Key[string]` 时谁也拦不住）。边界如此，但后果有兜底——`Batch.Get(a)` 按 Key
// 对象取，传错类型编译期就红、写错名字也轮不到；要按位置读，声明顺序即语义顺序。
//
// 严格有两种粒度：`return b.WaitAll()` 是「缺一条就别跑我」；`v, err := b.Get(a)`
// 之后把 err 返回出去是「这一条缺了就别跑我」。
func Join[T, O any](g *Graph, id string, ins []Key[T], out Key[O],
	fn func(rc *RunCtx, b Batch[T]) (O, error), aspects ...Aspect) error {
	if g == nil {
		return fmt.Errorf("pulse: Join %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Join: empty node id")
	}
	if len(ins) == 0 {
		return fmt.Errorf("pulse: Join %q: no inputs", id)
	}
	if fn == nil {
		return fmt.Errorf("pulse: Join %q: nil fn", id)
	}
	requires := make([]keyRef, len(ins))
	for i, k := range ins {
		requires[i] = k.asRef()
	}
	err := g.Add(NewNode(id, requires, Provides(out), func(rc *RunCtx) error {
		b, err := collectBatch(rc, ins)
		if err != nil {
			return err
		}
		v, err := fn(rc, b)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
	}, aspects...))
	if err != nil {
		return fmt.Errorf("pulse: Join %q: %w", id, err)
	}
	return nil
}

// FanOut 是 fan-out 的语法糖：**同一个输入**喂给 N 个并行实例，每个实例写自己
// 的那条输出槽（节点名依次为 `id-1` … `id-N`）。
//
// 名字叫 FanOut 而不是 Spread，是因为它**不切分数据**：N 个实例看到的是同一条
// 输入，要「各做一份」得靠 fn 自己按分片号挑。fn 拿到运行上下文（取消看
// rc.Context()，与手写节点同一个）、分片号（1 … N，与节点名后缀一致）与那条
// 输入的值。
//
// N 是**装配期固定**的——引擎的拓扑不随数据变；数据条数不定的并行请在节点内部
// 做业务循环，别指望运行时长出节点。
//
// 装配是**整批原子**的：N 个 worker 一次校验、一次提交，任一装不进去（输出槽
// 已有来源、节点名撞车…）就整个 FanOut 失败，图上**不留这一批的任何痕迹**——
// 不会出现「函数返回了错误，图里却跑着半个 fan-out」。
//
// 语义与手写 N 个 NewNode **完全一致**：
//
//   - 实例各自一个节点、各自 goroutine（并发上限仍由 WithMaxRunning 管）；
//   - 某个实例返回 NoValue() → **它整个节点以跳过收尾**，不是失败；下游 Join
//     会在 `Batch.Missing()` 里看见它（这正是「部分产出」的正常表达）；
//   - 任一实例返回 error → 首错取消整图（失败显式，糖不吞错）；
//   - 输入本身以跳过到达 → 引擎的门让每个实例都不执行（级联跳过）。
//
// 编译期锁住的是**元素类型**（in / outs / fn 的参数与返回是同一组 I、O）；
// 实例个数是运行期长度。
func FanOut[I, O any](g *Graph, id string, in Key[I], outs []Key[O],
	fn func(rc *RunCtx, shard int, in I) (O, error), aspects ...Aspect) error {
	if g == nil {
		return fmt.Errorf("pulse: FanOut %q: nil graph", id)
	}
	if id == "" {
		// 空 id 必须在拼后缀**之前**拦住：放行的话会造出叫 "-1" / "-2" 的节点，
		// 观测里既看不出它是谁的分片，也和白名单式命名对不上。
		return fmt.Errorf("pulse: FanOut: empty node id")
	}
	if len(outs) == 0 {
		return fmt.Errorf("pulse: FanOut %q: no outputs", id)
	}
	if fn == nil {
		return fmt.Errorf("pulse: FanOut %q: nil fn", id)
	}
	workers := make([]*Node, len(outs))
	for i, out := range outs {
		shard := i + 1
		worker := id + "-" + strconv.Itoa(shard)
		n := NewNode(worker, Requires(in), Provides(out), func(rc *RunCtx) error {
			// 门已经等到输入到达（就绪或跳过）：跳过的那条在这里读回来是
			// *SkipError，本实例随之以跳过收尾，与手写节点一模一样。
			v, err := Get(rc, in)
			if err != nil {
				return err
			}
			got, err := fn(rc, shard, v)
			if err != nil {
				return err
			}
			return Set(rc, out, got)
		}, aspects...)
		// 同组 worker 共读一条流出口是显式的「抢」：Start 的流式校验据此把
		// 「一组 FanOut」与「两个各写各的消费者」分开，见 checkStreamLocked。
		n.compete = id
		workers[i] = n
	}
	if err := g.addAll(workers); err != nil {
		return fmt.Errorf("pulse: FanOut %q: %w", id, err)
	}
	return nil
}

// collectBatch 读一束输入。门保证进到这里时每条输入都已到达，所以 TryGet
// 只会给出「就绪」或「跳过」两种结果；pending 走到这里是引擎的门出了问题，
// 报出来而不是当成缺项（缺项是「到达了、值为空」，语义不同）。
func collectBatch[T any](rc *RunCtx, ins []Key[T]) (Batch[T], error) {
	b := Batch[T]{Items: make([]BatchItem[T], 0, len(ins))}
	for _, k := range ins {
		v, ok, skipped, err := TryGet(rc, k)
		if err != nil {
			return b, err
		}
		switch {
		case ok:
			b.Items = append(b.Items, BatchItem[T]{Key: k.Name(), Value: v, Present: true})
		case skipped:
			b.Items = append(b.Items, BatchItem[T]{Key: k.Name()})
		default:
			return b, fmt.Errorf("pulse: Join: key %q has not arrived", k.Name())
		}
	}
	return b, nil
}

// Only 是排他分支的一句话写法：写这一条，本节点其余 Provides 全部作废。
//
// 它**不判断任何条件**——走哪条仍然是调用方 if 出来的。Only 负责把「我走这条，
// 别的作废」压成一次表态，消灭的是**漏表态**这一类写错：手写分支要写 1 次 Set
// 加 N−1 次 Skip，少写一边是**静默的**——只 Skip(B)、忘了 Set(A) 时，A、B 两条
// 下游都不跑（未写的 Provide 被自动跳过），而整轮仍返回 nil、没有任何提示。
// 用 Only 的调用方不再自己写 Skip，「漏表态」在形态上就不存在了。
//
// 另一面：重复表态会**变吵**。若先 Set 过别的出口再 Only，Only 会给那条已就绪
// 的槽补一次 Skip，直接 `ErrConflict`；而手写 Set 两次是被静默忽略的。所以
// Only 要**代替** Set，不要与之并用——需要同时写出多个值的节点继续手写 Set。
func Only[T any](rc *RunCtx, k Key[T], v T) error {
	if rc == nil || rc.node == nil {
		return fmt.Errorf("pulse: Only: nil run context")
	}
	want := k.asRef().name
	// 先预检、再发布：sibling 里已经有**就绪**的槽就当场冲突，别先把 k 发出去、
	// 再在补 Skip 时才失败——那会让本节点在这次错误之前多发布一条出口，下游甚至
	// 可能已经被唤醒（引擎不回滚已发布的值）。
	// 跳过过的 sibling 不算冲突：Skip 幂等，补一次是 no-op。
	for _, p := range rc.node.provides {
		if p.name != want && rc.g.slotOf(p).isReady() {
			return ErrConflict
		}
	}
	if err := Set(rc, k, v); err != nil {
		return err
	}
	for _, p := range rc.node.provides {
		if p.name == want {
			continue
		}
		if err := skipRef(rc, p); err != nil {
			return err
		}
	}
	return nil
}
