# v0.5.0 发布前重测：Pulse／Durable、Eino、Temporal

这轮为选择 Go 工作流基础设施的开发者保留同一组对照：**Pulse（可靠维度加 Durable）、Eino、Temporal**，贯穿执行、观测、资源、存储和恢复。Go Taskflow 不参与本轮新对照；[旧基线与优化结果](./performance.md)按历史版本保留。

本页由 v0.5.0 发布前新采样生成，报告相同轻计算 DAG 下的执行、资源、可靠确认和恢复成本。各框架的原生保证保留，不把内存库和持久服务压成一张生产选型总排名；Eino 的 Agent/组件生态和 Temporal 的分布式部署能力不由此短任务测试评分。

## 版本、样本与保证

采样时间：北京时间 2026-10-21 23:16 至 2026-10-22 01:13（UTC：2026-10-21 15:16–17:13）。Pulse 候选提交 `be778722b781ad97f2846848dcd9dacdc0e501e7`、Durable 配套提交 `5db6eae0e9fb1cb743b56027b1cdadeb03736d1c`；固定公开 pseudo-version，无本地 replace。**测量时尚未创建 v0.5.0 tag，本页不表示版本已发布。** 只有最终 tag 的引擎源码与此候选一致时，这组成绩才适用于该版本；否则重测。

| 组件 | 固定版本 |
|---|---|
| Pulse | v0.4.2-0.20261021081643-be778722b781 |
| Durable | v0.0.0-20261021085955-5db6eae0e9fb |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| Benchmark Go / Server build Go | go1.26.0 / go1.26.8 |
| Host SQLite driver | v1.60.1 |


Windows 11 / i9-14900HX；各 Go 进程含独立 Server 的 GOMAXPROCS=4，业务并发=4，不是全进程合计四核的 CPU 亲和性。三图 chain/wide/diamonds，N=1/10/100/1000/10000，业务 rounds=1/256。一节点对应一个真实任务/常规 Activity；输入只由真实直接前驱值组成，终端结果独立校验。小规模先核逐节点值、次数、并发与复用，再冻结无 race 二进制串行轮换。

| 实验 | 计划 / 保留 | 完成 | context到期 | 父硬超时 | 不再重复 | 错误 |
|---|---|---|---|---|---|---|
| native | 900 / 900 | 784 | 6 | 19 | 90 | 1 |
| resources | 81 / 81 | 75 | 0 | 2 | 4 | 0 |
| checkpoint | 45 / 45 | 36 | 3 | 0 | 6 | 0 |
| recovery | 36 / 36 | 36 | 0 | 0 | 0 | 0 |


1062 条计划身份逐一核对，无缺失、无重复；234 组统计，931 完成、30 触及时间边界、100 不再重复，1 次测量错误，成功结果 checksum 错误=0。普通执行/观测计划重复5批，其余计划重复3次。60s context 包含准备及首轮，65s父看门狗杀整个进程树；首次触限后剩余重复不再运行。`—` 是没有有效完成成本，绝非零；硬超时没有输出完整进度时也不推测完成数。

| 维度 | Pulse / Durable | Eino | Temporal |
|---|---|---|---|
| 普通执行 | Pulse 内存执行，每轮独立 Execution | compose Graph，AllPredecessor、唯一 output key | 真实 Server/worker，始终原生持久化 |
| 观测 | Observer 生命周期计数 | Graph/Lambda start/end/error callback | 非 replay 的 Workflow/Activity interceptor |
| 可靠确认 | Durable 经 Store 确认后发布 | 普通成功不自动逐节点保存；另用公开 CheckPointStore 主动中断 | 原生历史和 Activity 完成确认 |
| 恢复 | 已确认前缀后强杀宿主，新进程 Resume | 主动 checkpoint 后退出，新进程恢复 | 无在途 Activity 的前缀后强杀 worker；另测 Server 重开 |

Eino 的 semaphore 限制业务体，宿主负责错误后取消并等自身业务体，不宣称引擎所有内部 goroutine 已退出。Temporal 显式 MaximumAttempts=1，先等前驱再取 workflow semaphore，避免宽图提前堆上万在途 Activity。

本轮保留1次 Temporal setup 建连失败：plain/chain/N=1/rounds=1 的 repeat=3 在业务执行前连接 127.0.0.1:59472 被拒绝，0次迭代、无首轮/复用耗时及 checksum；根因未定，不归因于任一引擎。该点另外4次完成，表中 n=4。失败没有补零或被重跑结果替换；采样与汇总 exit=1 原样保留，结构完整性通过不等于所有实验成功。

## 普通执行与准备成本

下表 rounds=1，单位 ms，每个配置计划5个重复批次，以有效完成批次均值取中位数；不足5次时标明 n。小计算的细粒度计时在 Windows 使用 QPC；完整 min–max、两档计算、首次与准备阶段均在 CSV。中位数不是线上 p50，min–max 不是置信区间。

