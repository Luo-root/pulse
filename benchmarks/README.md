# 整图规模与同类引擎对照

本目录回答：相同业务任务从 1、10、100、1000 增至 10000 个节点时，构建、运行和分配成本如何变化。基线与方案见 [#418](https://github.com/Luo-root/pulse/issues/418)，持久化组见 [Durable #37](https://github.com/Luo-root/pulse-durable/issues/37)。

`dag/` 是独立 Go 模块。对照库不会进入 Pulse 根模块的依赖闭包。Durable 与 SQLite 的对照在 Durable 仓库的独立 `benchmarks/workflow/` 模块中；两组共用本目录的采样与绘图脚本，不混成一张快慢排名。

英文说明：[README_en.md](README_en.md)。

## #422 优化候选的单独复测

[配置对照 CSV](reports/2026-10-17-optimization.csv)、[分阶段统计](reports/2026-10-17-optimization-stages.csv) 和[版本摘要](reports/2026-10-17-optimization.json) 保留这轮 1380 条成功样本的精简统计。30 组万节点验证、完整原始样本、12 对图和诊断日志另存外部证据包，不入源码仓。原始基线、首轮候选与最终候选各自保存，不跨轮混算。

三层代码为 [#423](https://github.com/Luo-root/pulse/pull/423)、[#424](https://github.com/Luo-root/pulse/pull/424)、[#425](https://github.com/Luo-root/pulse/pull/425)；最终被测提交为 `f6049c4aac4d2d5316e0ced5959dc7bc2a257b46`。当前 v0.5.1 发布候选的正式 SQLite、可靠执行和恢复结果见[统一生产性能对比](../site/reference/production-comparison.md)。

## 相同的业务任务

所有引擎使用同一索引、依赖、整数输入与输出。节点数是实际业务节点数，不用额外的空源或汇总节点凑图。

| 形状 | 依赖 | N=1 的含义 |
|---|---|---|
| `chain` | 每个节点依赖前一个节点 | 一个独立节点 |
| `wide` | N 个独立节点 | 一个独立节点 |
| `diamonds` | 层宽按 1、2、1、2 重复；上层所有节点连接下层所有节点，最后一层截断到恰好 N 个节点 | 第一层只有一个节点 |

每个节点的输入是 `1 + 所有前驱输出之和`，无前驱时为 1。以 uint64 自然溢出计算。业务函数固定为：

```text
x = input XOR ((id + 1) * 0x9e3779b97f4a7c15)
重复 rounds 次：
    x = x XOR (x >> 12)
    x = x XOR (x << 25)
    x = x XOR (x >> 27)
    x = x * 0x2545f4914f6cdd1d
返回 x
```

`rounds=1` 展示调度/装配开销占比较高的小任务，`rounds=256` 增加固定的业务计算。结果是所有末端节点输出的 uint64 求和。Go 适配器检查输出；Python 采样器另用独立实现验算输出、边数和关键路径，不接受只运行空回调却声称完成任务的结果。

直接 Go 计算只提供业务计算成本参照，不是第四个工作流引擎。共同成功路径不等于各库在错误、skip、数据交付、Retry 或观测方面提供相同保证。

## 固定版本与环境

| 对象 | 本次固定版本 |
|---|---|
| Pulse | `v0.4.2-0.20261016143631-7d70e1e14771` |
| Azure/go-workflow | `v0.1.13` |
| noneback/go-taskflow | `v1.2.0` |

不得使用 `@latest` 复跑此基线。`go.sum` 和采样 manifest 保留模块版本、下载校验和与二进制 SHA256；补充的 `module-origins.json` 再从同版本公共代理查询实际 Origin，未提供或获取失败的条目明确标记，不编造 Hash。版本仅表示此轮测量对象，不能据此推断其他版本的性能。

本机完整矩阵固定 `GOMAXPROCS=4` 和业务并发上限 4。线程数、节点并发限制和节点总数是不同维度；改变其中一项应另存一次矩阵。服务进程、编译器、race 或 profiler 不与正式样本并行运行。普通工作站的电源、温度与其他程序仍会带来波动，结果不构成线上 SLO。

## 构建与正确性

以下命令从本仓根目录执行；PowerShell 的多条命令逐行运行即可。

```sh
cd benchmarks/dag
go mod download
go build -o dag.exe .
go vet ./...
go test -race -count=1 -timeout=180s ./...
```

Linux/macOS 将输出名改为 `dag`。Windows PowerShell 可先设 `$env:GOWORK = 'off'`；Bash 用 `export GOWORK=off`。无需修改根模块或建立跨仓 replace。正式计时使用非 race 构建，race 仅用于正确性门禁。

## 采样和生成图表

先运行正确性，再从仓库根目录串行采样：

```sh
python benchmarks/tools/run_matrix.py --suite dag --module benchmarks/dag --binary benchmarks/dag/dag.exe --out benchmarks/results/local-dag --sizes 1,10,100,1000,10000 --rounds 1,256 --repeats 5 --procs 4 --parallelism 4
python benchmarks/tools/capture_origins.py benchmarks/results/local-dag/manifest.json --out benchmarks/results/local-dag/module-origins.json
python benchmarks/tools/summarize.py benchmarks/results/local-dag/samples.jsonl --out benchmarks/results/local-dag/figures
```

可通过 `--sizes 1,2,4,8,16,32,64` 改用 2 的幂。`--shapes`、`--modes`、`--engines` 可以缩小矩阵。主程序每次只跑一个 case；采样器按重复轮次轮换引擎，始终串行执行。输出目录已有数据时默认拒绝覆盖；`--resume` 只接受同一配置与同一二进制。

绘图需要 Python 的 matplotlib；采样与 CSV 汇总只需标准库，可用 `--no-plots`。仓库不会自动安装系统软件。通过 CLI 的 `-help` 查看当前适配器参数与原生 Go benchmark 入口。

Durable 的适配器在另一仓库，但使用相同采样脚本：

```sh
python benchmarks/tools/run_matrix.py --suite workflow --module ../pulse-durable/benchmarks/workflow --binary ../pulse-durable/benchmarks/workflow/workflow.exe --out ../pulse-durable/benchmarks/results/local-workflow --sizes 1,10,100,1000,10000 --rounds 1 --repeats 5 --procs 4 --parallelism 4
```

两个仓库的实际相对位置不同则传入绝对路径。文件后端的临时数据位于输出目录的 `work/`。原始 JSONL、完整环境清单、诊断日志和整套生成图保留在本地或单独的证据包，`benchmarks/results/` 不入源码仓；检查点、数据库和二进制同样不提交。仓库只保留复跑代码、`benchmarks/reports/` 的精简统计与版本摘要，以及站点实际引用的单份 PNG。

## 读懂数据

- `manifest.json`：固定配置、Go/OS、模块版本/校验和、二进制 SHA256 和源码状态。编译与模块下载发生在计时外。`module-origins.json` 是单独获取的补充元数据，保存其获取时间与未提供/失败状态。
- `samples.jsonl`：每个计划样本的原始 stdout/stderr、命令、退出状态及解析字段。主耗时由 Go 程序内部测量；`wall_seconds` 还包括进程启动和采样器等待，不能替换为同一指标。
- `summary.csv` / `summary.json`：按完整配置分组，给有效样本数、中位数与观察到的最小/最大值。重复批次均值不是单次请求的 p95/p99。
- SVG：整任务耗时、每节点摊销和累计 Go 分配曲线。误差棒只表示本次样本的 min–max，不是置信区间。

`bytes_per_op` 和 `allocs_per_op` 来自 Go 累计分配计数，不是峰值堆、RSS 或设备物理写出。每节点时间和吞吐由整任务时间换算，不能理解成链上各节点的独立延迟。

超时、超资源和不支持的点保留状态，无有效数据时不画数值。一次触及上限后，后续计划重复标记 `not_repeated`，不继续空耗同一预算，也不补零。各测点上限是测量边界，不是引擎合格线。输出错误或 checksum/拓扑不符会中止矩阵，原始异常样本仍保留。

计时边界、执行器复用条件和实际结果见[统一生产性能对比](../site/reference/production-comparison.md)；解释结果时请同时读取适配器的 `notes`。
