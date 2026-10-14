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
		GraphID: "yaml-demo", // 必填：观测靠它区分「跑的是哪张图」
	})
	if err != nil {
		panic(err)
	}

	// 3) 宿主把 Seed 计划解成这一次运行的输入，然后运行。
	in, err := plan.Inputs(nil) // literal 之外需要 resolve
	if err != nil {
		panic(err)
	}
	if err := g.Run(context.Background(), in); err != nil {
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
- **引擎不做 IO**。`from.kind` 不是 `literal` 时，`Load` 只把它原样交给 `plan.Inputs(resolve)`；读文件、读环境变量、从请求取 context 都是宿主的事。
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

它展开成[图即节点](/guide/orchestration)的**静态组合 `pulse.Sub`**：子图在 **`Load` 期按挂载位置装好**（一处挂载一张实例），父侧那个节点的 `Requires` / `Provides` 由 `in` / `out` 推出来，引擎的静态校验（来源 / 环 / 名额）照常生效。由此：

- **装完不运行就能看完整棵树**：`g.Topology()` 沿静态挂载边递归，`Nodes[i].Sub` 就是这一处的子图（再深一层继续 `.Sub`），路径按**挂载位置**推导（`step1`、`step1/toInner`），图身份是 `<根 id>/<挂载路径>`；
- **子图的静态毛病在 `Load` 期报**，报文点明**挂载路径**：`pulse/yaml: graph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides it (declare it with DeclareInput if the value comes from outside)`；
- **同一张 spec 挂几处 = 几张独立实例**：每处都有自己的槽位与挂载路径，两处都跑得了，观测里靠 graph id 与 `pulse.path` 分得开；
- **取消从挂载点传进子图**：父图 ctx 一取消，子图节点当场看见——不是跑满整段再把取消吞掉。

规则：

- **边界写在接线处**：`in` / `out` 一律「子键: 父键」；子图节点**不能**再写 `requires` / `provides`。
- **类型与名字都在 `Load` 期比**：两端各按 `{name, type}` 查表且用**同一个** type 记号（父侧那条键登记成别的类型就报）；引用不存在的子图、绑定子图里没人声明的键、子图没有节点，三条都点名报错。
- **引用环在 `Load` 期拦**：子图里再挂子图允许、深度不设限；`a → b → a` 会被拦并给出具体路径。
- **边界只有一处说法**：子图节点不能再写 `requires` / `provides`，**工厂节点也不能写 `in` / `out`**——写错那一侧就是静默不生效，所以都在装图期报。
- **`in:` 喂的键不能被子图自己再提供**：一条键恰好一个来源——`in:` 要做的正是把那条键声明成子图的进口，声明那一刻就撞 `ErrDuplicateSource`，装图期就报。
- **子图的 `seeds`**：**要种值**的那条只允许 `literal`（`env` / `file` / `context` 要靠宿主 IO，而 `SeedPlan` 是父图的产物；`skip: true` 的那条 `from` 没有语义，与顶层 `seeds` 一个口径）；键名 / 类型按登记表对账；**同一条键在同一张图里声明两次**也报——第二次会被幂等首写静默忽略，谁赢取决于声明顺序。
- **被引用的每一张 `graphs:` 声明都查，而且按引用点各查一遍**：装配期先装一棵**只装不跑**的校验树——**同一段装配代码**，子图一律按静态形态挂上去，每个引用点各是一张实例。每张实例按**那一处**的 `in:` / `out:` 把自己的接口面声明成端口（`DeclareInput` / `DeclareOutput`），于是「这本来就是外部输入」与「你忘了接线」分得开，也不必再往校验树上种「假来源」；最后让引擎自己递归 `Validate` 判一遍——所以子图里「某条 `Requires` 没人提供（也没声明成入口）」「声明过的出口没人产出」与「依赖成环」也在 `Load` 报，**报文与 `Start` 那次逐字相同**，还点明**挂载路径**（例：`graph "inner" (mounted at "step1/toInner")`）。按引用点而不是按并集：同一张 spec 挂两处时两边的 `in:` 可以不一样，按并集校验等于说「另一处喂了 = 这一处也喂了」。
- **未引用 spec 不判缺来源，仍判已知冲突**：未知 Requires 可能是未来的入口，先声明进临时校验图；literal/skip seed 是已知的外部来源，同键还有节点 Provides 时，Load 与 Analyze 都包装 ErrDuplicateSource 拒绝。工厂名 / 空 `nodes` / 重复 id / 依赖环 / 流式关系也照查。实际引用后，缺来源按那一处的 `in:` / `out:` 判断。
- **`Load` 不碰任何 ctx**：图是定义、定义没有生命周期，一次运行在谁的取消域里跑是 `Run` / `NewExecution` 那一刻的事（`LoadOptions.Context` 因此删掉了——留着只会是个静默失效的旋钮）；子图的取消照样从挂载点传进去（运行期由挂载点把父节点这一层的 ctx 派生给子执行）。`Load` 失败时返回 `nil, nil, err`，调用方连清理的把手都没有，所以「所有可能失败的校验」都排在「把图交出去」前面。
- **切面落在父侧那个节点上**：`timeout: 30s` 就是给整张子图限时，`retry: {attempts: 3, delay: 100ms}` 就是整张子图最多跑三次——`retry:` 的**每一次尝试各建一份新的子执行**，第一次那轮的桥回结果随它自己那条 **attempt 写集**一起作废，父侧只拿得到**成功那一次**的输出版本（「一半第一次、一半第二次」的混装不会出现）；子图**里面**的节点各写各的，放进里面的 `retry:` 照旧只重跑那一个节点。
- **每处挂载装一张、各有自己的槽位与挂载路径，两处都跑得了**：同一张 spec 被引用两次就是两个独立实例，观测里靠 graph id 与 `pulse.path` 分得开。
- **根层自己那几条判据仍在启动那一刻报**（`Run` / `NewExecution`；装图期不判根层）：顶层 `seeds:` 只把键声明成根图的入口，值由宿主在运行前经 `SeedPlan.Inputs` 给出——根层的外部输入面在 `Load` 之后还可能被宿主自己补声明，装图期看不全，所以来源那条留给启动一并判。

**观测默认继承，可按挂载点另选出口**：根图通过 `LoadOptions.Graph` 挂观察者；子图需要不同观察者或记录目的地时，才给 `LoadOptions.ObserverFor(path)`。它在 **Load 装配每处子图挂载时调用一次**，path 一层为节点 id，更深为 `outer/inner`，根图不调它。返回 nil 则继承父图的有效观察者，选出的观察者也可继续由后代继承。每条回调已有 `Site.Path`，同一个出口能服务多层并写出各自的 `pulse.path`，通常不填 `ObserveConfig.Path`。校验树**不调** ObserverFor，宿主的回调次数不会因额外校验而变。

## 边界与坑

**`literal` 的取值按登记类型做形状对齐，列表与映射开箱即用。** YAML 解出的泛型容器（`[]any` / `map[string]any`）会被 `pulse.SeedByName` 递归转成目标类型：`value: ["a","b"]` 直接填 `[]string`、`value: {a: 1}` 直接填 `map[string]int`，都不需要 `resolve`。

**刻意不做**的三类（一律报错，用 `resolve` 给出类型正确的值）——判据是「形状可以机器对齐，语义只有调用方知道」：

| 情形 | 报错（本机实测，类型名随你的包而变） |
|---|---|
| 元素是对象、目标是 struct（字段映射是宿主的语义） | `pulse: seed "demo.in": value type []interface {} not assignable to []yaml_test.Doc (element 0: cannot convert map[string]interface {} to yaml_test.Doc)` |
| 字符串 ↔ 数字互转 | `pulse: seed "demo.in": … (element 0: cannot convert string to int)` |
| 浮点截断成整数 / 越界收窄 | `… (element 0: cannot convert float64 to int)` · `… (element 0: cannot convert int to int8)` |

需要这些取值时，把 `resolve` 交给 `plan.Inputs` 自己给出类型正确的值：

```go
in, err := plan.Inputs(func(from yaml.SeedFrom) (any, error) {
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
- **额度只走 `LoadOptions.MaxRunning`**：它是**整棵声明树**的额度（`<=0` = 不限，默认）。不要在 `LoadOptions.Graph` 里再放 `pulse.WithMaxRunning`——两个都写时按「`MaxRunning > 0` 压过 `Graph` 里那份（哪怕更小），`<= 0` 不动它」合（`int` 的零值分不出「没写」与「写了 0」，所以 `MaxRunning: 0` 表达不了「整棵树不限」）；七档组合由包内用例 `TestLoadOptionsQuotaPrecedence` 钉住；
- 本包是唯一带第三方依赖的包（`gopkg.in/yaml.v3`）；根包与 `observe` 都不依赖它。

包级 API 与示例见 [yaml 包文档](/packages/yaml/)。
