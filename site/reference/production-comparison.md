# Pulse／Durable、Eino、Temporal：统一对照

这轮为选择 Go 工作流基础设施的开发者保留同一组对照：**Pulse（可靠维度加 Durable）、Eino、Temporal**，贯穿执行、观测、资源、存储和恢复。当前页面只保留这一份性能对比；本轮正式 SQLite 与恢复数据已合入对应的可靠运行、存储和恢复章节。

内存执行、Eino 与 Temporal 的 2026-10-17 对照数据保持原样；SQLite 可靠确认、Temporal 原生持久化基线和进程恢复更新为 2026-10-23 的正式模块重测。两轮测量范围不同，不把新数据伪装成完整矩阵重跑，也不把 Go 1.26 与 Go 1.25 之间写成未经测量的速度收益。

## 版本、样本与保证

当前 SQLite 重测面向 Pulse v0.5.1 发布候选，Core 运行时源码与 v0.5.0 正式发布版本一致，工具链统一为 Go 1.26；Durable 与官方 sqlitestore 使用正式 v0.1.0 tag。没有本地 replace。

| 组件 | 固定版本 |
|---|---|
| Pulse Core source baseline | v0.5.0 formal release / v0.5.1 release candidate |
| Durable / official sqlitestore | v0.1.0 / v0.1.0 |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| SQLite driver | modernc.org/sqlite v1.60.1 |
| Formal SQLite benchmark host | Go 1.26.0 / windows/amd64 |


Windows 11 / i9-14900HX；各 Go 进程含独立 Server 的 GOMAXPROCS=4，业务并发=4，不是全进程合计四核的 CPU 亲和性。三图 chain/wide/diamonds，N=1/10/100/1000/10000，业务 rounds=1/256。一节点对应一个真实任务/常规 Activity；输入只由真实直接前驱值组成，终端结果独立校验。小规模先核逐节点值、次数、并发与复用，再冻结无 race 二进制串行轮换。

上表是 2026-10-17 的历史全维度矩阵，仅用于解释页面中保留的内存执行、观测和资源数据。当前正式 SQLite 重测的有效记录如下：

| 正式 SQLite 重测矩阵 | 计划身份 | 状态 |
|---|---:|---|
| checkpoint / Pulse | 45 | 36 ok、3 deadline、6 not_repeated |
| checkpoint / Eino | 45 | 15 not_equivalent、30 not_repeated |
| Temporal native | 45 | 30 ok、1 deadline、4 hard_timeout、10 not_repeated |
| recovery | 36 | 36 ok |

正式重测有效记录共 171 条，另保留 45 条 Temporal checkpoint 配置诊断；整体状态为 **COMPLETED_WITH_ERRORS**。`—` 是没有有效完成成本，绝非零；超时和未重复点不补零、不外推。历史全维度矩阵的 1062 条计划身份不与本轮正式 SQLite 数字拼接。

| 维度 | Pulse / Durable | Eino | Temporal |
|---|---|---|---|
| 普通执行 | Pulse 内存执行，每轮独立 Execution | compose Graph，AllPredecessor、唯一 output key | 真实 Server/worker，始终原生持久化 |
| 观测 | Observer 生命周期计数 | Graph/Lambda start/end/error callback | 非 replay 的 Workflow/Activity interceptor |
| 可靠确认 | Durable 经 Store 确认后发布 | 普通成功不自动逐节点保存；另用公开 CheckPointStore 主动中断 | 原生历史和 Activity 完成确认 |
| 恢复 | 已确认前缀后强杀宿主，新进程 Resume | 主动 checkpoint 后退出，新进程恢复 | 无在途 Activity 的前缀后强杀 worker；另测 Server 重开 |

Eino 的 semaphore 限制业务体，宿主负责错误后取消并等自身业务体，不宣称引擎所有内部 goroutine 已退出。Temporal 显式 MaximumAttempts=1，先等前驱再取 workflow semaphore，避免宽图提前堆上万在途 Activity。

## 普通执行与准备成本

下表 rounds=1，单位 ms，给每个配置5个重复批次均值的中位数。小计算的细粒度计时在 Windows 使用 QPC；完整 min–max、两档计算、首次与准备阶段均在 CSV。中位数不是线上 p50，min–max 不是置信区间。

