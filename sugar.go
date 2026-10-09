package pulse

import (
	"fmt"
	"strconv"
)

// Batch 是 fan-in 读到的一束同类型输入：**到了值的** + **以跳过到达的**。
//
// 两条切片都按 Join 的 `ins` **声明顺序**排：`Missing` 给的是输入名，所以
// 「第 k 条值到底来自哪个 Key」用 `ins` 与 `Missing` 一对就还原得出来
// （`Values` 本身不带名字——同类型槽位的顺序编译期锁不住，声明顺序即语义顺序）。
//
// 「缺项在类型上可见」是刻意的：只给一束值会让宿主分不清「这一路没值」和
// 「这一路本来就不在」，而那正是 fan-in 最需要分辨的一件事（#265 的口径）。
// 缺项不是失败——它到达了，只是值为空；要「缺一条就别跑我」就显式
// `return b.WaitAll()`。
type Batch[T any] struct {
	// Values 按声明顺序列出到达了值的输入（缺项占的位次直接跳过）。
	Values []T
	// Missing 按声明顺序列出**以跳过到达**的输入名（即 Key.Name()）。
	Missing []string
}

// Len 返回到达值的条数（不含缺项）。
func (b Batch[T]) Len() int { return len(b.Values) }

// WaitAll 是**显式的严格 fan-in 声明**：有缺项就返回 *SkipError
// （errors.Is(err, ErrSkipped) 成立）。直接把它 return 出去，本节点就以跳过
// 收尾（尚未发布的输出被跳过，已发布的槽位不回滚）——与 pulse.WaitAll 同口径，
// 只是这里的「谁没值」已经攒好了。
//
// 判据只看 Missing：「还没到达」不该出现在 Batch 里——引擎的门没等到全部输入
// 到达（就绪或跳过）不会放 fn 进来，所以这里不会把「还没到」误报成缺项。
//
// 默认策略相反：到几个收几个，缺项拦不住节点。
func (b Batch[T]) WaitAll() error {
	if len(b.Missing) > 0 {
		return skipErr(b.Missing...)
	}
	return nil
}

// Keys 把若干**同类型** Key 收成一条声明，供 Join / Spread 使用。
// 元素类型由 Go 推断并锁定；不同元素类型的多路输入用 Deps + NewNode。
func Keys[T any](ks ...Key[T]) []Key[T] { return ks }

// NoValue 是「这次调用没有值」的显式结果：把它当 error 返回，**整个节点以跳过
// 收尾**——不是失败、不取消整图，它的输出槽随之跳过（到达了，值为空），下游的
// Join 会在 Batch.Missing 里看见它。
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
//   - 缺项在类型上可见（Batch.Missing），要严格就 `return b.WaitAll()`；
//   - fn 返回的 error 就是节点错误：首错取消整图，不会被改写成跳过；
//   - fn 返回 NoValue()（或任何 *SkipError）→ 本节点以跳过收尾，不是失败。
//
// 编译期锁住的是**元素类型**（ins 与 fn 的参数是同一个 T）；元素个数是运行期
// 长度，不参与类型检查。
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

// Spread 是 fan-out 的语法糖：**同一个输入**喂给 N 个并行实例，每个实例写自己
// 的那条输出槽（节点名依次为 `id-1` … `id-N`）。
//
// fn 拿到的运行上下文（取消看 rc.Context()，与手写节点同一个）、分片号
// （1 … N，与节点名后缀一致）与那条输入的值。N 个实例看到的是**同一条输入**，
// 它不切分数据：要「各做一份」得靠 fn 自己按分片号挑。N 是**装配期固定**的——
// 引擎的拓扑不随数据变；数据条数不定的并行请在节点内部做业务循环，别指望运行
// 时长出节点。
//
// 装配是**整批原子**的：N 个 worker 一次校验、一次提交，任一装不进去（输出槽
// 已有来源、节点名撞车…）就整个 Spread 失败，图上**不留这一批的任何痕迹**——
// 不会出现「函数返回了错误，图里却跑着半个 fan-out」。
//
// 语义与手写 N 个 NewNode **完全一致**：
//
//   - 实例各自一个节点、各自 goroutine（并发上限仍由 WithMaxRunning 管）；
//   - 某个实例返回 NoValue() → **它整个节点以跳过收尾**，不是失败；下游 Join
//     会在 Batch.Missing 里看见它（这正是「部分产出」的正常表达）；
//   - 任一实例返回 error → 首错取消整图（失败显式，糖不吞错）；
//   - 输入本身以跳过到达 → 引擎的门让每个实例都不执行（级联跳过）。
//
// 编译期锁住的是**元素类型**（in / outs / fn 的参数与返回是同一组 I、O）；
// 实例个数是运行期长度。
func Spread[I, O any](g *Graph, id string, in Key[I], outs []Key[O],
	fn func(rc *RunCtx, shard int, in I) (O, error), aspects ...Aspect) error {
	if g == nil {
		return fmt.Errorf("pulse: Spread %q: nil graph", id)
	}
	if id == "" {
		// 空 id 必须在拼后缀**之前**拦住：放行的话会造出叫 "-1" / "-2" 的节点，
		// 观测里既看不出它是谁的分片，也和白名单式命名对不上。
		return fmt.Errorf("pulse: Spread: empty node id")
	}
	if len(outs) == 0 {
		return fmt.Errorf("pulse: Spread %q: no outputs", id)
	}
	if fn == nil {
		return fmt.Errorf("pulse: Spread %q: nil fn", id)
	}
	workers := make([]*Node, len(outs))
	for i, out := range outs {
		shard := i + 1
		worker := id + "-" + strconv.Itoa(shard)
		workers[i] = NewNode(worker, Requires(in), Provides(out), func(rc *RunCtx) error {
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
	}
	if err := g.addAll(workers); err != nil {
		return fmt.Errorf("pulse: Spread %q: %w", id, err)
	}
	return nil
}

// collectBatch 读一束输入。门保证进到这里时每条输入都已到达，所以 TryGet
// 只会给出「就绪」或「跳过」两种结果；pending 走到这里是引擎的门出了问题，
// 报出来而不是当成缺项（缺项是「到达了、值为空」，语义不同）。
func collectBatch[T any](rc *RunCtx, ins []Key[T]) (Batch[T], error) {
	b := Batch[T]{Values: make([]T, 0, len(ins)), Missing: make([]string, 0)}
	for _, k := range ins {
		v, ok, skipped, err := TryGet(rc, k)
		if err != nil {
			return b, err
		}
		switch {
		case ok:
			b.Values = append(b.Values, v)
		case skipped:
			b.Missing = append(b.Missing, k.Name())
		default:
			return b, fmt.Errorf("pulse: Join: key %q has not arrived", k.Name())
		}
	}
	return b, nil
}
