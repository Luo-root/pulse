package pulse

import (
	"fmt"
	"reflect"
)

// Port 是一条**端口声明**：这张图对外接受 / 产出的那条 Key。方向由声明它的方法决定
// （`DeclareInput` 是入口、`DeclareOutput` 是出口），所以端口本身不记方向——同一条键
// 可以既是入口又是出口（透传）。
//
// 用 `NewPort` 从已有的 `Key[T]` 造：名字与类型都从那条 Key 拿，**只写一遍**。写字符串
// 名字的那种形状（`DeclareInput("topic")`）要到装图期才能对出类型，写歪了只能等到那时。
//
// 端口是**静态**的：它只参与拓扑与静态校验，不携带任何一次运行的值——本次执行给什么值
// 走那一层的输入（今天还是 `Seed`，新模型里是 `Execution` 的输入）。
type Port struct {
	ref keyRef
}

// NewPort 从一条 `Key[T]` 造端口声明。
//
//	g.DeclareInput(pulse.NewPort(topic), pulse.NewPort(user))
//	g.DeclareOutput(pulse.NewPort(summary))
func NewPort[T any](k Key[T]) Port { return Port{ref: k.asRef()} }

// PortRef 是 `NewPort` 的**动态版**：从已解析的 key 记号造端口声明。
//
// 给声明式装配（`pulse/yaml`）用——那边子图的端口面是 `in` / `out` 两张映射表，
// 两端是 `{name, type}` 记号，编译期拿不到 `T`。与 `InRef` / `OutRef` 同一命名口径
// （`Ref` 后缀 = 收已解析的记号），手写装配仍然优先用 `NewPort`。
func PortRef(ref keyRef) Port { return Port{ref: ref} }

// Name 是这条端口声明的键名（与 `Key.Name()` 同一个名字）。
func (p Port) Name() string { return p.ref.name }

// String 是给人看的写法（与 `Key` 的 `String()` 同形）。
func (p Port) String() string { return p.ref.String() }

// declared 说这条端口声明是不是空的——零值 `Port{}` 什么都不指，声明它没有意义。
func (p Port) declared() bool { return p.ref.typ != nil }

// DeclareInput 声明「这张图接受外部喂这几条 Key」（入口）。
//
// 声明过之后，那几个 `Requires` **不再**算「无来源」：`Validate` 因此分得清「你忘了接线」
// 与「这本来就是外部输入」——前者是装配错，后者是这张图的接口。静态拓扑里它们的来源是
// `pulse.SourceInput`——定义层「外部来源」只有这一种写法：`Seed` 是运行期的动作，
// 值活在一次运行的 `Inputs` 里，不进定义的结构。
//
// 再声明一次同一条键是幂等的（同名同类型）；同名不同类型报错。**改不了定义的时机有两个**：
// 定义封口之后（`Validate` 通过过一次）与图跑起来之后（`Start` 成功）——两者都被拒，
// 共用 `ErrGraphStarted`，报文里分得出是哪一种（封口那句写着 sealed）。
//
// **一个键恰好一个来源**：这条键在图内已经有节点 `Provides` 时，声明成进口会被拒
// （`ErrDuplicateSource`）——那样的声明只会得到一个**用不了的入口**：喂值会撞「图内已有
// 来源」，不喂又等于白声明。反过来也拒：先声明进口，再 `Add` 一个产出它的节点，同样报
// （见 `Add`）。想要「子图自己有一个默认值」就在子图里放一个产出它的节点（那是产出，不是
// 外部来源）；想要「这一处挂载换个值」用挂载点的 `Fix` / `FixSkip`。
func (g *Graph) DeclareInput(ports ...Port) error { return g.declarePorts(ports, true) }

// DeclareOutput 声明「这张图对外产出这几条 Key」（出口）。
//
// 与 `DeclareInput` 对称：组合这张图时 `Out(子, 父)` 只能绑**声明过的出口**——绑到子图
// 内部的中间 Key（某个节点 `Provides` 但没声明成出口的）是静态错。想让父图用中间 Key 就
// 显式把它声明成出口，意图明确；不声明等于「这是我的实现细节，不承诺」。
func (g *Graph) DeclareOutput(ports ...Port) error { return g.declarePorts(ports, false) }