| shape / N | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| chain / 1 | 0.0043375 | 0.0065289 | 253.826 |
| chain / 100 | 0.262107 | 0.613691 | 5049.45 |
| chain / 1000 | 2.81535 | 38.6123 | — |
| chain / 10000 | 31.2935 | 4867 | — |
| wide / 1 | 0.0043377 | 0.0062323 | 229.887 |
| wide / 100 | 0.170895 | 0.714113 | 1374.74 |
| wide / 1000 | 1.87012 | 39.9403 | 11183.6 |
| wide / 10000 | 19.2415 | 4824.91 | — |
| diamonds / 1 | 0.00406 | 0.0064852 | 225.973 |
| diamonds / 100 | 0.282539 | 0.834692 | 3400.07 |
| diamonds / 1000 | 2.87873 | 40.5372 | — |
| diamonds / 10000 | 34.214 | 4796.76 | — |


![普通执行复用成本](/performance/2026-10-17-production/native_reuse.png)

Temporal 的链/菱形 N=1000 及全部 N=10000 配置没有完成整案例；不能外推一个耗时或说结果错误。N=1000 宽图有有效结果。Pulse 与 Eino 的全部普通/观测计划都完成。万节点轻计算内存任务的差距不等于完整 Agent 应用同倍数更快；本轮没有 profile，因此不把曲线拐点写成已证实的内部热点。

| N=100 chain / stage | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| server_setup_ns | 0 | 0 | 364.083 |
| adapter_setup_ns | 0.0648 | 0.4919 | 66.1348 |
| first_ns | 0.4939 | 1.035 | 4186.82 |
| run_ns | 0.262107 | 0.613691 | 5049.45 |


上表各列阶段职责不同：Pulse 是建图与首次校验，Eino 是装配/Compile，Temporal adapter setup 是连接/注册/worker.Start，Server另列。`first_ns` 从提交至确认正确结果，`run_ns` 是随后复用；禁止拿这些阶段中位数相减作耗时归因。Temporal 的原生持久化保留，不与内存执行解释成同一保证。

## 观测

| N=100 / shape | Pulse plain → observe (ms) | Eino plain → observe (ms) | Temporal plain → observe (ms) |
|---|---|---|---|
| chain | 0.262107 → 0.271002 | 0.613691 → 0.662909 | 5049.45 → 5052.55 |
| wide | 0.170895 → 0.173396 | 0.714113 → 0.741753 | 1374.74 → 1301.09 |
| diamonds | 0.282539 → 0.283872 | 0.834692 → 0.881922 | 3400.07 → 3398.72 |


![普通和观测的实际成本](/performance/2026-10-17-production/observability.png)

三家都是公开扩展接口上的原子计数，不写外部日志。事件覆盖与数量不同，差值不代表同一种观测功能的价格；部分 observe 中位数更低属于独立样本波动，不能宣称“加观测会加速”。CSV保留回调种类、数量和全部重复范围。

## 同工作量 CPU 与内存

资源另跑，每家都执行首轮加一次复用，含宿主启动、准备和GC；Temporal的独立服务也在 Windows Job 内。下表 chain/rounds=1，CPU单位秒，RSS单位MiB。

| chain / N | Pulse CPU s / RSS MiB | Eino CPU s / RSS MiB | Temporal CPU s / RSS MiB |
|---|---|---|---|
| 1 | 0.015625 / 18.3594 | 0.015625 / 17.793 | 0.609375 / 170.289 |
| 100 | 0.015625 / 18.3906 | 0.03125 / 17.7969 | 2.53125 / 213.496 |
| 1000 | 0.015625 / 18.4102 | 0.140625 / 26.9883 | — / — |


![相同工作量的CPU和采样RSS](/performance/2026-10-17-production/resources.png)

Job CPU包含已退出后代；本机进程CPU计数有15.625ms级量化，小值持平不能推断为真正相同。RSS是每25ms采样的同时进程树峰值，短进程可能漏过真实峰值。共用二进制链接全部适配器，RSS含共同宿主底座，不是各框架最小部署占用。未完成点的资源只在raw保留，不混入完成成本。

CSV另保留复用期累计Go分配；Temporal该数字不含独立Server，不能拿来做全引擎内存排名。累计分配、活堆与RSS是不同指标。资源矩阵与吞吐矩阵批长不同，不混用它们的CPU总量。

## 可靠运行与存储增长

正式 SQLite 重测的 Pulse + Durable 可靠确认如下，单位 ms；每格为首次/复用中位数，n 只统计完整 ok 样本。Eino 普通成功没有同义的逐节点 checkpoint，Temporal 此处只列原生持久化基线，不把三者压成单一排名。

