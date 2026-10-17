# #422 优化候选：最终同轮证据

本目录对应 [#422](https://github.com/Luo-root/pulse/issues/422) 的三步优化，英文见 [README_en.md](README_en.md)。它与 `../2026-10-17-dag/` 原始基线分别保存，不能跨轮拼接计算收益。结果与代价见[中文性能页](../../../site/reference/performance.md)。

## 被测代码

| 变体 | 精确 Core 提交 | 说明 |
|---|---|---|
| baseline | `7d70e1e1477192669d5008d47729e3c8c19a336f` | 原始公共伪版本 `v0.4.2-0.20261016143631-7d70e1e14771` |
| index | `3a4c890334786b6d64c6904e24bbbf57c6b86e84` | #423，第二个节点成功提交才创建 ID 索引 |
| validation | `e33b5cbb27d038fcf6594e402c604037f01dac4b` | #424，再加普通图静态检查复用和输入表分配优化 |
| optimized | `f6049c4aac4d2d5316e0ced5959dc7bc2a257b46` | #425，再加真正 nil 错误的零分配分类 |
| taskflow | `v1.2.0` | 与 baseline 使用同一个 benchmark 二进制，以 engine 参数选择 |

适配器/业务源码固定于 `3ac6dca7aa4d4325acc4e6b20a72d39a603a779f` 的 `benchmarks/dag`。候选不是已发布版本，只在项目外生成的 `-modfile` 中使用本地 Core replace；生产及正式 benchmark 的 `go.mod`/`go.sum` 未变。本目录不提交二进制或本地 replace 文件，准确准备命令及路径保留在 `prepare-commands.json`，二进制 SHA256 在 `manifest.json` 和 `acceptance.json`。

## 文件和口径

- `plan.json` / `samples.jsonl`：1380 个唯一计划身份及原始命令、stdout/stderr、退出码、结果字段。全部 ok。
- `summary.json`：276 个分组，每组 5 个成功样本，各指标的 min/median/max。
- `comparison.csv` / `comparison.json`：三图 × 五规模 × 两档计算 × 三模式共 90 个原版/最终候选/Taskflow 对照。两个中间阶段只额外测 chain/N10000/rounds1 的三模式。
- `regression-observations.json`：全部中位数慢超过 10% 的点（本轮为空），及两个候选 min–max 高于原版的点，未删除不利结果。
- `large-correctness.json`：5 变体 × 3 图 × 2 档计算的 30 组万节点逐节点次数、值、并发与两轮复用验证；它们在正式计时前运行。
- `figures/`：12 对 SVG/PNG；每对为一图形、一计算档的三个模式，分别画时间或累计 Go 分配，阴影是 5 个批次均值的 min–max。
- `checks/`：最终 Core build/vet/full race/依赖闭包/空白检查、变异对照和候选 Core 下 Durable 全量 race（包括进程退出/恢复）的原始证据。
- `tools/`：本次准备、串行轮换采样、汇总/绘图脚本原件。路径沿用本次工作树布局，不是安装程序；跨机器复跑应调整脚本中的工作树位置并新建输出目录。
- `previous-candidate/`：第一次正式候选矩阵的版本、raw、汇总和回退观察。它暴露 N=1 的额外索引分配后，生产实现才延迟建表；此目录只保留历史，不参与最终统计或计算收益。
- `evidence-checks.json`：本目录机器生成证据的 SHA256 清单及身份统计，不是测试执行器。

原始 `samples.jsonl` SHA256：`7e76785fb76a682f48f888c2f5fd34961d280722a5dfbef20d4ab1807d3458b3`。

时间是 Go 内部计时的每批均值再取中位数；进程 wall time 包含启动和采样器等待，不能混用。累计分配不是峰值堆/RSS，min–max 不是置信区间，模式或阶段中位数不能相减作为 profile 归因。没有装 Observer/observe 切面或日志 sink；引擎内建的状态与生命周期维护仍计入。

## 复跑最终候选

先检出上表精确提交，确认工作树干净。使用 Go 1.25.0、`GOWORK=off`、`GOFLAGS=-buildvcs=false`。从固定适配器的 `benchmarks/dag` 复制 `go.mod` / `go.sum` 到项目外同一目录，命名为 `candidate.mod` / `candidate.sum`；命令中的路径替换为本机绝对路径：

```text
go mod edit -modfile=<外部目录>/candidate.mod -replace=github.com/Luo-root/pulse=<候选Core目录>
go build -modfile=<外部目录>/candidate.mod -o <外部目录>/candidate.exe .
<外部目录>/candidate.exe -engine pulse -shape chain -nodes 10000 -rounds 1 -mode reuse -parallelism 4 -procs 4 -iterations 1 -min-time 1ms -timeout 60s -verify=true
<外部目录>/candidate.exe -engine pulse -shape chain -nodes 10000 -rounds 1 -mode reuse -parallelism 4 -procs 4 -iterations 0 -min-time 100ms -timeout 60s -verify=false
```

其余变体按 `prepare-commands.json` 构建；原版/Taskflow 不使用候选 replace。先完成全部 build/test/逐节点验证，再冻结二进制，按 `plan.json` 的顺序串行执行 raw 中保留的命令。用 `tools/measure_optimizations.py` 和同版本 `benchmarks/tools/run_matrix.py` 保留独立 uint64 oracle、Windows JobObject 进程看门狗、身份/退出码核对、原始输出和采前/采后 SHA。不要只照抄单条 CLI 就宣称已复现完整验收。

CLI 60s 整案例 context 与采样器 65s 看门狗均保留；准备、预热也消耗案例预算。先完成所有计时，再运行汇总与 matplotlib。新结果使用新目录，不覆盖本目录或原始基线；改变业务、工具链、并发或候选提交也另存一轮。
