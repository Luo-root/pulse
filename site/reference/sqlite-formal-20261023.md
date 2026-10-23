# 正式版 SQLite 与恢复测量（2026-10-23）

**COMPLETED_WITH_ERRORS**。这是面向 Core v0.5.1 发布准备的当前正式 SQLite 与恢复测量。仅测量 SQLite 可靠确认、Temporal 原生持久化基线和进程恢复；没有重跑完整 native、observe 或 resource 矩阵。不作 Go 升级带来加速的因果结论；所有 deadline、hard_timeout、not_equivalent 和 not_repeated 均按原始状态披露。

## 身份与方法

| 组件 | 实际固定身份 |
|---|---|
| 业务宿主 | Go 1.26.0 / windows/amd64 |
| Core source baseline | v0.5.0 formal release, measured for v0.5.1 preparation |
| Durable / 官方 sqlitestore | v0.1.0 / v0.1.0 |
| Eino | v0.9.21（原 pin） |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0（原 pin） |

2026-10-23 采样，无本地 replace。每配置计划 3 个独立样本，iterations=1：一次首次执行加一次复用执行，不是单轮计时覆盖整个案例。普通矩阵 rounds=1，N=1/10/100/1000/10000；业务并发及各进程 GOMAXPROCS=4。60s 案例预算包含 setup、first、reuse；65s 父看门狗限制整个进程树。首次不完成后保留 not_repeated，不补零、不重跑替换。

chain 是串行前驱链；wide 是 N 个无边独立任务，不是带共同首尾的 fan-out/fan-in；diamonds 是宽度 1、2 交替且相邻层全连接的窄分叉汇合图，N 是实际任务总数。三者不能只按 N 视为同一并行结构。

## 完成与错误

| 有效矩阵 | 计划身份 | 状态 |
|---|---:|---|
| checkpoint / Pulse | 45 | 36 ok、3 deadline、6 not_repeated |
| checkpoint / Eino | 45 | 15 not_equivalent、30 not_repeated |
| Temporal native | 45 | 30 ok、1 deadline、4 hard_timeout、10 not_repeated |
| recovery | 36 | 36 ok |

有效共 171 条。另保留 45 条 Temporal checkpoint 调用配置错误：该模式不受适配器支持，原始 checkpoint 文件共 135 条，剔除这些诊断后为 90 条。错误不是 Temporal 性能失败，亦不纳入性能统计；因此整轮不能标为全成功。Eino 普通成功不逐节点 checkpoint，not_equivalent 不是零成本或不支持持久应用的证明，主动中断恢复另测。

## 首次与复用：分别阅读，不作总排名

单位 ms；每格为中位数 [min–max]，n 仅统计完整 ok 样本。来自 JSON 的 first_ns 与 run_ns；iterations=1 时复用批次只有一轮。中位数不是线上 p50，范围不是置信区间。— 表示没有有效完成成本，绝非零。10000 的超时不能外推成单轮必定超过 60s 或引擎最大规模。

### Pulse + Durable 官方 SQLite：逐节点可靠确认

| shape / N | n | 首次 ms [min–max] | 复用 ms [min–max] | 状态 |
|---|---:|---|---|---|
| chain / 1 | 3 | 10.364200 [8.471900–11.266600] | 7.471000 [6.365900–10.390000] | ok=3 |
| chain / 10 | 3 | 54.447100 [53.846000–56.343500] | 53.536500 [51.450400–55.578800] | ok=3 |
| chain / 100 | 3 | 600.267700 [599.082100–610.809500] | 603.486300 [600.530300–615.435800] | ok=3 |
| chain / 1000 | 3 | 12525.316300 [12447.054900–13269.253900] | 14624.322000 [14287.211800–15754.115800] | ok=3 |
| chain / 10000 | 0 | — | — | deadline=1, not_repeated=2 |
| diamonds / 1 | 3 | 8.190700 [7.268500–9.051200] | 7.555100 [6.442200–8.790400] | ok=3 |
| diamonds / 10 | 3 | 58.358500 [54.990600–60.623600] | 57.345700 [51.630300–57.347000] | ok=3 |
| diamonds / 100 | 3 | 605.073000 [603.608900–649.888200] | 617.546100 [606.134100–673.631000] | ok=3 |
| diamonds / 1000 | 3 | 12985.821400 [12443.816000–13358.483400] | 13829.337400 [13772.147200–15070.546800] | ok=3 |
| diamonds / 10000 | 0 | — | — | deadline=1, not_repeated=2 |
| wide / 1 | 3 | 9.249400 [8.091800–9.359200] | 7.612800 [7.356700–8.520700] | ok=3 |
| wide / 10 | 3 | 55.805300 [53.114600–58.778600] | 54.483800 [47.481500–59.538900] | ok=3 |
| wide / 100 | 3 | 616.340100 [608.083400–778.549100] | 669.565200 [605.473100–671.984900] | ok=3 |
| wide / 1000 | 3 | 12879.575300 [12431.172700–13581.729100] | 14727.901900 [14027.857200–14899.504400] | ok=3 |
| wide / 10000 | 0 | — | — | deadline=1, not_repeated=2 |

### Temporal：原生服务持久化基线

这是 plain 模式中的原生事件历史及 Activity 确认，不是重跑不支持的 checkpoint 模式；也不与 Pulse 内存执行等价。

