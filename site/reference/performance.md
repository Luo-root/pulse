# 规模与性能

这份基线把同一串确定性整数计算交给不同 Go 引擎，观察节点数从 1 到 10000 时的成本。它面向正在选择执行库的开发者：既看小图固定开销，也看大图增长、复用收益与累计分配，不预设 Pulse 最快。

测量日期为 **2026-10-17**。Pulse 固定为 `v0.4.2-0.20261016143631-7d70e1e14771`，对应 main 集成提交 `7d70e1e1477192669d5008d47729e3c8c19a336f`；这是 v0.5.0 发布准备期间的公开开发版本，不是尚未创建的正式 tag。对照固定 Azure/go-workflow [v0.1.13](https://github.com/Azure/go-workflow/releases/tag/v0.1.13)、go-taskflow [v1.2.0](https://github.com/noneback/go-taskflow/releases/tag/v1.2.0)，使用各 tag 的公开 API。

## #422：保留语义的三步优化复测

以下是同日另一轮独立保存的候选提交实测，原始基线章节和数据保持原样。比较对象为原版 `7d70e1e1477192669d5008d47729e3c8c19a336f`、最终候选 `f6049c4aac4d2d5316e0ced5959dc7bc2a257b46` 和 Taskflow `v1.2.0`；候选不是发布 tag。收益全部用这轮重新测量的原版计算，不把下面历史基线的数字拼进来。

优化分三层提交，便于逐层审查：

1. [#423 节点 ID 索引](https://github.com/Luo-root/pulse/pull/423)：逐个 Add 原先反复扫描已有节点，10000 个无重名节点累计比较 49,995,000 次。改用只登记成功提交节点的私有索引。单节点不单独建表；成功提交使节点总数超过 1 时才建表并回填。整批校验完成后再登记，失败批次仍不占名字，错误优先级与声明顺序不变。
2. [#424 静态检查复用与输入表分配](https://github.com/Luo-root/pulse/pull/424)：已成功封口、没有 Sub/流声明的普通图复用不再变化的静态检查。每轮输入、宿主输入、checkpoint、动态值和恢复检查仍执行。无声明进口时不建必需输入表，有进口时按进口数预分配。快路仍扫描声明，不能理解为 O(1) Start；新建图的首次启动不会命中封口复用。
3. [#425 成功路径空错误分类](https://github.com/Luo-root/pulse/pull/425)：三处错误分类只对真正的 nil 接口直接返回 false。非 nil 路径仍执行原 `errors.As`，包括 typed nil、自定义 As、包装/Join 以及 Unwrap panic，保留原返回与异常行为。

没有调整调度模型、节点工作量、状态维护、正常 skip、取消优先、Retry 预算或 checkpoint 确认后发布规则。Graph 继续复用，每轮创建独立 Execution，根包运行依赖闭包仍仅标准库。

### 万节点结果与剩余差距

下表为 **N=10000、rounds=1** 的每图耗时中位数，单位 **ms**，每点 5 个批次均值。build 只建图，reuse 在预热定义上执行，e2e 每轮建图并运行；三种模式不能相减作 profile 归因。

| 图形 | 模式 | 本轮原版 Pulse | 最终候选 | 耗时下降 | Taskflow |
|---|---|---:|---:|---:|---:|
| chain | build | 75.994 | 6.193 | 91.9% | 1.520 |
| chain | reuse | 41.413 | 31.061 | 25.0% | 12.790 |
| chain | e2e | 119.783 | 45.484 | 62.0% | 15.020 |
| wide | build | 86.701 | 7.480 | 91.4% | 1.158 |
| wide | reuse | 23.002 | 21.255 | 7.6% | 5.761 |
| wide | e2e | 102.520 | 31.543 | 69.2% | 7.261 |
| diamonds | build | 91.330 | 6.585 | 92.8% | 1.674 |
| diamonds | reuse | 45.135 | 38.139 | 15.5% | 9.535 |
| diamonds | e2e | 130.238 | 51.355 | 60.6% | 11.828 |

主要进展是消除了大图装配时的反复查重；复用运行也有改善，幅度随图形变化。Taskflow 在这三个万节点轻任务中仍更快：候选 e2e 耗时仍约为它的 3.0–4.3 倍，不能据此声称已追平。各库的数据交付、错误与生命周期契约仍有差别，本轮没有测出剩余差距中每项契约占多少。

![最终候选与本轮原版及 Taskflow 的长链曲线](/performance/2026-10-17-optimization/chain-R1-time.png)

![独立节点宽图曲线](/performance/2026-10-17-optimization/wide-R1-time.png)

![连续分叉汇聚曲线](/performance/2026-10-17-optimization/diamonds-R1-time.png)

外部完整证据包保留三图、1/10/100/1000/10000 节点、rounds=1/256、build/reuse/e2e 和 12 对 SVG/PNG；源码仓仅留精简统计与页面引用的 4 张 PNG。上述万节点轻任务表不替代全部配置。

### 空间代价与变慢的点

索引以额外 O(N) 空间换取查重速度。万节点 build 的累计 Go 分配在 chain/wide/diamonds 分别由 **7.164/10.186/7.418MiB** 增至 **7.997/11.019/8.251MiB**，约多 **0.833MiB**。这是累计分配量，不能当作索引的存活大小、峰值堆或 RSS。

复用运行的累计分配则由 **23.581/20.652/24.085MiB** 降至 **17.449/16.905/18.104MiB**，分别下降 **26.0%/18.1%/24.8%**；e2e 累计分配分别下降 **7.7%/6.4%/5.9%**。

![长链的累计 Go 分配代价与收益](/performance/2026-10-17-optimization/chain-R1-allocation.png)

90 个原版/候选配置对照中，没有候选耗时中位数慢超过 10% 的点，但不等于每点都加速。两处观察到候选 min–max 整段高于原版：N=10、rounds=256 标签的 chain build **4.829→5.115μs（慢 5.9%）**，diamonds build **5.412→5.723μs（慢 5.7%）**。build 不执行计算体，rounds 在这里仅是配置标签。这两个点及全部其他配置保留在 `benchmarks/reports/2026-10-17-optimization.csv`，各阶段 min–max 另见阶段 CSV；原始回退诊断在外部证据包中。五个样本的范围不是置信区间。

首轮正式采样曾发现单节点多出索引分配，随后才改为第二个节点成功提交时建表，并对最终提交完整重跑。首轮矩阵单独保留，不与最终矩阵混算，也不删除它所暴露的问题。

### 分层证据、正确性与复跑

同轮另测了 chain/N10000/rounds1 的两个中间阶段。下表是累计阶段的实测中位数，不是逐层 profiler 占比；没有影响某模式的代码改动也可能出现采样波动。

| 阶段 | 精确提交前缀 | build / ms | reuse / ms | e2e / ms |
|---|---|---:|---:|---:|
| 原版 | `7d70e1e14771` | 75.994 | 41.413 | 119.783 |
| ID 索引 | `3a4c89033478` | 5.953 | 40.960 | 49.116 |
| 再加静态检查/输入表 | `e33b5cbb27d0` | 6.117 | 33.841 | 48.979 |
| 再加空错误分类 | `f6049c4aac4d` | 6.193 | 31.061 | 45.484 |

正式采样前，5 个变体 × 3 图 × 2 档计算的 **30 组万节点检查**验证了逐节点运行次数、值、最大业务并发和两轮定义复用。计时关闭逐节点探针，仍核对每次最终 checksum；本轮 **1380/1380 条采样全部 ok，276 个分组各有 5 个有效样本**。最终组合通过 build、vet、全量 race、根依赖闭包与空白检查；最终 Core 替换下 Durable 的全量 race 及真实进程退出/恢复也通过。定向用例和变异对照覆盖整批提交、静态/本轮事实边界、正常 skip、恢复拒绝和 nil/typed nil 的原行为，各层另经独立复审。

环境、业务和计时边界沿用下文；正式计时在本机编译/测试/绘图停止后串行轮换。原版与 Taskflow 使用同一 benchmark 二进制，以 engine 参数选择；三个候选只替换 Core。候选尚未发布，构建使用项目外生成的 `-modfile` 和本地源码 replace，正式生产及 benchmark 的 `go.mod`/`go.sum` 未改。精确提交、准备命令、采前/采后二进制 SHA256 和原始命令输出均保留。

精简资料：[全部配置 CSV](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-optimization.csv) · [各阶段 min/median/max](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-optimization-stages.csv) · [版本摘要](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-optimization.json) · [复跑方法](https://github.com/Luo-root/pulse/blob/main/benchmarks/README.md)。完整原始样本、各阶段日志、准备脚本和首轮候选另存外部证据包，不随源码仓分发。最终原始 JSONL SHA256：`7e76785fb76a682f48f888c2f5fd34961d280722a5dfbef20d4ab1807d3458b3`。

以下继续保留优化前的原始基线，数字对应原始采样轮次。

## 本机结果：小任务的引擎成本很明显

在这组很轻的计算里，Taskflow 的成本低于 Pulse；Pulse 也没有在所有规模领先 Azure。10000 节点长链可以完成，但定义复用对成本有明显影响，不能把“节点越拆越细”当成免费抽象。

以下为 **chain、rounds=1、e2e（每轮建图并运行）** 的每图耗时中位数，单位 **ms**。有数值的点均为 5 个有效样本；直接 Go 计算参照保留在图表及原始数据，不列作第四个引擎。

| 业务节点数 | Pulse | Taskflow | Azure/go-workflow |
|---:|---:|---:|---:|
| 1 | 0.00562 | 0.00236 | 0.00349 |
| 10 | 0.0464 | 0.0175 | 0.159 |
| 100 | 0.407 | 0.155 | 37.1 |
| 1000 | 6.25 | 1.56 | 案例预算触顶，无有效样本 |
| 10000 | 119 | 14.5 | 案例预算触顶，无有效样本 |

Pulse 的 10000 节点长链在 reuse 模式为 **44.8ms**，e2e 为 **119ms**；累计 Go 分配分别约 **23.4MiB / 31.5MiB**。同点 Taskflow 分别为 **13.6ms / 14.5ms**。这说明复用已有定义有实际价值；这些独立模式的中位数之差不等于经过 profiler 分解的“纯装配时间”。

![三种图形的构建并运行耗时](/performance/2026-10-17/dag-e2e-r1-time.png)

![复用定义后的整图耗时](/performance/2026-10-17/dag-reuse-r1-time.png)

![每个业务节点的摊销时间](/performance/2026-10-17/dag-e2e-r1-per-node.png)

![整图累计 Go 分配](/performance/2026-10-17/dag-e2e-r1-allocations.png)

![固定较多业务计算时的成本](/performance/2026-10-17/dag-e2e-r256-time.png)

图表保留 wide、diamonds 和 rounds=256 的结果，不以这张长链表代替全部矩阵。点的横轴按倍数增加，并不预设耗时必须按指数增长。没有数值的点保留预算说明，不把未测成的性能画成零。

10000 节点的三种形状都保留了 5 个有效样本，不能只挑最有利的一张图：

| 10000 业务节点，rounds=1 | Pulse e2e / ms | Pulse reuse / ms | Taskflow e2e / ms | Taskflow reuse / ms |
|---|---:|---:|---:|---:|
| chain | 119 | 44.8 | 14.5 | 13.6 |
| wide | 121 | 30.9 | 8.41 | 6.92 |
| diamonds | 141 | 50.4 | 13.4 | 9.56 |

从 1000 到 10000 节点，业务规模增加 10 倍，Pulse 的 reuse 耗时在这三种图上约增 10–11 倍，e2e 约增 19–23 倍。这是本次区间的实测比例，不是复杂度证明。实际使用中，固定流程应复用 Graph，每轮新建 Execution；如果要把极轻的计算拆成上万节点，应先把这部分调度与分配预算算进去。这里没有测出 Taskflow 的低成本究竟有多少来自不同契约，不把差距全归因于某一项保证。

整组 DAG 保留 **1800 条计划样本**：1700 条成功、20 条案例超时、80 条后续未重复。20 个超时均来自 Azure 的运行模式测点；每个这样的配置保留首次超时和其余 4 条 `not_repeated`。没有 checksum 不符或采样器错误被当成成功。

精简资料：[CSV 汇总](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-dag.csv) · [版本与原始数据摘要](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-dag.json)。完整原始样本、诊断清单和生成图集另存证据包，不随源码仓分发；复跑会在被忽略的 `benchmarks/results/` 生成它们。原始样本 SHA256 为 `fc31f4a71d8bd3cff4d85b6697a71935cb52982cf570593e115371c8ca689ffd`。

## 相同任务是什么

每个节点计算一个 uint64 小值。根输入为 1；有前驱时输入为 `1 + 所有前驱输出之和`，溢出遵循 uint64 语义。相同的 xorshift/multiply 函数分别重复 1 和 256 轮。最终结果是所有末端节点输出之和，Go 与独立 Python 实现都核对 checksum。

| 场景 | 实际接线 | 单节点退化 |
|---|---|---|
| chain | 每个节点依赖前一个节点 | 一个无前驱节点 |
| wide | 全部节点独立，受同一业务并发上限约束 | 一个无前驱节点 |
| diamonds | 层宽 1、2 交替；前层所有节点连接后层所有节点；末层截断为恰好 N 个节点 | 第一层的一个节点 |

没有额外空源或汇总节点。每条样本保存真实节点数、边数与以节点计的关键路径。比如 10000 节点的 chain/wide/diamonds 分别有 9999/0/13332 条边，关键路径为 10000/1/6667。直接 Go 计算是串行业务成本参照，不是第四个工作流引擎。

这更像把许多很小的算式交给引擎：算式本身很便宜，图装配、检查依赖、唤醒节点和传值容易成为主要成本。它不包含模型、远程 API、数据库调用或人工 sleep，也不能代替应用自身的负载测试。

## 三种计时要分别读

| 模式 | 计入 | 排除 |
|---|---|---|
| e2e | 每轮定义、适配器、executor 初始化，原生运行/校验，最终输出读取与 checksum 比较 | 公共任务规范、oracle、一次预热、样本前 GC、进程启动 |
| build | 定义、适配器、executor 装配；保留最后一个对象至计时结束 | 节点运行，以及只在 Start/Do/Run 才发生的隐式校验 |
| reuse | 已预热定义上的原生 run/reset、每轮运行态、缓冲清空与输出读取 | 定义/适配器/executor 初始装配 |

Pulse 每轮仍新建独立 Execution；Azure 的 Do reset/preflight 每轮计入；Taskflow 的原生 Run 准备每轮计入。没有只把 Pulse 的校验挪出计时。`build` 不是“已经可以执行的全部准备”，也不要把三个模式的中位数相加或相减当成精确分解。

Pulse 使用类型化 Get/Set 和声明出口 ReadOutput；对照库通过依赖顺序保护的业务值完成相同计算。三者的错误、skip、数据槽、Retry、观测契约不同，本轮只比较共同成功任务。Taskflow 没有池 shutdown/join 或 context 取消 API：Wait 确认业务任务完成，短暂 worker 收尾可能跨过边界；CLI 的进程级看门狗统一限制超时。这些差异随 JSON notes 保留。

## 对陡增结果的核对

Azure 适配器是普通叶子 Step，业务体只做共同的整数计算和数组读写，没有输出日志、复制整图或人为 sleep。固定版本的 [Workflow 源码](https://github.com/Azure/go-workflow/blob/v0.1.13/workflow.go)中，preflight 以反复扫描推进；tick 为各 Step 查询 StateOf/UpstreamOf，而 StateOf/RootOf 又遍历 root；[Traverse 实现](https://github.com/Azure/go-workflow/blob/v0.1.13/wrap.go)会构造遍历路径。这确认了原生路径上存在反复查找/遍历，不是业务计算在变重。读码不等于精确 profile，不据此报耗时占比，也不把旧 tag 的曲线推到 main 或其他版本。

## 机器、统计与边界

Intel Core i9-14900HX，24 核/32 逻辑线程，约 32GiB 内存，Windows 11 `10.0.26200`、amd64、Go `1.25.0`。固定 `GOMAXPROCS=4` 和业务并发上限 4。对照串行运行；正式计时不与本机编译、race、站点构建或 profiler 并跑。文件缓存未清空，工作站电源、温度与无关程序不受严格控制。

每个主要点计划 5 个样本。每样本在一次预热和 GC 之后累计计时至少 100ms；慢点可以是一轮。数据是重复批次均值的中位数，误差棒为本次观察到的 min–max，均不代表线上 p95/p99 或置信区间。极短任务的纳秒平均值不代表 Windows 时钟本身具有同等分辨率。N=1 时三种形状实际相同，分时采样出现的差值不能解释成拓扑效果，也提醒我们本机短测存在波动。

60s 子进程案例预算包含装配、预热和测量，另有 65s 父进程硬上限。因此“超时、无有效样本”不能翻译成“单次执行必定大于 60s”。一次触及上限后，后续计划重复保留为 `not_repeated`；失败、不支持和超时均不填零，也不从小规模外推 10000 节点成绩。

B/op 与 allocs/op 是进程累计 Go 分配差值/完成次数，不是峰值堆或 RSS。每节点时间与业务节点吞吐由整任务时间换算，不是链上单节点的独立延迟。MemoryStore、文件确认和进程恢复应读取 [Durable 性能报告](https://github.com/Luo-root/pulse-durable/blob/main/site/guide/performance.md)，不能用本页纯内存成绩替代。

## 复跑与核验

完整命令、业务函数、固定版本和适配器差异见[评测方法](https://github.com/Luo-root/pulse/blob/main/benchmarks/README.md)。`benchmarks/dag` 是独立模块，无 replace；对照依赖没有进入根模块，Pulse 根包运行依赖闭包仍仅标准库。完整计时手动运行，常规 CI 只做小规模正确性。

执行前通过 192 个小规模图/计算量/并发/引擎组合的逐节点检查及 race；direct/Pulse/Taskflow 的三种 10000 节点图也完成非 race 逐节点校验。独立复审进一步核对业务等价、复用、超时与采样器错误处理。超过预算的 Azure 点不因为没完成就被算成正确性通过。

完整证据包保留原始 JSONL、配置、模块版本/校验和、二进制 SHA256、机器说明、CSV 和图集；源码仓仅保留精简统计、版本摘要与报告实际引用的图。Go 的本次 JSON 清单没有给出 Origin；另附 `module-origins.json` 查询同版本公共代理的实际元数据，未提供/获取失败均明确标记。复跑不使用 `@latest`；改变并发、工具链、业务函数或后端，应另存矩阵。此基线没有修改调度、缩减必要语义或先优化后只展示有利数字。