| shape / N | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| chain / 1 | 0.004523 | 0.0061328 | 257.098 (n=4) |
| chain / 100 | 0.269892 | 0.603431 | 5046.78 |
| chain / 1000 | 2.73028 | 38.0955 | — |
| chain / 10000 | 31.661 | 5020.85 | — |
| wide / 1 | 0.0043671 | 0.0061426 | 141.126 |
| wide / 100 | 0.166703 | 0.700557 | 1577.25 |
| wide / 1000 | 1.686 | 38.6783 | 13092.5 |
| wide / 10000 | 19.3844 | 4876.8 (n=2) | — |
| diamonds / 1 | 0.0044405 | 0.006177 | 190.714 |
| diamonds / 100 | 0.269941 | 0.850725 | 3399.2 |
| diamonds / 1000 | 2.86546 | 39.9722 | — |
| diamonds / 10000 | 34.2804 | 4919.04 | — |


![普通执行复用成本](/performance/v0.5.0-candidate-20261021/native_reuse.png)

完成情况按本轮 CSV 中每个配置的状态判断，未完成案例不外推耗时，也不因此称输出错误。轻计算 DAG 的差距不等于完整 Agent 应用同倍数更快；本轮没有 profile，不把曲线拐点写成已证实的内部热点。

| N=100 chain / stage | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| server_setup_ns | 0 | 0 | 416.315 |
| adapter_setup_ns | 0.0683 | 0.4674 | 72.8935 |
| first_ns | 0.6663 | 1.0291 | 4187 |
| run_ns | 0.269892 | 0.603431 | 5046.78 |


上表各列阶段职责不同：Pulse 是建图与首次校验，Eino 是装配/Compile，Temporal adapter setup 是连接/注册/worker.Start，Server另列。`first_ns` 从提交至确认正确结果，`run_ns` 是随后复用；禁止拿这些阶段中位数相减作耗时归因。Temporal 的原生持久化保留，不与内存执行解释成同一保证。

## 观测

| N=100 / shape | Pulse plain → observe (ms) | Eino plain → observe (ms) | Temporal plain → observe (ms) |
|---|---|---|---|
| chain | 0.269892 → 0.271658 | 0.603431 → 0.64464 | 5046.78 → 5044.3 |
| wide | 0.166703 → 0.163291 | 0.700557 → 0.709335 | 1577.25 → 1529.96 |
| diamonds | 0.269941 → 0.276178 | 0.850725 → 0.90326 | 3399.2 → 3397.31 |


![普通和观测的实际成本](/performance/v0.5.0-candidate-20261021/observability.png)

三家都是公开扩展接口上的原子计数，不写外部日志。事件覆盖与数量不同，差值不代表同一种观测功能的价格；部分 observe 中位数更低属于独立样本波动，不能宣称“加观测会加速”。CSV保留回调种类、数量和全部重复范围。

## 同工作量 CPU 与内存

资源另跑，每家都执行首轮加一次复用，含宿主启动、准备和GC；Temporal的独立服务也在 Windows Job 内。下表 chain/rounds=1，CPU单位秒，RSS单位MiB。

| chain / N | Pulse CPU s / RSS MiB | Eino CPU s / RSS MiB | Temporal CPU s / RSS MiB |
|---|---|---|---|
| 1 | 0.015625 / 18.7422 | 0.015625 / 18.6445 | 0.71875 / 163.582 |
| 100 | 0.015625 / 18.3477 | 0.03125 / 18.4883 | 4.32812 / 215.613 |
| 1000 | 0.03125 / 18.4102 | 0.15625 / 27.1953 | — / — |


![相同工作量的CPU和采样RSS](/performance/v0.5.0-candidate-20261021/resources.png)

Job CPU包含已退出后代；本机进程CPU计数有15.625ms级量化，小值持平不能推断为真正相同。RSS是每25ms采样的同时进程树峰值，短进程可能漏过真实峰值。共用二进制链接全部适配器，RSS含共同宿主底座，不是各框架最小部署占用。未完成点的资源只在raw保留，不混入完成成本。

CSV另保留复用期累计Go分配；Temporal该数字不含独立Server，不能拿来做全引擎内存排名。累计分配、活堆与RSS是不同指标。资源矩阵与吞吐矩阵批长不同，不混用它们的CPU总量。

## 可靠运行与存储增长

