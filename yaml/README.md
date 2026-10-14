[English](README_en.md) | [中文](README.md)

# yaml

`github.com/Luo-root/pulse/yaml` 将 YAML 文档装配为可重复运行的 `pulse.Graph` 和一次运行的输入计划 `SeedPlan`，也提供不依赖宿主注册表的静态分析。本页是 API、字段和装配契约参考；逐步教程见[声明式装图指南](https://luo-root.github.io/pulse/guide/assembly)。

拓扑由 YAML 声明，宿主登记的具名工厂只提供节点执行体 `pulse.RunFunc`，不返回 Node，不决定接线。本包提供 YAML 入口，没有单独的 JSON 入口。

依赖方向为 `pulse` ← `yaml`。本包依赖根包 `pulse`、`gopkg.in/yaml.v3` 和标准库；根包自身只依赖标准库。

## 版本与安装

需要 Go 1.25.0 或更高版本。本页描述当前 `main` 的 API，其中部分接口尚未随正式 tag 发布。最新正式 tag 为 `v0.4.1`；要运行本页示例，请固定到包含这些接口的提交：

```bash
go get github.com/Luo-root/pulse@1f67045d6c42
```

## 完整示例

以下程序内嵌一份可用 YAML，登记一个 `[]string` 输入键和一个统计工厂。它不需要文件、环境变量或外部服务，正常输出 `count=3`。登记、Load、Inputs 和 Run 的错误都显式检查。

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

const document = `
version: 1
description: Count a literal list
seeds:
  - key: {name: items, type: "[]string"}
    from: {kind: literal, value: [alpha, beta, gamma]}
nodes:
  - id: count
    description: Print the number of items
    uses: demo.count
    requires: [{name: items, type: "[]string"}]
    provides: []
    timeout: 2s
    retry: {attempts: 3, delay: 10ms}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	items := pulse.NewKey[[]string]("items")
	reg := pulse.NewRegistry()
	if err := pulse.RegisterKey(reg, items); err != nil {
		return err
	}
	if err := reg.Register("demo.count", func(rc *pulse.RunCtx) error {
		values, err := pulse.Get(rc, items)
		if err != nil {
			return err
		}
		if _, err := fmt.Printf("count=%d\n", len(values)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	g, plan, err := pulseyaml.Load([]byte(document), reg, pulseyaml.LoadOptions{
		GraphID: "demo",
	})
	if err != nil {
		return err
	}
	in, err := plan.Inputs(nil)
	if err != nil {
		return err
	}
	if err := g.Run(context.Background(), in); err != nil {
		return err
	}
	return nil
}
```

YAML 解码器将列表解为通用容器，Inputs 再按登记类型转换为 `[]string`。这是字面量形状对齐，不需要 resolver。

## API 与装配阶段

| 入口 | 结果 | 何时使用 |
|---|---|---|
| `Load(data, reg, opts)` | `(*pulse.Graph, *SeedPlan, error)` | 已有 YAML 字节和宿主 Registry，准备执行 |
| `LoadFile(path, reg, opts)` | 与 Load 相同 | 先读取 YAML 文件，再按 Load 规则装配 |
| `Analyze(data, opts)` | `(*pulse.Topology, error)` | 无宿主 Registry，检查文档并查看静态结构 |
| `plan.Inputs(resolve)` | `(pulse.Inputs, error)` | 在每轮执行前获取本轮输入 |

Load 要求非 nil Registry；GraphID 对 Load 和 Analyze 都必填。Load 不创建 Execution，不执行工厂或切面，也不解析顶层外部 seed。失败时返回 `nil, nil, err`，调用方应先检查错误再使用产物。

| 阶段 | 主要检查 |
|---|---|
| 登记 Registry | 工厂名不为空、不重复，执行体非 nil；同名键必须具有相同真实 Go 类型 |
| Load | 文档版本与节点形态、登记的工厂与键、类型记号、绑定、子图引用与引用环、子图固定输入及每处挂载的静态问题 |
| SeedPlan.Inputs | 顶层 resolver 的结果、字面量可赋值性、nil 值、重复输入 |
| Graph.Validate | 完整图定义的来源、出口、依赖环等静态问题；通过后封定整棵定义树 |
| Run / NewExecution / Start | 本轮必需入口是否已供值或显式跳过；Start 再做定义静态校验并封定定义 |

Load 不提前判定根层的全部静态问题，例如根层缺来源和依赖环；宿主在 Load 后仍可补充根图输入声明。可在补充声明后调用 `g.Validate()`，或由执行启动时校验。子图的接口已由具体引用点确定，因此其静态问题在 Load 时报告，并带挂载路径。

同一份 SeedPlan 每次调用 Inputs 都创建独立输入表，可为同一图定义的多份 Execution 提供不同值。输入表隔离不代表深拷贝：宿主值中的指针、map 等可变对象仍由宿主管理。

### LoadOptions

| 字段 | 用途 | 规则 |
|---|---|---|
| `GraphID` | 根图定义身份 | 必填；每轮的执行身份另由引擎生成 |
| `Graph` | 根图的 `[]pulse.Option` | 用于 WithObserver、WithAspects 等；子图通过有效执行环境继承，非逐层重复声明 |
| `MaxRunning` | 整棵声明树共享的运行节点额度 | 建议只在这里配置；默认不限，挂载节点自身不占额度 |
| `ObserverFor` | `func(path string) pulse.Observer` | 为子图挂载点选择独立观察者；不需要不同出口时可省略 |

`MaxRunning > 0` 会覆盖 Graph 中已有的 WithMaxRunning；`MaxRunning <= 0` 不覆盖它。因 int 零值无法区分“未配置”和“明确写 0”，要采用默认不限，应避免在 Graph 中另外配置 WithMaxRunning。

ObserverFor 在真图装配时对每个子图挂载点调用一次，路径如 `step`、`outer/inner`。根图观察者通过 Graph 配置，不通过此回调。返回 nil 表示该处继承父执行的观察者。只装不跑的校验树及 Analyze 都不调用 ObserverFor，也不产生观察事件。继承的观察者仍能从每条回调的 Site.Path 区分层级。

LoadOptions 没有 context 字段。图定义不持有生命周期；执行的 context 在 Run / NewExecution 时提供，子执行从挂载节点的 context 派生。

## Registry 与 Go 类型

### 登记接口

| 接口 | 契约 |
|---|---|
| `pulse.NewRegistry()` | 创建独立的装配期注册表 |
| `pulse.RegisterKey(reg, key)` | 登记 Key 的名字与真实 Go 类型；同名同类型重复登记允许，同名异类型报错 |
| `reg.Register(name, run)` | 登记具名 `pulse.RunFunc`，即 `func(*pulse.RunCtx) error`；同名重复报错 |
| `reg.TypeTagOf(name)` | 返回 `(string, bool)`，用于获取文档应使用的类型记号 |
| `reg.ResolveKey(name, typeTag)` | 按名字与非空类型记号对账；未登记或类型不符报错 |
| `pulse.MustRegisterKey` / `reg.MustRegister` | 相应登记接口的 panic 版本，适合已知无误的静态初始化 |

“工厂”在此是具名 Run 执行体，不是返回 Node 的构造函数。Load 查找它并按 YAML 的 requires / provides 建立节点。执行体通过 `pulse.Get` / `pulse.Set` 使用声明过的键；漏声明或类型不符仍由核心契约拒绝。

### type 记号

Load 要求 KeySpec 的 `type` 与登记键的 `reflect.Type.String()` 完全一致。它不是任意昵称，也不是 Go import alias。

| 登记的 Go 类型 | 常见 type 记号 |
|---|---|
| `string` | `string` |
| `[]string` | `[]string` |
| `map[string]int` | `map[string]int` |
| 当前 main 包中定义的 `Doc` | `main.Doc`，列表为 `[]main.Doc` |
| model 包中定义的 `Doc` | `model.Doc`，列表为 `[]model.Doc` |
| `any` | `interface {}` |

命名类型保留包名和类型名；类型别名沿用它指向的真实类型。包的导入别名不会改变反射记号。排查类型不符时，以 TypeTagOf 的返回值或错误中的期望值为准，不应手工猜测 `[]Doc` 一类缩写。

## YAML 字段参考

以下字段对应当前的 [Document / NodeSpec / SeedSpec](load.go) 和 [GraphSpec](subgraph.go)。节点声明顺序不决定执行顺序，数据依赖决定何时进入 Run。

### 文档与子图声明

| 位置 / 字段 | 语法 | 含义 |
|---|---|---|
| `version` | 整数，建议 `1` | 省略、0 或 1 接受，其他版本拒绝 |
| `description` | 字符串 | 根图的人类可读说明，进入 Topology / Snapshot，不影响运行语义 |
| `nodes` | NodeSpec 列表 | 根图至少一个节点 |
| `seeds` | SeedSpec 列表 | 根图输入声明和 SeedPlan 条目，按文档顺序保留 |
| `graphs` | 名字 → GraphSpec | 文档内可复用子图的扁平表 |
| `graphs.<name>.description` | 字符串 | 每处装配出的子图说明 |
| `graphs.<name>.nodes` | NodeSpec 列表 | 每张子图至少一个节点 |
| `graphs.<name>.seeds` | SeedSpec 列表 | 子图挂载点的固定输入，只允许 literal 或 skip |
| `observer` | 字符串 | 仅文档提示，Load 忽略；观察者通过 LoadOptions 配置 |

GraphSpec 不再包含自己的 graphs 表；嵌套通过引用顶层 graphs 中的名字表达。

### 节点

| 字段 | 工厂节点（uses） | 子图节点（graph） |
|---|---|---|
| `id` | 必填，当前图内唯一，不能含 `/` | 同左，也用于挂载路径 |
| `description` | 可选的节点说明 | 父侧挂载节点的说明；子图本身的说明在 GraphSpec |
| `uses` | 必填的登记工厂名 | 不允许与 graph 同时填写 |
| `graph` | 不允许与 uses 同时填写 | 必填的子图名 |
| `requires` | `[{name, type}, ...]`；省略表示空集 | 非空声明拒绝；输入由 in 推导 |
| `provides` | `[{name, type}, ...]`；省略表示空集 | 非空声明拒绝；输出由 out 推导 |
| `in` | 非空声明拒绝 | `{子键: 父键}`，把父值接入子图 |
| `out` | 非空声明拒绝 | `{子键: 父键}`，把子图产物桥回父图 |
| `timeout` | Go duration 字符串，如 `30s` | 限制整张子图这一步的时长 |
| `retry` | `{attempts: 3, delay: 100ms}` | 重试整张子图这一步 |

每个节点必须在 uses 与 graph 中选择一个。子图节点至少有一条 in 或 out 绑定。requires / provides 可显式写 `[]`，便于显示没有输入或输出。

当前语法没有顶层输出端口字段，也没有 `fix:` 字段。provides 声明节点可写的键，不会自动声明根图的宿主出口；需要读取宿主结果时，应在执行前用 Go 的 DeclareOutput 声明出口，并按 [ReadOutput 示例](../examples_test.go)持有 Execution 读取。

### timeout 与 retry

时间字段使用 Go 的 `time.ParseDuration` 形式，例如 `30s`、`100ms`，不使用裸数字。timeout 大于 0 时才安装切面；retry 存在时安装 Retry，attempts 包括第一次尝试，<= 0 按一次处理，delay <= 0 不等待。

装配顺序固定为 **Timeout 在外、Retry 在内**，与 YAML 字段书写顺序无关。因此 timeout 覆盖等待输入、全部重试尝试及其间隔，并非“每次尝试各有这么久”。超时采用协作式取消：内层须响应 context；若执行体忽略取消，框架仍等待它返回，不会强制终止 goroutine。

正常跳过和已取消的上下文不触发重试。Retry 为每次尝试暂存引擎输出，失败尝试的输出不发布给下游；外部 HTTP、数据库或文件等副作用不自动回滚。更多规则见根包 Retry 契约。

### KeySpec、SeedSpec 与 SeedFrom

| 字段 | 含义 |
|---|---|
| `key: {name, type}` | 必填的已登记键名与类型记号 |
| `skip: true` | 本轮输入显式跳过，from 的内容不使用 |
| `from.kind` | `literal` 或空字符串直接取 value；其他值交给宿主 resolver |
| `from.value` | literal 的 YAML 值 |
| `from.env` | 供 env resolver 使用的变量名 |
| `from.path` | 供 file resolver 使用的路径 |
| `from.key` | 供 context 等 resolver 使用的宿主键 |

顶层 env、file、context 是传给 resolver 的描述，不是框架内置 IO。其他非 literal kind 也交由 resolver 判断。Inputs 的回调形态为 `func(from pulseyaml.SeedFrom) (any, error)`：宿主决定如何读取及处理缺值，返回错误会终止 Inputs。需要请求 context 时可由回调闭包持有。

literal 或 skip 条目不调用 resolver；只使用这些条目时可传 nil。非 literal 条目未提供 resolver 会报错。LoadFile 只负责读取 YAML 文档本身，不代为读取 seed 引用的文件。

### 字面量形状对齐

Inputs 通过与 `pulse.SeedByName` 相同的转换规则，将 YAML 解码结果递归对齐到登记类型。子图 literal 则在 Load 时通过 FixByName 对齐。

| YAML 值 → 登记类型 | 结果 |
|---|---|
| `[a, b]` → `[]string` | 逐元素转换 |
| `{a: 1}` → `map[string]int` | 逐键、逐值转换 |
| 对应长度列表 → 数组 | 逐元素转换，长度须匹配 |
| 数值 → 数值类型 | 做范围检查；浮点转整数要求整值，不截断小数 |
| map → struct | 不猜字段，报错；由 resolver 返回正确的 struct |
| 字符串 ↔ 数字 | 不自动互转，报错 |

数值转浮点遵循 Go 转换口径，整数可能损失精度。nil 接口不能作为宿主输入值，缺值应显式 skip。根层重复写同一输入在 Inputs 时拒绝；子图重复 seed 在 Load / Analyze 时拒绝。

## Analyze：无需 Registry 的静态视图

下面是可放入程序中的分析函数，需要导入 fmt 和本包：

```go
func describe(doc []byte) error {
	top, err := pulseyaml.Analyze(doc, pulseyaml.LoadOptions{GraphID: "demo"})
	if err != nil {
		return err
	}
	if len(top.Problems) > 0 {
		return fmt.Errorf("static problems: %v", top.Problems)
	}
	if _, err := fmt.Print(top.Mermaid()); err != nil {
		return err
	}
	return nil
}
```

Analyze 与 Load 共用解码、装配、端口、绑定及核心静态判据，返回 `*pulse.Topology`。它不创建 Execution，不运行工厂或切面，不解析外部 seed，不调用 ObserverFor 或 Observer。Graph / MaxRunning 的静态选项与 Load 一致。

| 检查项 | Analyze | Load / Inputs |
|---|---|---|
| YAML 形态、节点、引用、绑定、静态子图 | 检查 | 检查 |
| 根层静态问题 | 保留在 Topology.Problems，调用方须读取 | Load 不提前判全部问题；Validate / Start 检查 |
| 子图与装配错误 | 返回 error | 返回 error |
| 同名键及绑定两端的已知类型标签 | 要求一致，并沿绑定传播 | 按 Registry 的真实类型对账 |
| 工厂是否登记 | 不检查，未知名字可通过 | Load 检查 |
| 类型标签是否是实际 Go 类型 | 无法证明 | Load 检查登记表 |
| literal 是否能赋值到目标类型 | 不推断、不检查 | 根层 Inputs、子图 Load 检查 |
| 工厂内的流式读写行为 | 不推断 | 同样无法从 Run 推断 |

显式 KeySpec 仍须填写 type。Analyze 将 `example.Item` 之类的标签原样保留，不从 literal 猜类型。只出现在接线中、且不能从任何已知声明推导类型的键，返回的 Type 为空字符串。因此 Analyze 成功并不代表文档可以执行，仍需 Load 和 Inputs 校验宿主类型与值。

拓扑保留每处挂载的完整输入、输出和透传端口，内部键不会自动变成出口。Go 端口按首次成功声明序；YAML 的 in / out 映射按键名排序，根 seeds 按文档序，子图固定输入在 in 之后按键名声明。标签可知、身份与接线一致时，Analyze、Load 与手写 Go 的拓扑可逐字段比较；类型标签预检查更早，多种错误并存时不承诺首条诊断相同。

YAML 不声明 Produce / Consume / Tee 等 Go 流糖标记。静态校验不能推断工厂或切面内部隐藏的 channel 等待、手写 close 时机等行为。

## 可复用静态子图

graphs 声明拓扑模板，nodes[].graph 在某一步静态挂载它。下面的模板挂载两次，子图的 prefix 是固定输入，topic 来自每处 in 接线：

```yaml
version: 1
graphs:
  enrich:
    description: Add a prefix
    seeds:
      - key: {name: sg.prefix, type: string}
        from: {kind: literal, value: "Summary: "}
    nodes:
      - id: work
        uses: demo.enrich
        requires:
          - {name: sg.topic, type: string}
          - {name: sg.prefix, type: string}
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: root.input, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: first
    graph: enrich
    in: {sg.topic: root.input}
    out: {sg.summary: root.first}
  - id: second
    graph: enrich
    in: {sg.topic: root.input}
    out: {sg.summary: root.second}
```

执行此片段需要登记六个 string 键：root.input、root.first、root.second、sg.topic、sg.prefix、sg.summary，以及 demo.enrich 执行体。执行体读取 sg.topic 和 sg.prefix，写出 sg.summary。

每处挂载在 Load 时创建独立图定义，并展开为引擎的静态 Sub。父侧挂载节点的 Requires / Provides 由 in / out 推导。装配后无需运行即可通过 `g.Topology().Nodes[i].Sub` 查看完整子树；子图 ID 为 `<根 id>/<挂载路径>`，路径为 first、second 或 outer/inner。

运行时，每处挂载及每次重试创建独立子 Execution，各自拥有槽位、首错和取消域。子执行 context 从挂载节点派生，父取消会传入；代码仍需协作响应取消。

### 接线与固定输入

| 声明 | 方向与规则 |
|---|---|
| `in: {child: parent}` | 父读 → 子输入；child 成为该引用点子图的入口 |
| `out: {child: parent}` | 子出口 → 父输出；child 成为该引用点子图的出口 |
| 子图 `seeds` literal | 转为挂载点的 Fix 绑定；固定值不写入图定义 |
| 子图 `seeds` skip | 转为挂载点的 FixSkip 绑定；from 忽略 |
| 同一 child 同时出现在 in 和 out | 透传：父值直接作为该子出口值，不要求内部有人读取或产出 |

两端键都必须登记，真实类型必须匹配。非透传的 in 子键必须被子图要求，非透传的 out 子键必须由子图提供。in 的子键不能同时被子图内部提供，也不能已经在该子图 seeds 中声明；否则一条输入具有两个来源。透传同样不能有内部提供者。

子图非 skip seed 只允许 literal 或省略 kind，env / file / context 等在装配期拒绝。需要外部值时，应在根 SeedPlan 中解析，再通过 in 传入。子图重复 seed、未知引用、空 nodes、无效绑定与引用环都在装配期报告；引用环会给出具体路径，嵌套本身不设固定深度上限。

### 按引用点校验

同一张 spec 的接口由每一处 in / out / seeds 分别确定。两处引用中只有一处接入某输入时，另一处仍算缺来源，诊断指向缺少接线的挂载路径。检查不会把两处输入合并后替缺少的一处放行。

每处子图的来源、声明出口和依赖等静态问题使用核心 Validate / Start 的判据。因此 out 声明但无人产出的子出口在 Load 时拒绝，无须等运行后才发现没有到达。

所有 graphs 声明都会检查。没有任何引用的 spec 缺少挂载接口：未解决的 Requires 可能是未来的 in 输入，因此不判缺来源。已知 literal / skip seed 仍是真实外部来源；同键再有节点 Provides 时，Load 与 Analyze 都以 `ErrDuplicateSource` 拒绝。空节点、重复 id、已知来源冲突、引用与依赖环等仍检查，Load 还检查其工厂登记和实际类型。模板中的具体嵌套引用仍按其接线检查。

### 子图的 timeout 与 retry

写在 graph 节点上的 timeout / retry 作用于父侧挂载节点，控制整张子图这一步；写在子图内部工厂节点上的配置只控制该内部节点。

整张子图重试时，每次尝试都有新的子 Execution。失败尝试桥回父侧的输出随该尝试写集丢弃，父侧只接收成功或正常跳过尝试提交的输出，不会混用不同尝试的版本。多键提交逐键可见，不承诺整个输出批次在同一瞬间原子可见；外部副作用不自动回滚。

## 常见问题

| 问题 | 处理 |
|---|---|
| Analyze 成功，Load 却说工厂不存在？ | Analyze 不依赖宿主 Registry；登记 uses 对应的 RunFunc |
| `[]Doc` 为什么与登记表不符？ | 通过 TypeTagOf 获取真实反射类型记号，命名类型通常包含包名 |
| Load 成功，Run 仍报缺来源或缺输入？ | Load 留给根层后续装配和本轮供值；检查入口声明、Inputs 和根层 Topology.Problems |
| YAML 的 observer 为什么没有效果？ | 这是提示字段；根观察者用 LoadOptions.Graph，子图独立出口用 ObserverFor |
| 子图需要读取文件怎么办？ | 根层 resolver 读文件得到类型正确的值，再通过 in 传入 |
| 怎么声明宿主输出？ | 在 Go 中 DeclareOutput，并通过 Execution + ReadOutput 读取；provides 不是宿主端口 |
| skip 会令整轮失败吗？ | 正常 skip 是到达，无失败错误；真实错误保持失败身份 |

## 相关资料

- [声明式装图指南](https://luo-root.github.io/pulse/guide/assembly)
- [Load、SeedPlan 与文档字段](load.go)、[Analyze](analyze.go)、[子图装配](subgraph.go)
- [Registry 与类型登记](../key.go)、[输入转换](../inputs.go)、[Retry / Timeout](../aspect.go)
- [静态子图核心契约](../sub.go)、[宿主结果读取示例](../examples_test.go)
- [设计说明](../docs/design/pulse.md)中的装配部分