// declarePorts 是两段式：先全量校验（只读），全部通过才提交——半途失败不能在图里留下
// 改了一半的端口表（与 `addAll` 同一条纪律：失败那次不留痕迹）。
//
// 「全量」也包括**同一批内部**：`DeclareInput(NewPort(NewKey[string]("x")),
// NewPort(NewKey[int]("x")))` 的第一条会在提交段真的写进登记表，只查**旧**表的话第二条
// 要到提交时才炸——那时第一条已经进表了，all-or-nothing 成了半句（实测：失败之后再单独
// 声明一次 `int x`，报的还是「already registered as string」）。所以校验段带一张
// **本批次**的 name → 声明 表。
func (g *Graph) declarePorts(ports []Port, input bool) error {
	if len(ports) == 0 {
		return nil
	}
	what := "DeclareOutput"
	if input {
		what = "DeclareInput"
	}

	// 改定义：与 `Validate` / `Start` 的「判 + 封」共用装配锁，见 assemblyMu。
	assemblyMu.Lock()
	defer assemblyMu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()
	// 「已经开始过」不再单独判：某次运行 `Start()` 成功会顺带把整棵树封口，
	// 所以它一定落在 `sealed` 里；`errSealed` 的报文两个触发点都点了名。
	if g.sealed {
		return errSealed(what)
	}

	table := &g.outPorts
	other := &g.inPorts
	if input {
		table, other = &g.inPorts, &g.outPorts
	}

	// 校验段：只读。零值端口、键登记表冲突、**本批次内同名跨型**、与既有声明冲突，都在这段报完。
	batch := make(map[string]keyRef, len(ports))
	for _, p := range ports {
		if !p.declared() {
			return fmt.Errorf("pulse: %s: zero Port{}: build ports with NewPort(k)", what)
		}
		if err := g.keys.check(p.ref); err != nil {
			return err
		}
		if old, ok := batch[p.ref.name]; ok && old.typ != p.ref.typ {
			return fmt.Errorf("pulse: %s: key %q is declared twice in this call, as %s and as %s: "+
				"one key has exactly one type", what, p.ref.name, typeName(old.typ), typeName(p.ref.typ))
		}
		if old, ok := (*table)[p.ref.name]; ok && old.typ != p.ref.typ {
			return errPortType(what, p.ref, old)
		}
		if old, ok := (*other)[p.ref.name]; ok && old.typ != p.ref.typ {
			return errPortType(what, p.ref, old)
		}
		// 一个键恰好一个来源，而**节点产出**也是来源：这条键在图内已经有人产出时，声明成
		// 进口只会得到一个用不了的入口（喂值撞「图内已有来源」，不喂又等于白声明），所以
		// 装配期就拒。判据的另一面（先声明、后产出）在 `addAllLocked`，两条同一句话、
		// 都撞 `ErrDuplicateSource`。**出口**不受这条限制：同一个节点产出这条键正是出口
		// 想要的形状。
		if input {
			if prev, ok := g.producer[p.ref.name]; ok && prev.present() {
				return fmt.Errorf("pulse: %s: key %q is provided inside this graph by %s, so it "+
					"cannot also be declared as an input: a value fed from outside would be "+
					"silently ignored (%w)", what, p.ref.name, prev, ErrDuplicateSource)
			}
		}
		batch[p.ref.name] = p.ref
	}

	// 提交段。
	if *table == nil {
		*table = make(map[string]keyRef, len(ports))
	}
	for _, p := range ports {
		if err := g.keys.register(p.ref); err != nil {
			return err
		}
		(*table)[p.ref.name] = p.ref
	}
	return nil
}

// errPortType 说清「同一条键被声明成两个类型」：这类错直接决定接线对不上，所以要在声明处
// 就报，别留到绑定时才说「类型不符」。
func errPortType(what string, got, old keyRef) error {
	return fmt.Errorf("pulse: %s: key %q is already declared as %s, cannot declare it as %s",
		what, got.name, typeName(old.typ), typeName(got.typ))
}

// typeName 是报错里用的类型短名。
func typeName(t reflect.Type) string {
	if t == nil {
		return "unknown"
	}
	return t.String()
}

// errSealed 说清「定义已封口」这条：它是 `ErrGraphStarted` 的**第二个触发点**——
// 不是「这一次运行已经启动」（那是 `Start` 的判据），而是「这张**定义**不再变化」。
// 两者共用同一个哨兵（冻结面里七条哨兵不新增、不改名），语义分开写在各自的 godoc 与
// 迁移说明里。
//
// 句子里两个触发点都点出来：`Validate()` 通过过一次，或这张图已经启动过（`Start` 成功
// 会顺带封整棵树，见 Validate / Start）。单独看报文的人要能知道自己是踩了哪一个。
func errSealed(what string) error {
	return fmt.Errorf("pulse: %s: the graph definition is sealed: Validate() already passed "+
		"(or the graph has started, which seals the tree too), so the definition no longer "+
		"changes (declare and add before the first successful Validate, or build a new graph) (%w)",
		what, ErrGraphStarted)
}
