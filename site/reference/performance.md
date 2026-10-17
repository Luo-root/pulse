# 规模与性能

这份基线把同一串确定性整数计算交给不同 Go 引擎，观察节点数从 1 到 10000 时的成本。它面向正在选择执行库的开发者：既看小图固定开销，也看大图增长、复用收益与累计分配，不预设 Pulse 最快。

测量日期为 **2026-10-17**。Pulse 固定为 `v0.4.2-0.20261016143631-7d70e1e14771`，对应 main 集成提交 `7d70e1e1477192669d5008d47729e3c8c19a336f`；这是 v0.5.0 发布准备期间的公开开发版本，不是尚未创建的正式 tag。对照固定 Azure/go-workflow [v0.1.13](https://github.com/Azure/go-workflow/releases/tag/v0.1.13)、go-taskflow [v1.2.0](https://github.com/noneback/go-taskflow/releases/tag/v1.2.0)，使用各 tag 的公开 API。

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

![三种图形的构建并运行耗时](/performance/2026-10-17/dag-e2e-r1-time.svg)

![复用定义后的整图耗时](/performance/2026-10-17/dag-reuse-r1-time.svg)

![每个业务节点的摊销时间](/performance/2026-10-17/dag-e2e-r1-per-node.svg)

![整图累计 Go 分配](/performance/2026-10-17/dag-e2e-r1-allocations.svg)

![固定较多业务计算时的成本](/performance/2026-10-17/dag-e2e-r256-time.svg)

图表保留 wide、diamonds 和 rounds=256 的结果，不以这张长链表代替全部矩阵。点的横轴按倍数增加，并不预设耗时必须按指数增长。没有数值的点保留预算说明，不把未测成的性能画成零。

10000 节点的三种形状都保留了 5 个有效样本，不能只挑最有利的一张图：

| 10000 业务节点，rounds=1 | Pulse e2e / ms | Pulse reuse / ms | Taskflow e2e / ms | Taskflow reuse / ms |
|---|---:|---:|---:|---:|
| chain | 119 | 44.8 | 14.5 | 13.6 |
| wide | 121 | 30.9 | 8.41 | 6.92 |
| diamonds | 141 | 50.4 | 13.4 | 9.56 |

从 1000 到 10000 节点，业务规模增加 10 倍，Pulse 的 reuse 耗时在这三种图上约增 10–11 倍，e2e 约增 19–23 倍。这是本次区间的实测比例，不是复杂度证明。实际使用中，固定流程应复用 Graph，每轮新建 Execution；如果要把极轻的计算拆成上万节点，应先把这部分调度与分配预算算进去。这里没有测出 Taskflow 的低成本究竟有多少来自不同契约，不把差距全归因于某一项保证。

整组 DAG 保留 **1800 条计划样本**：1700 条成功、20 条案例超时、80 条后续未重复。20 个超时均来自 Azure 的运行模式测点；每个这样的配置保留首次超时和其余 4 条 `not_repeated`。没有 checksum 不符或采样器错误被当成成功。

原始资料：[样本 JSONL](https://github.com/Luo-root/pulse/blob/main/benchmarks/results/2026-10-17-dag/samples.jsonl) · [版本与环境清单](https://github.com/Luo-root/pulse/blob/main/benchmarks/results/2026-10-17-dag/manifest.json) · [CSV 汇总](https://github.com/Luo-root/pulse/blob/main/benchmarks/results/2026-10-17-dag/figures/summary.csv)。

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

报告层保留原始 JSONL、配置、模块版本/校验和、二进制 SHA256、机器说明、CSV 和 SVG。Go 的本次 JSON 清单没有给出 Origin；另附 `module-origins.json` 查询同版本公共代理的实际元数据，未提供/获取失败均明确标记。复跑不使用 `@latest`；改变并发、工具链、业务函数或后端，应另存矩阵。此基线没有修改调度、缩减必要语义或先优化后只展示有利数字。
