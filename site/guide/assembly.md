# 声明式装图

`pulse/yaml` 把一份 YAML 装成 `*pulse.Graph` + `SeedPlan`。分工是硬的：

> **YAML 拥有拓扑，注册的工厂只给 `Run`。**

工厂不返回 `*Node`，因为它不该决定自己接在图的哪里。同一个 `Run` 换个 YAML 就能接进另一张图。

## 两条路

| 路线 | 拓扑写在哪 | 适用 |
|---|---|---|
| 命令式 | Go 代码：`pulse.NewNode(...)` + `g.Add(...)` | 拓扑与代码同生命周期，编译期就能对上 |
| 声明式 | YAML：`id` / `requires` / `provides` | 拓扑要能改、要能按请求下发、要能与人共享 |

两条路都产出 `*Graph`，可以混用（声明式里 `uses` 指向的工厂本身就是普通 Go 函数）。

## 完整例子

```go
package main

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/yaml"
)

var (
	Topic   = pulse.NewKey[string]("topic")
	Summary = pulse.NewKey[string]("summary")
	Report  = pulse.NewKey[string]("report")
)

const doc = `
version: 1
seeds:
  - key: {name: topic, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: summarize
    uses: demo.summarize
    requires: [{name: topic, type: string}]
    provides: [{name: summary, type: string}]
    timeout: 30s
    retry: {attempts: 3, delay: 100ms}
  - id: report
    uses: demo.report
    requires: [{name: summary, type: string}]
    provides: [{name: report, type: string}]
`

func main() {
	// 1) 注册 Key（YAML 用 {name, type} 对账）与具名工厂。
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, Topic)
	pulse.MustRegisterKey(reg, Summary)
	pulse.MustRegisterKey(reg, Report)

	reg.MustRegister("demo.summarize", func(rc *pulse.RunCtx) error {
		topic, err := pulse.Get(rc, Topic)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Summary, "about "+topic)
	})
	reg.MustRegister("demo.report", func(rc *pulse.RunCtx) error {
		summary, err := pulse.Get(rc, Summary)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Report, "report: "+summary)
	})

	// 2) 装图：拓扑来自 YAML。
	g, plan, err := yaml.Load([]byte(doc), reg, yaml.LoadOptions{
		Context: context.Background(),
		GraphID: "yaml-demo", // 必填：观测靠它区分「跑的是哪张图」
	})
	if err != nil {
		panic(err)
	}

	// 3) 宿主执行 Seed 计划，然后运行。
	if err := plan.Apply(g, nil); err != nil { // literal 之外需要 resolve
		panic(err)
	}
	if err := g.Run(); err != nil {
		panic(err)
	}
	fmt.Println("run ok:", g.ID())
}
```

```text
run ok: yaml-demo
```

## YAML 字段

| 字段 | 说明 |
|---|---|
| `version` | 缺省或 `1`；其它值拒绝 |
| `seeds[].key` | `{name, type}`——必须与 `RegisterKey` 登记的一致 |
| `seeds[].from` | `kind`：`literal` / `env` / `file` / `context`；`literal` 之外的取值由宿主的 `resolve` 给出 |
| `seeds[].skip` | `true` 表示这条走 `SkipSeed`（槽位以「跳过」到达） |
| `nodes[].id` | 节点 ID，必填；观测归因键 |
| `nodes[].uses` | 必填，对应 `Registry` 上的具名工厂 |
| `nodes[].requires` / `provides` | `{name, type}` 列表 |
| `graphs` | 可复用子图的声明（名字在文档内唯一），子图自己不写 `graphs` |
| `nodes[].graph` | 这个节点 = 装一张子图（与 `uses` 二选一） |
| `nodes[].in` / `out` | 子图节点的边界，一律「**子键: 父键**」 |
| `nodes[].timeout` | 装成 `pulse.Timeout`，**在外** |
| `nodes[].retry` | `{attempts, delay}`，装成 `pulse.Retry`，**在内** |

几个要点：

- **Key 用 `{name, type}` 对账**。`type` 就是 `reflect.Type.String()`（`string`、`[]string`、`[]Doc`……），与 `RegisterKey` 登记的一致；名字对不上或缺 `type` 都在 `Load` 期报错。
- **时间字段**用 Go 的 `ParseDuration` 形式（`30s` / `100ms`），不写裸数字。
- **切面顺序**：先列的更靠外 → `timeout` 在外、`retry` 在内，即「总时长受限、每次尝试各自重试」。
- **引擎不做 IO**。`from.kind` 不是 `literal` 时，`Load` 只把它原样交给 `plan.Apply(g, resolve)`；读文件、读环境变量、从请求取 context 都是宿主的事。
- `observer` 字段是文档提示位，`Load` 忽略它——观察者走 `LoadOptions.Graph`（`pulse.WithObserver(...)`）与 `LoadOptions.ObserverFor`（按层，见下）。

## 子图：一步 = 一张图

`graphs:` 声明可复用的子图，`nodes[].graph` 把某一步指向它——**子图是拓扑的复用单元**，不用为了复用一段流程去写 Go 工厂：

```yaml
version: 1
graphs:
  enrich:
    nodes:
      - id: work
        uses: demo.enrich
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: sg.input, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: step1
    graph: enrich
    in:  {sg.topic: sg.input}      # 子键: 父键（父读 → 子 Seed）
    out: {sg.summary: sg.result}   # 子键: 父键（子产出 → 父 Set）
```

它展开成[图即节点](/guide/orchestration)的 `pulse.Sub`：父侧那个节点的 `Requires` / `Provides` 由 `in` / `out` 推出来，引擎的静态校验（来源 / 环 / 名额）照常生效。规则：

- **边界写在接线处**：`in` / `out` 一律「子键: 父键」；子图节点**不能**再写 `requires` / `provides`。
- **类型与名字都在 `Load` 期比**：两端各按 `{name, type}` 查表且用**同一个** type 记号（父侧那条键登记成别的类型就报）；引用不存在的子图、绑定子图里没人声明的键、子图没有节点，三条都点名报错。
- **引用环在 `Load` 期拦**：子图里再挂子图允许、深度不设限；`a → b → a` 会被拦并给出具体路径。
- **边界只有一处说法**：子图节点不能再写 `requires` / `provides`，**工厂节点也不能写 `in` / `out`**——写错那一侧就是静默不生效，所以都在装图期报。
- **`in:` 喂的键不能被子图自己再提供**：一条键恰好一个来源，否则跑到那一步就是 `ErrDuplicateSource`。
- **子图的 `seeds`**：**要种值**的那条只允许 `literal`（`env` / `file` / `context` 要靠宿主 IO，而 `SeedPlan` 是父图的产物；`skip: true` 的那条 `from` 没有语义，与顶层 `seeds` 一个口径）；键名 / 类型按登记表对账；**同一条键在同一张图里声明两次**也报——第二次会被幂等首写静默忽略，谁赢取决于声明顺序。
- **每一张 `graphs:` 声明都查**（含谁也引用不到的那些），而且**按引用点各查一遍**：装进图的只有根层那批，子图要等运行到才装——只查根层的话，第 2 层往后写错的毛病会「装图全过、跑到一半才炸」。做法是给每个引用点起一张**只装不跑**的校验图走同一段装图代码，把它那一层拿得到的来源（spec 自己的 `seeds` + 父侧 `in:` 喂进来的键）种上，最后请引擎自己判一遍——所以子图里「某条 `Requires` 没人提供、也没人 seed」与「依赖成环」也在 `Load` 报，**报文与 `Start` 那次逐字相同**。按引用点而不是按 spec：同一张图被两处引用时两边的 `in:` 可以不一样，按并集校验等于说「另一处喂了 = 这一处也喂了」。
- **校验图不碰宿主的 ctx，根图最后才建**：校验图都在 `context.Background()` 上；`Load` 失败时返回 `nil, nil, err`，调用方连清理的把手都没有，所以所有可能失败的校验都过完才建根图。根图照旧继承宿主 ctx（取消传得进去），宿主跑到 `Wait` 就把它释放。
- **切面落在父侧那个节点上**：`timeout: 30s` 就是给整张子图限时；子图**里面**的节点各写各的。
- **同一张子图被引用两次 = 两个独立实例**：每次运行新装一张图，观测里靠 graph id 与 `pulse.path` 分得开。

**观测按层给**：`LoadOptions.ObserverFor(path)` 按层建出口（`path` 根为空串、一层子图是它的节点 id、两层是 `outer/inner`），交给 `observe.ObserveConfig.Path` 就让那一层的每条记录都带 `pulse.path`——嵌套的日志 / 追踪才拼得回树。

## 边界与坑

**`literal` 的取值按登记类型做形状对齐，列表与映射开箱即用。** YAML 解出的泛型容器（`[]any` / `map[string]any`）会被 `pulse.SeedByName` 递归转成目标类型：`value: ["a","b"]` 直接填 `[]string`、`value: {a: 1}` 直接填 `map[string]int`，都不需要 `resolve`。

**刻意不做**的三类（一律报错，用 `resolve` 给出类型正确的值）——判据是「形状可以机器对齐，语义只有调用方知道」：

| 情形 | 报错（本机实测，类型名随你的包而变） |
|---|---|
| 元素是对象、目标是 struct（字段映射是宿主的语义） | `pulse: seed "demo.in": value type []interface {} not assignable to []yaml_test.Doc (element 0: cannot convert map[string]interface {} to yaml_test.Doc)` |
| 字符串 ↔ 数字互转 | `pulse: seed "demo.in": … (element 0: cannot convert string to int)` |
| 浮点截断成整数 / 越界收窄 | `… (element 0: cannot convert float64 to int)` · `… (element 0: cannot convert int to int8)` |

需要这些取值时，用 `resolve` 自己给出类型正确的值：

```go
plan.Apply(g, func(from yaml.SeedFrom) (any, error) {
	if from.Kind == "file" {
		raw, err := os.ReadFile(from.Path) // 宿主做 IO
		if err != nil {
			return nil, err
		}
		var docs []Doc
		if err := json.Unmarshal(raw, &docs); err != nil {
			return nil, err
		}
		return docs, nil
	}
	return nil, fmt.Errorf("unsupported seed kind %q", from.Kind)
})
```

其它边界：

- **YAML only**：不提供 JSON 入口（同一种拓扑只维护一种方言）；
- `nodes` 为空、节点缺 `id` / `uses`、`uses` 指向未注册的工厂，都在 `Load` 期报错（后者带上节点 id）；
- `LoadOptions.GraphID` 必填——图身份是观测的归因键；
- 本包是唯一带第三方依赖的包（`gopkg.in/yaml.v3`）；根包与 `observe` 都不依赖它。

包级 API 与示例见 [yaml 包文档](/packages/yaml/)。