| shape / N | n | 首次 ms [min–max] | 复用 ms [min–max] | 状态 |
|---|---:|---|---|---|
| chain / 1 | 3 | 95.225400 [88.151700–117.517800] | 145.276700 [95.541900–228.674900] | ok=3 |
| chain / 10 | 3 | 526.301300 [514.445800–613.305400] | 245.812200 [231.687600–259.565600] | ok=3 |
| chain / 100 | 3 | 4225.259400 [4091.973300–4282.209300] | 5050.856600 [5045.190400–5061.768300] | ok=3 |
| chain / 1000 | 0 | — | — | hard_timeout=1, not_repeated=2 |
| chain / 10000 | 0 | — | — | deadline=1, not_repeated=2 |
| diamonds / 1 | 3 | 84.936900 [82.057900–89.767100] | 160.572800 [143.179300–167.099700] | ok=3 |
| diamonds / 10 | 3 | 470.463900 [466.056500–503.188700] | 189.810800 [183.122700–210.378300] | ok=3 |
| diamonds / 100 | 3 | 2566.570500 [2542.619200–2576.617800] | 3398.344300 [3394.475700–3398.855400] | ok=3 |
| diamonds / 1000 | 0 | — | — | hard_timeout=1, not_repeated=2 |
| diamonds / 10000 | 0 | — | — | hard_timeout=1, not_repeated=2 |
| wide / 1 | 3 | 90.837800 [84.933700–105.078500] | 129.734100 [127.996900–158.591400] | ok=3 |
| wide / 10 | 3 | 439.554000 [426.323400–486.264900] | 162.987400 [142.887500–179.507100] | ok=3 |
| wide / 100 | 3 | 1504.366700 [1422.477300–1653.597400] | 1582.763500 [1376.340000–1681.513100] | ok=3 |
| wide / 1000 | 3 | 12702.614500 [12275.964200–14018.747100] | 12981.282300 [12338.029500–13129.035500] | ok=3 |
| wide / 10000 | 0 | — | — | hard_timeout=1, not_repeated=2 |

## 恢复：四种语义分别保留

chain，rounds=256，N=10/100/1000，确认前缀分别 5/10/10，每配置 n=3，36 次全部 ok。Pulse 是确认前缀后硬杀宿主并在新进程 Resume；Eino 是主动 interrupt 保存 blob 后退出再恢复，不是任意 crash；Temporal runtime_kill 只杀 worker，server_restart 同时重开 worker 与 Server。只在无在途 Activity/外部副作用的确认边界测试，不承诺任意副作用 exactly-once。

wall 是控制器恢复墙钟；operation_ns 是子命令报告的操作范围，包含重新建图/打开 Store 或重建客户端等准备，不是纯后缀业务时间，Temporal 还包含控制命令/等待。两者不可混用，也不按跨语义 wall 排名。

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

服务重开与仅 worker 恢复存在反常快慢及较大范围；未做控制变量诊断，不推断重开服务使恢复更快。恢复只跑后缀，普通案例包含首次及复用两整轮，因此恢复成功不证明普通案例能在相同预算内完成。

## SQLite、setup 与证据边界

Pulse 的运行时 Load/Save、CAS 与本机运行独占均转发官方 sqlitestore v0.1.0；不是历史自制 SQLite Store。薄包装另开 inspection 连接作独立读取/故障注入，不承担运行时读写。该连接的打开和 PRAGMA 校验属于本次 adapter_setup_ns，不能把准备成本全算作引擎建图成本。Temporal server_setup_ns 与 Dial/注册/worker.Start 的 adapter_setup_ns 也不是同一种工作。

官方后端源码配置并读回 WAL/FULL（synchronous=2），inspection/Eino 连接也配置并读回；Temporal 启动参数请求 WAL/FULL。inspection 连接的 PRAGMA 读回不能单独证明另一连接的 synchronous，Temporal 启动参数也不是独立逐连接读回证据。此处是配置与源码层面的保证；进程硬杀不是断电/磁盘缓存丢失测试，未验收备份恢复、迁移、分布式 lease/fencing。DB/WAL/SHM 长度不是累计物理写出；本轮不能更新完整资源排名。

## 数据来源与复核

仓内精简数据：[summary.csv](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/summary.csv) · [identity.json](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/identity.json)。Temporal 原生组的 `kind` 标为 `native_persistence`，不与 Durable checkpoint 混同。

仓库外本轮证据根：`go126-release-20261023/`。`summary/identity.json` 的 57 组为本页数字源，与 `summary/summary.csv` 一一对应；`measurement-plan.json`、`measurement-source-identity.json` 固定公开模块、工具链及源文件哈希。`measurements/{checkpoint,temporal-native,recovery}/manifest.json` 与 `raw.jsonl` 保留身份、预算、全部失败/未重复记录。未复制原始数据库、日志或二进制进源码仓。

| raw.jsonl | SHA-256 |
|---|---|
| checkpoint | `d59f5116fe41cc87308d5be6724df55d18c679f4898f875a211d9325e436269d` |
| temporal-native | `610bb237276d167a968176d8fb33750538269a1cb8571786f511423a0d37e7b3` |
| recovery | `d0485cee9514d4d3f888b5a60eb9e0c5af5907202f0e88d774a264c53ba54d80` |