| shape / N | Pulse + Durable first / reuse (ms) | status |
|---|---|---|
| chain / 1 | 10.364200 / 7.471000 | ok=3 |
| chain / 10 | 54.447100 / 53.536500 | ok=3 |
| chain / 100 | 600.267700 / 603.486300 | ok=3 |
| chain / 1000 | 12525.316300 / 14624.322000 | ok=3 |
| chain / 10000 | — / — | deadline=1, not_repeated=2 |
| diamonds / 1000 | 12985.821400 / 13829.337400 | ok=3 |
| wide / 1000 | 12879.575300 / 14727.901900 | ok=3 |
| wide / 10000 | — / — | deadline=1, not_repeated=2 |

官方 sqlitestore v0.1.0 配置并读回 WAL/FULL；运行时 Load/Save、CAS 与本机运行独占均使用该正式 Store。检查连接、PRAGMA 校验和 setup 成本不等同于引擎建图成本。进程硬杀不是断电测试，未验收备份恢复、迁移、分布式 lease/fencing；DB/WAL/SHM 长度不是累计物理写出。

## 跨进程恢复

正式重测恢复场景使用 chain、rounds=256、N=10/100/1000，确认前缀为 5/10/10，每配置 n=3，36/36 完成。Pulse 是确认前缀后强杀宿主并在新进程 Resume；Eino 是主动 interrupt 保存 blob 后恢复；Temporal 分 runtime_kill 与 server_restart。只在无在途 Activity/外部副作用的确认边界测试，不承诺任意副作用 exactly-once。

| 引擎 / 场景 / N | n | wall s [min–max] | operation ms [min–max] |
|---|---:|---|---|
| eino / active_interrupt / 10 | 3 | 0.049907 [0.046073–0.050180] | 24.372800 [21.061400–24.645800] |
| eino / active_interrupt / 100 | 3 | 0.046845 [0.044777–0.054857] | 21.419600 [21.387700–25.076200] |
| eino / active_interrupt / 1000 | 3 | 0.134673 [0.130496–0.136944] | 103.711600 [103.558600–108.297300] |
| pulse / runtime_kill / 10 | 3 | 0.072193 [0.065890–0.074435] | 19.992000 [19.426300–23.130100] |
| pulse / runtime_kill / 100 | 3 | 0.335978 [0.303561–0.389495] | 286.402000 [276.070700–328.842300] |
| pulse / runtime_kill / 1000 | 3 | 9.886758 [9.355772–10.618498] | 9804.680500 [9303.068600–10548.256300] |
| temporal / runtime_kill / 10 | 3 | 5.199157 [5.188746–5.213046] | 5134.301200 [5111.029900–5134.654700] |
| temporal / server_restart / 10 | 3 | 10.167985 [0.713383–10.179320] | 9884.872200 [441.128800–9916.820000] |
| temporal / runtime_kill / 100 | 3 | 8.647032 [8.632625–13.595738] | 8578.779300 [8567.382300–13526.297700] |
| temporal / server_restart / 100 | 3 | 4.080621 [4.029154–4.241825] | 3795.921100 [3757.966300–3846.748700] |
| temporal / runtime_kill / 1000 | 3 | 53.706356 [53.680285–53.717853] | 53615.510200 [53612.884800–53635.186500] |
| temporal / server_restart / 1000 | 3 | 49.185665 [49.084596–49.220536] | 48907.995200 [48819.051800–48933.869500] |

wall 是控制器恢复墙钟，operation 是子命令范围，包含重建 Graph/Store 或客户端等准备；两者不可混用，也不按跨语义 wall 排名。服务重开与仅 worker 恢复存在反常快慢及较大范围，未做控制变量诊断，不推断重开服务使恢复更快。

## 数据、复跑与检查

历史全维度数据仍由原有 `benchmarks/reports/2026-10-17-production/` 精简 CSV 和 identity 保留；当前正式 SQLite 数据见 [sqlite-formal-20261023 summary.csv](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/summary.csv) 与 [identity.json](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/identity.json)。仓库外证据根为 `go126-release-20261023/`，原始记录保留全部失败和未重复状态，未复制数据库、日志或二进制进源码仓。

正式 SQLite 重测已独立复算 102 条成功记录 checksum 和 57 组统计；恢复 36/36 完成。状态为 **COMPLETED_WITH_ERRORS**，不把旧 Go 1.25 数字改写成 Go 1.26 的升级收益，也不把本轮限定矩阵描述成完整性能重跑。

固定来源：[Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21)、[Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0)、[CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1)。
