# eval：工程能力 property test 套件

本包把 [`docs/design/agent-framework-evaluation.md`](../docs/design/agent-framework-evaluation.md) §5.2 的工程能力四主题落成**可执行断言**：随机输入序列 + 不变式检查（property test），固定 seed 可复现、零新依赖、`go test -race ./eval/` 一条命令全跑。

定位：这是评测三步走的**第二步**——「工程可靠性检查表」。第一步（基建 benchmark）与第三步（GAIA / tau-bench 接入）另票推进。与各包 `*_test.go` 的分工：包内测试覆盖固定用例，本包覆盖**不变式**（任意合法输入下性质必须保持），两者互补不重复。

## 运行

```powershell
go test -race -count=1 ./eval/    # 全套（约 10s）
$env:EVAL_SEED = "12345"          # 换种子探索新路径（默认 seed 固定，CI 与本地一致）
go test -race -run TestPropertySessionTornRecovery -v ./eval/
```

失败信息一律带 `seed=<N>`：设 `EVAL_SEED` 为该值即可回放。

## 检查表

每项 = 不变式 + 对应测试 + 竞品对照现状（截至 2026-09 的调研结论，见评测调研文档 §4.2）。

### 1. 崩溃恢复（memory/session）— `TestPropertySessionTornRecovery`

| # | 不变式 | 说明 |
|---|---|---|
| P1 | 撕裂识别 | 任意字节截断的 JSONL 在 Open 时被识别：合法前缀完整、损坏尾行丢弃，Open 不失败 |
| P2 | 合法前缀保持 | 回合边界截断后 surface 与基准逐条相等，节点数恰等于保留事件数（零合成事件泄漏） |
| P3 | fold 合法 | 任意截断后 Surface 可折叠——未闭合工具组由合成 `interrupted` 结果闭合，不产生孤儿 |
| P4 | 可续写 | 恢复后 Append 新事件成功且反映到 surface |
| P5 | 恢复幂等 | 二次 Open 合成事件只补一次（Events 数不变） |

竞品对照：event-sourced 会话 + 撕裂恢复 + 合成事件写回在 Go 框架中无对应物（Python 阵营亦无公开等价 property 口径）。

### 2. 压缩事务与 token 效率（memory/compaction）— `TestPropertyCompactionTransaction` / `TestPropertyCompactionShrinks`

| # | 不变式 | 说明 |
|---|---|---|
| P1 | 二选一 | 任意合法窗口下 Compact 要么事务成功、要么零落盘失败——绝不产生中间态 |
| P2 | raw log 只增不减 | 成功时原事件逐条原样保留，恰好追加 started/summarized/checkpoint/ended 四事件 |
| P3 | Report 与日志互证 | Replaced == 窗口 source seqs；CheckpointSeq == checkpoint 事件 Seq |
| P4 | 版本与折叠 | 压缩后 surface 可折叠、FormatVersion 抬升 |
| P5 | token 下降 | 合理摘要引擎下全量压缩后 surface token 严格下降且收敛为单条 |

竞品对照：压缩普遍是「替换列表」无事务语义；checkpoint 事务 + raw log 不可变 + 零落盘拒绝是 Pulse 独有口径。

### 3. 记忆治理（memory/store + memory/candidate）— `TestPropertyStoreLifecycleInvariants` / `TestPropertyCandidateApprovalInvariants`

| # | 不变式 | 说明 |
|---|---|---|
| G1 | 禁物理删除 | 随机操作序列下，任意曾写入 item 永远可 Get（Superseded / Revoked 是状态不是墓碑删除） |
| G2 | 仅 Active 可召回 | Search 默认不返回任何非 Active item |
| G3 | 状态机合法 | Supersede：旧 → Superseded、新 → Active；Revoke → Revoked |
| C1 | Pending 不可见 | 候选在审批前对默认 Search 不可见 |
| C2 | 审批不改 taint | Approve 晋升 Active + Confidence=1.0，Taint 继承不变（审批是晋升闸，taint 是数据属性） |
| C3 | Reject 永不可见 | 否决 = Revoke 留痕 |
| C4 | 状态机闭合 | 审批后 Pending 清空；重复 Approve/Reject → `ErrNotPending` |
| C5 | scope 防污染 | 父 scope 管线批准子 scope 候选 → `ErrOutsideScope` |

竞品对照：Supersede/Revoke 生命周期 + taint 分级 + 审批面在现有 Go/Python 框架的记忆实现中均无对应语义（对齐 `memory-layer-research-and-v2-design.md` §17 调研）。

### 4. 拒绝语义（loop）— `TestPropertyToolRejectionSemantics`

| # | 不变式 | 说明 |
|---|---|---|
| R1 | 被拒绝不执行 | `before_tool_call` 拒绝的工具 handler 副作用为零 |
| R2 | IsError 回传 | 每个被拒调用都有工具结果回传且文本含拒绝原因——模型可自我修正 |
| R3 | 拒绝非失败 | 拒绝使回合 `completed` 而非 `error`（裁决是信息，不是崩溃） |
| R4 | 放行精确一次 | Waterfall 透传 `next` 的放行路径不重复、不丢失执行 |

竞品对照：HITL 审批普遍停在「中断等输入」；「拒绝作为一等结果回传 + 副作用零执行」的 property 口径无公开对应物。

## 可复现性

- 所有随机序列由 `math/rand/v2` PCG 驱动，seed = 固定基值 + 测试名散列（CI 与本地一致）；
- `EVAL_SEED` 环境变量覆盖种子；失败信息带 `seed=<N>` 与迭代号，直接回放；
- 本包只 import 被测包公开 API——若 property 抓出被测包真 bug，另开票修复，不在此放松断言。
