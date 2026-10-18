# Pulse／Durable、Eino、Temporal：统一对照

这轮为选择 Go 工作流基础设施的开发者保留同一组对照：**Pulse（可靠维度加 Durable）、Eino、Temporal**，贯穿执行、观测、资源、存储和恢复。Go Taskflow 不参与本轮新对照；[旧基线与优化结果](./performance.md)按历史版本保留。

本机共同轻计算 DAG 中，Pulse 的内存执行成本最低；Eino 在本轮万节点图有明显增长；Temporal 保留持久化、服务调用和 Activity 调度成本。这些结果说明固定任务下的实际成本，不能把内存库和持久服务压成一张生产选型总排名。Eino 的组件/Agent 生态、Temporal 的部署与分布式运行能力不由这项短任务测试评分。

## 版本、样本与保证

测量于 2026-10-17，基于先清理并合并的公开最新版。Pulse `aa02dd107a43845b1790e9c2d1e1a41fe5a53839`、Durable `71c693fc82ef24731c84c2c0aa9edf4c02d1e63e`，不是新发的正式 tag；模块无本地 replace。

| 组件 | 固定版本 |
|---|---|
| Pulse | v0.4.2-0.20261017111210-aa02dd107a43 |
| Durable | v0.0.0-20261017105818-71c693fc82ef |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| Benchmark Go / Server build Go | go1.26.0 / go1.26.8 |
| Host SQLite driver | v1.60.1 |


Windows 11 / i9-14900HX；各 Go 进程含独立 Server 的 GOMAXPROCS=4，业务并发=4，不是全进程合计四核的 CPU 亲和性。三图 chain/wide/diamonds，N=1/10/100/1000/10000，业务 rounds=1/256。一节点对应一个真实任务/常规 Activity；输入只由真实直接前驱值组成，终端结果独立校验。小规模先核逐节点值、次数、并发与复用，再冻结无 race 二进制串行轮换。

| 实验 | 计划 / 保留 | 完成 | context到期 | 父硬超时 | 不再重复 |
|---|---|---|---|---|---|
| native | 900 / 900 | 800 | 1 | 19 | 80 |
| resources | 81 / 81 | 75 | 1 | 1 | 4 |
| checkpoint | 45 / 45 | 36 | 3 | 0 | 6 |
| recovery | 36 / 36 | 36 | 0 | 0 | 0 |


1062 条计划身份全部保留，234 组统计；947 完成、25 触及时间边界、90 不再重复，业务校验 error=0。普通执行/观测重复5批，其余重复3次。60s context 包含准备及首轮，65s父看门狗杀整个进程树；首次触限后剩余重复不再运行。`—` 是没有有效完成成本，绝非零；硬超时没有输出完整进度时也不推测完成数。

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

SQLite均配置 WAL/FULL，保留原生保存粒度；Pulse/Durable与Eino通过现有Store接口接入的SQLite属于评测宿主，不是新增正式后端。Durable整份RunRecord以JSON CAS保存，运行独占复用filestore系统锁，不提供分布式租约或fencing。Temporal是CLI内嵌真实单机Service；[官方生产部署](https://docs.temporal.io/production-deployment)仍需生产Service，本轮不是集群/HA或生产数据库测试。普通矩阵显式提高历史计数error至200000、warn至100000，不称默认配置。

长链rounds=1的可靠复用中位数如下，保持同三列。Durable独立重复3次；Temporal引用上面本轮原生持久化的同点数据（5批），没有伪造第二份同义实验。

| chain / N | Pulse + Durable (ms) | Eino | Temporal (ms) |
|---|---|---|---|
| 1 | 7.1326 | 另测主动中断 | 253.826 |
| 10 | 45.5552 | 另测主动中断 | 197.664 |
| 100 | 503.488 | 另测主动中断 | 5049.45 |
| 1000 | 8553.19 | 另测主动中断 | — |
| 10000 | — | 另测主动中断 | — |


Durable N=10000 三种图全部触及60s案例预算，剩余重复保留不再运行；这不是已验收的万步可靠规模。Eino该列表示本轮普通成功没有同义的逐节点确认测量，不是说扩展机制不能承载应用持久化。

N=100长链，DB/WAL/SHM文件长度中位数：Durable准备后45136B → 首轮后4230648B → 一次复用后4329168B；Temporal为2356456B → 5053944B → 5250648B。前者2个不同RunID，后者2份原生Workflow历史；包含初始元数据和WAL复用，不能用差值冒充累计写盘或每节点记录大小。Eino普通组没有启用Store，实际0B不表示相同可靠工作量的零成本；主动中断blob另在恢复场景验证。

## 跨进程恢复

长链rounds=256；N=10时确认前5个，N=100/1000确认前10个。Pulse由父进程另读SQLite和业务审计，Eino另读并哈希已保存blob，Temporal独立客户端读原生历史证明前缀已完成且无在途Activity，然后才退出/强杀。每场景3次，36/36完成，最终checksum与前缀不重跑均核对。

| N / prefix | Pulse host kill (s) | Eino active interrupt (s) | Temporal worker kill (s) | Temporal + Server restart (s) |
|---|---|---|---|---|
| 10 / 5 | 0.150567 | 0.0454763 | 5.17285 | 0.663576 |
| 100 / 10 | 0.293597 | 0.0549201 | 8.62182 | 4.02771 |
| 1000 / 10 | 4.69759 | 0.126953 | 53.6735 | 49.0806 |


![按不同保证区分的恢复成本](/performance/2026-10-17-production/recovery.png)

单位秒，从确认前缀后启动退出/重启计时，到新进程确认正确结果，包含剩余业务。CSV的operation_ns另列最后CLI操作，不含控制器重开进程的全部工作。Eino是主动中断检查点，不能冒充任意crash自动恢复；此处也不测在途副作用的恰好一次。

Temporal的“服务还活着”场景反而比服务重开慢，原始结果保留。退出后的任务路由/粘性缓存等只可作为待诊断原因，本轮未做profile或控制变量，不给因果结论，也不由这9组结果推荐生产上重启Server。普通案例需要首次加复用两整轮，恢复案例仅完成确认前缀后的后缀，所以某个N=1000恢复成功不与普通整案例超时冲突。

## 数据、复跑与检查

[精简CSV](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-production/summary.csv)覆盖234组，保留全部状态/空值/分配/存储/事件计数；[版本与哈希摘要](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-production/identity.json)固定公开提交、原始哈希和采样范围。[双语复跑入口](https://github.com/Luo-root/pulse/tree/main/benchmarks/production)包含测试与命令，图由summarize_production.py从保留raw重建。原始JSONL、stdout/stderr、完整机器/构建清单、诊断与数据库不入Git，完整证据另包交付；没有假造在线raw下载链接。

合并后最新版Core树核对、Durable配最新公开Core的build/vet/全量race及真实进程恢复通过；新适配完整race/vet、真实Temporal失败/取消复用/历史并发验证、采样器反例与独立审查通过。正式采前后同二进制和脚本哈希相同，四阶段串行执行。此页不覆盖旧Go1.25成绩，也不把本轮数值拼成升级收益。未创建正式tag/release。

固定来源：[Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21)、[Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0)、[CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1)。