SQLite均配置 WAL/FULL，保留原生保存粒度；Pulse/Durable与Eino通过现有Store接口接入的SQLite属于评测宿主，不是新增正式后端。Durable将整份RunRecord编码为JSON，存入SQLite表的record BLOB列并以Revision做CAS；只复用filestore的系统锁，不调用其Load/Save，不额外写一份JSON运行文件，也不提供分布式租约或fencing。Temporal是CLI内嵌真实单机Service；[官方生产部署](https://docs.temporal.io/production-deployment)仍需生产Service，本轮不是集群/HA或生产数据库测试。普通矩阵显式提高历史计数error至200000、warn至100000，不称默认配置。

长链 rounds=1 的可靠复用中位数如下，保持同三列。Durable 独立计划重复 3 次；Temporal 引用上面本轮原生持久化的同点数据（计划 5 批，N=1 实际有效 4 批），没有伪造第二份同义实验。

| chain / N | Pulse + Durable (ms) | Eino | Temporal (ms) |
|---|---|---|---|
| 1 | 7.6359 | 另测主动中断 | 257.098 (n=4) |
| 10 | 53.8247 | 另测主动中断 | 230.261 |
| 100 | 551.431 | 另测主动中断 | 5046.78 |
| 1000 | 12588.3 | 另测主动中断 | — |
| 10000 | — | 另测主动中断 | — |


可靠规模是否完成，以本轮 checkpoint 行的有效样本与状态为准；空值表示没有完整案例成绩，不接受为已完成的可靠规模。Eino 该列表示普通成功没有同义的逐节点确认测量，不是说其扩展机制不能承载应用持久化。

N=100 长链的 DB/WAL/SHM 文件长度中位数（准备 → 首轮 → 复用）：Durable 45,136 B → 4,230,648 B → 4,329,168 B；Temporal 2,356,456 B → 5,053,944 B → 5,250,648 B。两者保留两个 RunID/Workflow 历史，并含初始元数据和 WAL 复用；差值不是累计物理写盘或每节点记录大小。Eino 普通组未启用 Store，实际 0B 不是同等可靠工作量的零成本；主动中断 blob 另在恢复场景验证。

## 跨进程恢复

长链rounds=256；N=10时确认前5个，N=100/1000确认前10个。Pulse由父进程另读SQLite和业务审计，Eino另读并哈希已保存blob，Temporal独立客户端读原生历史证明前缀已完成且无在途Activity，然后才退出/强杀。每场景3次，36/36完成，最终checksum与前缀不重跑均核对。

| N / prefix | Pulse host kill (s) | Eino active interrupt (s) | Temporal worker kill (s) | Temporal + Server restart (s) |
|---|---|---|---|---|
| 10 / 5 | 0.104242 | 0.0629073 | 5.20092 | 0.827797 |
| 100 / 10 | 0.338117 | 0.062967 | 8.6593 | 4.30853 |
| 1000 / 10 | 6.31158 | 0.143311 | 53.7078 | 49.3688 |


![按不同保证区分的恢复成本](/performance/v0.5.0-candidate-20261021/recovery.png)

单位秒，从确认前缀后启动退出/重启计时，到新进程确认正确结果，包含剩余业务。CSV的operation_ns另列最后CLI操作，不含控制器重开进程的全部工作。Eino是主动中断检查点，不能冒充任意crash自动恢复；此处也不测在途副作用的恰好一次。

服务仍运行与服务重启是两个独立恢复场景，不能由少量案例推导生产上应重启 Server。退出后的任务路由等原因需要另做控制变量诊断；本轮不作因果归因。普通案例包含首次加复用两整轮，恢复案例仅完成已确认前缀后的后缀，两者的超时/成功不能直接互换。

## 数据、复跑与检查

[精简CSV](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/v0.5.0-candidate-20261021/summary.csv)覆盖234组，保留全部状态/空值/分配/存储/事件计数；[版本与哈希摘要](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/v0.5.0-candidate-20261021/identity.json)固定公开提交、原始哈希和采样范围。[双语复跑入口](https://github.com/Luo-root/pulse/tree/main/benchmarks/production)包含测试与命令，图由summarize_production.py从保留raw重建。原始JSONL、stdout/stderr、完整机器/构建清单、诊断与数据库不入Git，完整证据另包交付；没有假造在线raw下载链接。

本轮适配器 vet/完整 race、真实 Temporal 服务测试、三个 Python 工具测试通过；正式采前后同一二进制及全部脚本哈希一致，四阶段串行执行，未并跑构建/测试/绘图。隔离模块缓存通过校验；Python 夹具使用短路径，外置工作树构建用 -buildvcs=false 并另存提交、差异和源码哈希。不改写历史成绩，也不拼接成升级收益。未创建正式 tag/release。

固定来源：[Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21)、[Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0)、[CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1)。

10 月 17 日对照按[原提交页面](https://github.com/Luo-root/pulse/blob/be778722b781ad97f2846848dcd9dacdc0e501e7/site/reference/production-comparison.md)和原始版本摘要保留；本页全部表格与图取自本轮新数据。
