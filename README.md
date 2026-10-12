[English](README_en.md) | [中文](README.md)

<div align="center">
  <a href="https://luo-root.github.io/pulse/">
    <img alt="Pulse" src=".github/assets/logo.svg" width="260" />
  </a>
</div>

<div align="center">
  <h3>Go 的一次性图引擎 —— 数据到达即调度，失败显式。</h3>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.25.0" src="https://img.shields.io/badge/Go-1.25.0-blue.svg" /></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg" /></a>
  <a href="https://github.com/Luo-root/pulse/releases/tag/v0.4.1"><img alt="Release v0.4.1" src="https://img.shields.io/badge/release-v0.4.1-2563eb.svg" /></a>
  <a href="https://luo-root.github.io/pulse/"><img alt="文档" src="https://img.shields.io/badge/docs-%E4%B8%AD%E6%96%87%20%7C%20English-2563eb.svg" /></a>
  <a href="docs/design/pulse.md"><img alt="设计文档" src="https://img.shields.io/badge/设计-pulse.md-2563eb.svg" /></a>
</div>

<br />

**Pulse** 是一个一次性运行的图引擎，外加一层图观测。节点只声明读哪些 Key、写哪些 Key，拓扑由数据的生产与消费隐式形成——没有边对象、没有「先排序再执行」的调度阶段、没有调度循环。

Pulse 只有三样东西：

| 包 | 是什么 | 依赖 |
|---|---|---|
| `pulse`（根） | 图引擎 | **零**（只用标准库） |
| `pulse/observe` | 图观测：把引擎的 `Observer` 回调折成结构化记录 | `pulse` |
| `pulse/yaml` | 声明式装图：YAML → 图 | `pulse` + `yaml.v3` |

引擎不 import 任何观测包，只暴露一个 `Observer` seam；不需要观测的宿主只 import 根包。

## 安装

```bash
go get github.com/Luo-root/pulse
```

## 快速上手

```go
package main

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse"
)

var Docs = pulse.NewKey[[]string]("docs")
var Summary = pulse.NewKey[string]("summary")

func main() {
	g, err := pulse.New(context.Background(), "demo", "演示图")
	if err != nil {
		panic(err)
	}
	_ = pulse.Seed(g, Docs, []string{"a", "b"})

	_ = g.Add(pulse.NewNode("summarize", "把 docs 折成一句摘要",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d docs", len(docs)))
		}))

	if err := g.Run(); err != nil {
		panic(err)
	}
	// Summary 已到达。
}
```

## 一屏看懂设计

