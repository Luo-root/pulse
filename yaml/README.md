[English](README_en.md) | [中文](README.md)

# yaml

`pulse/yaml` 是声明式装图（**YAML only**，不提供 JSON）：把 YAML 装成 `pulse.Graph` + `SeedPlan`。

**拓扑归 YAML**：节点必填 `id` / `uses` / `requires` / `provides`；`uses` 对应 `pulse.Registry` 上的具名 Run 工厂（`func(*pulse.RunCtx) error`）——工厂只给 Run，**不返回 `*Node`**，因为它不该决定自己接在图的哪里。

```go
reg := pulse.NewRegistry()
pulse.MustRegisterKey(reg, In)
reg.MustRegister("demo.step", func(rc *pulse.RunCtx) error { /* ... */ return nil })

g, plan, err := yaml.Load(doc, reg, yaml.LoadOptions{GraphID: "demo"})

// from.kind 不是 literal 时取值由宿主给出——引擎不做 IO。
resolve := func(from yaml.SeedFrom) (any, error) { return loadDocs(from.Path) }
_ = plan.Apply(g, resolve)

_ = g.Run()
```

```yaml
version: 1
seeds:
  - key: {name: docs, type: "[]Doc"}
    from: {kind: file, path: docs.json}
nodes:
  - id: summarize
    uses: demo.step
    requires: [{name: docs, type: "[]Doc"}]
    provides: [{name: summary, type: string}]
    timeout: 30s
    retry: {attempts: 3, delay: 100ms}
```

要点：

- 节点切面顺序：**Timeout 在外、Retry 在内**（先写的更靠外）。
- Key 用 `{name, type}` 对账；`type` = `reflect.Type.String()`，与 `RegisterKey` 一致。
- 时间字段用 Go 的 `ParseDuration` 形式（`30s` / `100ms`），不写裸数字。
- **字面量按登记类型做形状对齐**：`literal` 的取值由 YAML 解码器给出（泛型容器 `[]any` / `map[string]any`），`pulse.SeedByName` 会把它递归转成目标类型——`value: ["a","b"]` 直接填 `[]string`、`value: {a: 1}` 直接填 `map[string]int`，不需要 `resolve`。**不做**的：map→struct 的字段猜测、字符串↔数字互转、浮点截断成整数（这些一律报错，用 `resolve` 给出类型正确的值）。
- `Seed.from.kind` 除 `literal` 外需要宿主传 `resolve` 回调——**引擎不做 IO**：读文件、读 env、从请求取 context 都是宿主的事。
- 本包依赖 `gopkg.in/yaml.v3`；根包 `pulse` 不依赖 yaml。

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

它展开成引擎的 `pulse.Sub`（见设计文档 §5「图即节点」）：父侧那个节点的 `Requires` / `Provides` 由 `in` / `out` 推出来，引擎的静态校验（来源 / 环 / 名额）照常生效。规则：

- **边界写在接线处**：`in` / `out` 一律「**子键: 父键**」；子图节点**不能**再写 `requires` / `provides`——边界只有一处说法。
- **类型在 Load 期比**：两端各按 `{name, type}` 查表，而且用**同一个** `type` 记号——父侧那条键登记成别的类型，装图期就报。Go 那侧是编译期红，这一侧是装图期红。
- **名字在 Load 期比**：引用一张不存在的子图、绑定一条子图里没有任何节点声明的键、子图没有节点——三条都点名报错。
- **引用环在 Load 期拦**：子图里再挂子图是允许的、深度不设限；`a → b → a` 这种装不出图的环会被拦，并给出一条**具体**的路径（与引擎报依赖环同形）。
- **边界只有一处说法**：子图节点**不能**再写 `requires` / `provides`；反过来，**工厂节点写了 `in` / `out` 也在装图期报**——写错那一侧就是静默不生效。
- **`in:` 喂的那条子侧键不能被子图自己再提供** → 装图期报：一条键恰好一个来源，跑到那一步就是 `ErrDuplicateSource`。
- **子图的 `seeds`**：**要种值**的那条只允许 `literal`（`env` / `file` / `context` 要靠宿主 IO，而 `SeedPlan` 是**父图**的产物——装图期就报，不留到跑到一半；`skip: true` 的那条 `from` 没有语义，与顶层 `seeds` 一个口径）。键名 / 类型按登记表对账；**同一条键在同一张图里声明两次**也报（第二次会被幂等首写静默忽略，谁赢取决于声明顺序）。
- **子图自己 seed 了某条键、父侧又用 `in` 喂它** → 装图期报（两次种同一条槽，第二次会被幂等首写静默忽略，而两处声明看着都有道理）。
- **每一张 `graphs:` 声明都查**，包括谁也引用不到的那些，而且**按引用点各查一遍**：装进图的只有根层那批，子图要等运行到才装——只查根层的话，第 2 层往后写错的毛病会「装图全过、跑到一半才炸」，没人引用的那张更是永远不炸。做法是给每个引用点起一张**只装不跑**的校验图走同一段装图代码（引擎那边的装配期检查：节点 id、重复、来源冲突、绑定），把它那一层拿得到的来源种上（spec 自己的 `seeds` + 父侧 `in:` 喂进来的键），再请引擎判一遍（`Graph.Validate`，见设计文档 §5）——所以子图里「某条 `Requires` 没人提供、也没人 seed」与「依赖成环」同样在 `Load` 报，而且**报文与 `Start` 那次逐字相同**。按引用点而不是按 spec：同一张图被两处引用时两边的 `in:` 可以不一样，按并集校验等于说「另一处喂了 = 这一处也喂了」。
- **校验图不碰宿主的 ctx，根图最后才建**：校验图都在 `context.Background()` 上建；`Load` 失败时返回 `nil, nil, err`，调用方连清理的把手都没有——所以「所有可能失败的校验」都排在「建根图」前面。根图照旧继承宿主 ctx（取消得传得进去），宿主跑到 `Wait` 就把它释放。
- **切面**：子图节点照样能写 `timeout` / `retry`，它们落在**父侧那个节点**上——`timeout: 30s` 就是给整张子图限时；子图**里面**的节点各写各的。
- **同一张子图被引用两次 = 两个独立实例**：每次运行按 spec 新装一张图（一次性契约天然满足），观测里靠 graph id 与 `pulse.path` 分得开。

**观测按层给**：`LoadOptions.ObserverFor(path)` 按层建出口，`path` 是这一层的路径（根为空串、一层子图是它的节点 id、两层是 `outer/inner`）——把它交给 `observe.ObserveConfig.Path`，那一层的每条记录就都带 `pulse.path`。返回 nil 表示这一层按 `LoadOptions.Graph` 里挂的那条走（子图会继承父图的观察者，只是不带层级）。

设计见 [`docs/design/pulse.md`](../docs/design/pulse.md) §5 装配。
