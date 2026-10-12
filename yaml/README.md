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

它展开成引擎的 **`pulse.Sub`**（静态组合，见设计文档 §5「图即节点」）：子图在 **`Load` 期就装好**（一处挂载一张实例），父侧那个节点的 `Requires` / `Provides` 由 `in` / `out` 推出来，引擎的静态校验（来源 / 环 / 名额）照常生效。由此：

- **装完不运行就能看完整棵树**：`g.Topology()` 沿静态挂载边递归，`Nodes[i].Sub` 就是这一处的子图（再深一层继续 `.Sub`），路径按**挂载位置**推导（`step1`、`step1/toInner`），图身份是 `<根 id>/<挂载路径>`；
- **子图的静态毛病在 `Load` 期报**，报文点明**挂载路径**：`pulse/yaml: graph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides or seeds it`；
- **同一张 spec 挂几处 = 几张独立实例**：每处都有自己的槽位与挂载路径，两处都跑得了，观测里靠 graph id 与 `pulse.path` 分得开；
- **取消从挂载点传进子图**：父图 ctx 一取消，子图节点当场看见——不是跑满整段再把取消吞掉。

规则：

- **边界写在接线处**：`in` / `out` 一律「**子键: 父键**」；子图节点**不能**再写 `requires` / `provides`——边界只有一处说法。
- **类型在 Load 期比**：两端各按 `{name, type}` 查表，而且用**同一个** `type` 记号——父侧那条键登记成别的类型，装图期就报。Go 那侧是编译期红，这一侧是装图期红。
- **名字在 Load 期比**：引用一张不存在的子图、绑定一条子图里没有任何节点声明的键、子图没有节点——三条都点名报错。
- **引用环在 Load 期拦**：子图里再挂子图是允许的、深度不设限；`a → b → a` 这种装不出图的环会被拦，并给出一条**具体**的路径（与引擎报依赖环同形）。
- **边界只有一处说法**：子图节点**不能**再写 `requires` / `provides`；反过来，**工厂节点写了 `in` / `out` 也在装图期报**——写错那一侧就是静默不生效。
- **`in:` 喂的那条子侧键不能被子图自己再提供** → 装图期报：一条键恰好一个来源，跑到那一步就是 `ErrDuplicateSource`。
- **透传**：同一条子侧键**同时**挂在 `in:` 与 `out:`（子图里没有节点产出它）——与引擎那边「同名入口 + 出口是透传」是同一条规则（`problemsOutputsLocked`）。父侧喂进去的值就是这条出口的值，所以「内部得有人读它 / 有人产出它」两条对透传不适用；**但内部有人产出它仍要拒**（上一条：同一个槽两个来源）。
- **子图的 `seeds`**：**要种值**的那条只允许 `literal`（`env` / `file` / `context` 要靠宿主 IO，而 `SeedPlan` 是**父图**的产物——装图期就报，不留到跑到一半；`skip: true` 的那条 `from` 没有语义，与顶层 `seeds` 一个口径）。键名 / 类型按登记表对账；**同一条键在同一张图里声明两次**也报（第二次会被幂等首写静默忽略，谁赢取决于声明顺序）。
- **子图自己 seed 了某条键、父侧又用 `in` 喂它** → 装图期报（两次种同一条槽，第二次会被幂等首写静默忽略，而两处声明看着都有道理）。
- **每一张 `graphs:` 声明都查**，被引用的那些**按引用点各查一遍**：装配先装一棵**只装不跑**的校验树（同一段装配代码，子图一律按静态形态挂上去），每个引用点各是一张实例——于是「子图里这条键有没有来源」正是**按引用点**成立的：同一张 spec 挂两处、只有一处没喂那条 `in:`，报的就是没喂的那一处（按并集校验等于说「另一处喂了 = 这一处也喂了」）。每张实例的接口面按**那一处**的 `in:` / `out:` 声明成端口（`DeclareInput` / `DeclareOutput`），所以「这本来就是外部输入」与「你忘了接线」分得开；判据直接用引擎那套（`Graph.Validate`，见设计文档 §5），**报文与 `Start` 那次逐字相同**——包括新加的那条：`out:` 里声明了、子图里却没人产出的出口，装图期就报，不必等它跑完才说 `never arrived`。
- **谁也引用不到的 spec 不查来源**：它**没有挂载点**，因此**没有接口面**——里面那些「没人提供」的键，究竟是「等将来挂上去时由那一处的 `in:` 喂」还是「名字写错了」，装图期判不了（按闭合图去查会把前者一律报成后者，模板库里只要有一张暂时没接上，整份文档就 `Load` 不过）。所以这类 spec 只查**真判得了**的那几条：工厂名 / 空 `nodes` / 重复 id / 来源冲突 / 依赖成环 / 流式那批照旧，来源那条等它**真的被引用**时按**那一处**的 `in:` / `out:` 判。
- **校验树不碰宿主的 ctx，子图定义也不碰**：校验树整棵建在 `context.Background()` 上；真图里子图的定义同样不挂宿主 ctx（取消是运行期从挂载点转发进去的）——挂在宿主 ctx 上只会让每一次 `Load` 都在宿主 ctx 上多挂几层。`Load` 失败时返回 `nil, nil, err`，调用方连清理的把手都没有，所以「所有可能失败的校验」都排在「建根图」前面；根图照旧继承宿主 ctx（取消得传得进去），宿主跑到 `Wait` 就把它释放。
- **根层自己的判据仍在 `Start` 那一刻报**：根层的外部输入是宿主在 `Load` 之后种进去的（`SeedPlan.Apply`），装图期看它当然是「没人提供、没人 seed」——任何一份 `seeds:` 正常的文档都会被误报掉。「声明归端口、值归运行输入」是阶段 2 随 `Execution` 落地的事。
- **切面**：子图节点能写 `timeout`，它落在**父侧那个节点**上——`timeout: 30s` 就是给整张子图限时；子图**里面**的节点各写各的。**但 `retry:` 落在子图节点上会在 `Load` 期被拒**：静态挂载的子图只跑一次（第一次尝试就把它用掉了），第二次尝试起不来；而 `Retry` 返回的是**最后一次** attempt 的错误——那句「这张图已经在用」会把第一次失败的真正病根盖掉。报文给出的替代写法是：把 `retry:` 放到子图里那个真会失败的节点上。「每次尝试各装一张新图」要等阶段 3 的事务性 `Retry`。**Go 侧同一个形状同样用不了**，只是拒的时机不同：`pulse.Aspect` 是裸函数类型、闭包没有可查的身份，装配期认不出它是不是 `Retry`，所以由 `Retry` 在**进 `Run` 的第一件事**当场拒（那时子图还没被认领）——两个入口一个 `Load` 期拒、一个进 `Run` 就拒，都不静默接受。

**观测按层给**：`LoadOptions.ObserverFor(path)` 按层建出口，`path` 是这一层的路径（根为空串、一层子图是它的节点 id、两层是 `outer/inner`）——把它交给 `observe.ObserveConfig.Path`，那一层的每条记录就都带 `pulse.path`。子图在 **`Load` 装到那一层时各调一次**（每处挂载一次，返回的出口只属于那一层）。返回 nil 表示这一层按 `LoadOptions.Graph` 里挂的那条走（子图会继承父图的观察者，只是不带层级）。只装不跑的校验树**不调**它——宿主收到的回调次数不会因为「多校验一遍」而变。

设计见 [`docs/design/pulse.md`](../docs/design/pulse.md) §5 装配。