- **数据到达即调度。** `Requires` 是 AND 前置：全部输入到达（就绪或跳过）才判门；**到几个收几个**——只要有一条输入真的到了值就执行 `Run`，一条值都没到才跳过自己。
- **槽位三态**：`pending` | `ready(值)` | `skipped`。就绪与跳过**都是到达**——跳过不是失败。分支靠对未选中的 `Provide` 调用 `Skip`（选中的那条照常 `Set`）写出；**两边都要表态**，只 Skip 一边、漏了选中的 `Set`，那条输出会被自动跳过，两条下游都不跑。
- **失败显式。** 节点 error 记录首错并取消整图，**不会**被改写成 `ErrSkipped`。
- **一次运行一个世界（对象模型正在迁移）。** 当前实现里 `Graph` 同时是「图的定义」与「一次运行」：`Start` 只能成功一次，复用同一份定义的正确做法是再 `New` 一次——类比 CI/CD 的 workflow 定义被 run 无数遍、每遍一个独立 run 实例；跨运行状态（历史、缓存、会话）归调用方，不归引擎。**目标模型**把两者拆开：`Graph` 是**可复用**的静态定义，运行状态搬到新的 `Execution`（同一张图可被顺序 / 并发执行多次，也可被多处挂载为子图）——正在按 [#311](https://github.com/Luo-root/pulse/issues/311) 分阶段落地，契约见 [`docs/design/object-model.md`](docs/design/object-model.md)。
- **切面包住「等输入 + 执行」整段**，所以 `Timeout` 能打断还在等数据的节点。**图级切面会继承给子图并往下叠**（继承的在外、子图自己声明的在内，`pulse.WithoutAspects()` 切断）——正因如此**图级切面慎用、尽量用节点级切面**：一个图级 `Timeout(d)` 会同时套在父图普通节点、挂子图那一步（= 整张子图）以及子图里的每个节点上，而**子图天生比普通节点更容易超时**。子图的总时间**先被挂子图那一步上的 `d` 掐死**，不会变成「每个节点各 `d`、加起来 `N*d`」；名额紧时，超时还可能发生在子图拿到名额之前——那一步不占名额，但那个 `d` 从它进入 `Run` 就开始计时。
- **图即节点：一张图可以当一步。** 唯一的公共入口是 **`pulse.Sub`**：把一张**现成的**子图在装配期挂成父图上的一个节点，父图**不运行**就能递归看完整棵子树（`Topology().Nodes[i].Sub`，路径按挂载位置推导），子图的静态毛病（无来源 / 出口没人产出 / 依赖环）也在装配期就报、报文点明**挂载位置**；**端口是唯一的接线面**——`DeclareInput` / `DeclareOutput` 声明入口与出口，接到子图**内部的中间 Key** 是静态错（出口还多一条：声明了却没人产出的出口，装配期就报，不必等子图跑完才说 `never arrived`——入口与出口同名是**透传**，那不算）；组合环在装配期拒（判据只看当前递归路径上的祖先，同一张子图挂多处是合法复用；「查一遍子树」与「挂上」在同一个全局装配临界区里，两个方向对向挂载也进不去环）。运行语义：`In` / `Out` 是**箭头读法**（来源在前、去向在后）声明父侧边界，父图的静态校验（来源 / 环 / 名额）照常生效；子图失败即本节点失败、**首错原样冒泡**（`errors.Is` 成立）；输出就绪的 `Set`、跳过的 `Skip`，**全部跳过**则本节点以 `skipped` 收尾（不是失败）；子图没挂观察者时**自动继承**父图的（手工搭漏挂是**静默**的——父图照常跑完、`Run()` 返回 `nil`，而子图的记录一条都不出现；**继承只保证记录不丢、不带层级归因**，要 `pulse.path` 得**按层各建一个出口**：`Sub` 在装配期按挂载路径给 `ObserverFor`）。**名额整棵树共享**：`WithMaxRunning(n)` = 这棵树里同时最多 n 个节点在 `Run` 里干活（**没人自己声明过时**）——子图没自己声明就继承父图那份，挂子图那一步不吃名额（它整段都在等子图）；子图自己声明 `m` 是**这一支退出父表、自己另算**（`m<=0` = 这一子树不限），不是「在父表之上再卡一道更紧的上限」。**一次性契约这一阶段不动**：同一张子图**挂两处**时只能跑一处（跑的那一步报一句能照着改的话），每个挂载点各跑一份要等 [`Execution`](https://github.com/Luo-root/pulse/issues/311)；声明式装图里 `graph:` 节点声明了 `retry:` 在 `Load` 期就被拒——静态子图只跑一次，把 `retry:` 放到子图里那个真会失败的节点上，每次尝试一张新图要等阶段 3 的事务性 `Retry`。节点 id 不能带 `/`——它是 `pulse.path` 的层级分隔符，`a/b` 与「`a` 里嵌 `b`」会拼出同一条 path——装配期就拒。
- **装配糖只收名字、不改语义。** `pulse.FanOut` / `pulse.Join` 把「一个输入 → N 个并行实例」与「N 路同类型 → 一束」收进函数签名（`pulse.Keys(...)` + `pulse.Batch[T]`）：每条输入都带**来源名**（`Batch.Items`，缺项也占一行），要整束严格就 `m.WaitAll()`，要单路严格就 `m.Get(k)`（缺这一路回 `*SkipError`、传了没声明的 Key 回 `ErrUndeclared`），两个回调都拿到**本节点自己的 `*RunCtx`**（长任务靠 `rc.Context()` 感知取消）。`pulse.NoValue()` 是**节点级**跳过，与只跳一条输出的 `Skip(rc, key)` 不是一回事；`FanOut` 的 N 个 worker **整批一次校验、一次提交**，装不完就整个失败、图上不留半个 fan-out。糖产出的图与手写 `NewNode` 的图观测记录**逐字段一致**（有等价锚用例钉着）。**编译期锁元素类型与个数，锁不住同类型槽位的顺序**（`Keys(a,b)` 与 `Keys(b,a)` 都编译，实测）——按来源 Key 取不受影响，这条边界写在设计文档里。**接线之外还有三件**：`pulse.Only(rc, k, v)` 一句话写排他分支（写这一条、本节点其余出口全作废——手写漏表态时两条下游会都不跑而整轮仍报成功）；`pulse.Produce` / `pulse.Consume` 把流式生产 / 消费的 channel 创建、关闭责任、取消响应与循环收进一次调用（生产端**活着发完**，消费端用 `select` 而不是 `for range`：后者会在取消之后把缓冲区算完、还报成功）；`pulse.Tee` 把一根流**复制**给 N 个下游（普通值不需要它——一个 `Provides` 加 N 个 `Requires` 本来就是广播；channel 才会被瓜分）。流式三件在装配期做快速检查、`Start` 再按**整张流图**复核：流节点与**每条流出口的读取者**（消费者可以手写）要同时有名额，每条流出口得有人消费、且**只喂一个下游**（漏挂下游、或两个读取者静默瓜分值，都会在 `Start` 就被拒，不留到运行时卡死）。
- **观测只走一个 seam。** 引擎发出图级两条（开始 / 结束，把本轮夹在中间）与每节点至多三条回调；把它们折成记录是 `observe` 的事。

完整设计（编排 + 观测）：[`docs/design/pulse.md`](docs/design/pulse.md)。

## 设计理念

上一条「一屏看懂设计」讲的是**形状**，这里讲**为什么**——完整版见 [`docs/design/philosophy.md`](docs/design/philosophy.md)，站点上也有[设计理念](https://luo-root.github.io/pulse/guide/philosophy)一页。

- **拓扑由数据推出来，不由你写。** 节点只声明读什么、写什么；边是「谁生产这条 Key、谁消费它」推出来的——没有边对象，也就没有会漂的连线表。
- **`pending` 不是到达。** 槽位三态：`pending`（还没到）/ `ready`（有值）/ `skipped`（显式跳过）；就绪与跳过**都是**到达。
- **跳过是正常语义，失败绝不降级成跳过。** 门闩判的是「有没有值」——到几个收几个；「缺一条就别跑我」要用一句显式的 `WaitAll` 说出来，而不是默认。
- **错误显式，且在最早能判定处报。** `Start` 直接拒那几类静态可判的死图（无来源、声明的出口没人产出、依赖成环），`Validate` 让装配期反复问（**通过即封口**：这张定义**连同它挂着的子图整棵**从此不再变化；校验失败一层都不封，补上缺的那块再问一次正是要支持的用法），声明式装图连子图也在 `Load` 那一刻校验——按**引用点**各一条，报文点明**挂载路径**（谁也引用不到的 spec 例外：它没有挂载点就没有接口面，「这条键没人喂」在那里判不了，所以只查工厂名 / 重复 id / 来源冲突 / 依赖环 / 流式那几条）。
- **观测与语义同源，且观测不改语义。** 事件与状态在同一点写；观察者抛 panic 不会变成节点失败；业务维度只能走 `Attrs`。
- **引擎零依赖，能力分层。** `pulse` 只用标准库；观测在 `observe`、声明式装图在 `yaml`，依赖方向单向。

## 构建与测试

```bash
go build ./...          # 验证编译
go vet ./...
go test -race -count=1 ./...
```

需要 **Go 1.25.0+**。无 Makefile、无 linter 配置；CI 在每个 PR 与 `main` 推送时跑 build、vet、gofmt 检查与 `-race` 测试。

## 文档

- 指南（中文 / English）：<https://luo-root.github.io/pulse/>
- 设计文档（编排 + 观测）：[`docs/design/pulse.md`](docs/design/pulse.md)
- 理念与设计思想：[`docs/design/philosophy.md`](docs/design/philosophy.md)
- 对象模型与契约（`Graph` / `Execution`，迁移中）：[`docs/design/object-model.md`](docs/design/object-model.md)
- API 契约写在 godoc 里：每个导出符号都带一条。

## 许可证

MIT —— 见 [LICENSE](LICENSE)。
