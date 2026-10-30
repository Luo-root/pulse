# Pulse／Durable、Eino、Temporal：统一对照

本文比较 Pulse（可靠执行使用 Durable）、Eino 和 Temporal 在执行、观测、资源、存储与恢复场景下的表现。普通执行、观测和资源数据采集于 2026-10-17；SQLite 可靠执行与恢复数据采集于 2026-10-23。

本站当前 API 基线为 v0.5.2；下列图表仍是上述日期与固定版本的历史测量，不代表 v0.5.2 的重新实测。本次验证与图算法优化未另跑同规模生产矩阵，不能从旧数据推算新版本性能。

## 测试配置

SQLite 重测使用 Go 1.26、Pulse Core v0.5.0 source baseline、Pulse Durable/sqlitestore v0.1.0、Eino v0.9.21 和 Temporal v1.50.0。所有测试均无本地 replace。

| 组件 | 固定版本 |
|---|---|
| Pulse Core source baseline | v0.5.0（实际采样版本） |
| Durable / official sqlitestore | v0.1.0 / v0.1.0 |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| SQLite driver | modernc.org/sqlite v1.60.1 |
| Formal SQLite benchmark host | Go 1.26.0 / windows/amd64 |


测试环境为 Windows 11 / i9-14900HX；各 Go 进程 GOMAXPROCS=4，业务并发为 4。测试图包括 chain、wide、diamonds，N=1/10/100/1000/10000；恢复测试使用 rounds=256。当前正式 SQLite 重测结果如下：

| 正式 SQLite 重测矩阵 | 计划身份 | 状态 |
|---|---:|---|
| checkpoint / Pulse | 45 | 36 ok、3 deadline、6 not_repeated |
| checkpoint / Eino | 45 | 15 not_equivalent、30 not_repeated |
| Temporal native | 45 | 30 ok、1 deadline、4 hard_timeout、10 not_repeated |
| recovery | 36 | 36 ok |

正式重测有效记录共 171 条，另有 45 条 Temporal checkpoint 配置诊断。未完成项以状态表示，缺少有效样本的数值记为 `—`。

| 维度 | Pulse / Durable | Eino | Temporal |
|---|---|---|---|
| 普通执行 | Pulse 内存执行，每轮独立 Execution | compose Graph，AllPredecessor、唯一 output key | 真实 Server/worker，始终原生持久化 |
| 观测 | Observer 生命周期计数 | Graph/Lambda start/end/error callback | 非 replay 的 Workflow/Activity interceptor |
| 可靠确认 | Durable 经 Store 确认后发布 | 普通成功不自动逐节点保存；另用公开 CheckPointStore 主动中断 | 原生历史和 Activity 完成确认 |
| 恢复 | 已确认前缀后强杀宿主，新进程 Resume | 主动 checkpoint 后退出，新进程恢复 | 无在途 Activity 的前缀后强杀 worker；另测 Server 重开 |

## 普通执行与准备成本

单位为 ms；数值为重复样本的中位数。

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

Temporal 的 chain/diamonds N=1000 及全部 N=10000 配置未完成；wide N=1000 有效。

| N=100 chain / stage | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| server_setup_ns | 0 | 0 | 364.083 |
| adapter_setup_ns | 0.0648 | 0.4919 | 66.1348 |
| first_ns | 0.4939 | 1.035 | 4186.82 |
| run_ns | 0.262107 | 0.613691 | 5049.45 |


阶段定义：Pulse 为建图与首次校验，Eino 为装配/Compile，Temporal adapter setup 为连接、注册和 worker.Start；Server 准备单列。`first_ns` 为首次执行，`run_ns` 为复用执行。

## 观测

| N=100 / shape | Pulse plain → observe (ms) | Eino plain → observe (ms) | Temporal plain → observe (ms) |
|---|---|---|---|
| chain | 0.262107 → 0.271002 | 0.613691 → 0.662909 | 5049.45 → 5052.55 |
| wide | 0.170895 → 0.173396 | 0.714113 → 0.741753 | 1374.74 → 1301.09 |
| diamonds | 0.282539 → 0.283872 | 0.834692 → 0.881922 | 3400.07 → 3398.72 |


![普通和观测的实际成本](/performance/2026-10-17-production/observability.png)

观测数据使用公开扩展接口和原子计数；各框架的事件覆盖不同，数值不代表完全相同的观测能力。

## 同工作量 CPU 与内存

资源另跑，每家都执行首轮加一次复用，含宿主启动、准备和GC；Temporal的独立服务也在 Windows Job 内。下表 chain/rounds=1，CPU单位秒，RSS单位MiB。

| chain / N | Pulse CPU s / RSS MiB | Eino CPU s / RSS MiB | Temporal CPU s / RSS MiB |
|---|---|---|---|
| 1 | 0.015625 / 18.3594 | 0.015625 / 17.793 | 0.609375 / 170.289 |
| 100 | 0.015625 / 18.3906 | 0.03125 / 17.7969 | 2.53125 / 213.496 |
| 1000 | 0.015625 / 18.4102 | 0.140625 / 26.9883 | — / — |


![相同工作量的CPU和采样RSS](/performance/2026-10-17-production/resources.png)

CPU 包含子进程；RSS 为进程树采样峰值。Temporal 的 CPU/RSS 包含独立 Server；Go 分配统计不包含该 Server。

## 可靠运行与存储增长

Pulse + Durable 可靠确认结果如下，单位 ms；每格为首次/复用中位数。

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

官方 sqlitestore v0.1.0 使用 WAL/FULL；Pulse 的 Load/Save、CAS 和本机运行独占均由该 Store 提供。该测量不覆盖备份恢复、迁移或分布式 lease/fencing。

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

wall 为控制器恢复墙钟，operation 为子命令耗时，包含 Graph/Store 或客户端重建。两者不可混用。

## 数据、复跑与检查

历史全维度数据仍由原有 `benchmarks/reports/2026-10-17-production/` 精简 CSV 和 identity 保留；当前正式 SQLite 数据见 [sqlite-formal-20261023 summary.csv](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/summary.csv) 与 [identity.json](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/identity.json)。仓库外证据根为 `go126-release-20261023/`，原始记录保留全部失败和未重复状态，未复制数据库、日志或二进制进源码仓。

正式重测状态为 **COMPLETED_WITH_ERRORS**；恢复场景 36/36 完成。

固定来源：[Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21)、[Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0)、[CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1)。
