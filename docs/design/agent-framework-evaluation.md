# Agent 框架评测体系调研（Survey）

状态：Draft（评审通过后转 Accepted）
日期：2026-09-02
关联：Issue #97；`memory-layer-research-and-v2-design.md` §17（检索补遗）

本文回答一个问题：**Pulse 的工程能力如何变成可被外界验证的证据链**。团队场景下自研框架的选型说服力不来自「作者熟悉」，而来自可复现的第三方口径数据。本文梳理业界评测格局、Go 生态竞品位置，并给出三步走评测策略作为后续实现票的路线图。

**口径声明**：本文引用的所有数字均为第三方来源（论文 / 标准化 harness / 社区横评），标注「截至 2026-09」；厂商自报基准数字不作为 Pulse 验收锚点（理由见 §6）。

## 1. 为什么 Pulse 需要评测叙事

Pulse v2 的核心投入——kernel 效应跟踪（revertible effects）、崩溃恢复（session 冷恢复 + 合成事件写回）、记忆治理（Supersede/Revoke 状态机 + taint 传播 + 审批面）——在当前 Go 生态里没有对应物，但也因此没有对照系：外界无法判断这些能力是「真实差异化」还是「过度设计」。

两个事实使评测成为必要而非可选项：

1. **框架层对最终能力有实证影响，且影响幅度大于大多数模型迭代**（§2）——意味着框架选择本身就是用户可感知的质量变量，需要被测量而不是被声称。
2. **Go 生态在 agent 框架评测上整体缺席**（§4）——先建立 Go 内评测口径的框架，天然占据该生态的叙事位置。

## 2. 框架层对能力的影响：实证

### 2.1 HAL：scaffold 差 7–30 点

HAL（Holistic Agent Leaderboard，Kapoor et al.，Princeton，arXiv 2510.11977，ICLR 2026）是目前最大规模的 agent 评测基建研究：21,730 rollouts，9 模型 × 9 基准 × 多 scaffold，约 $40K 成本，25 亿 token 轨迹日志全部公开。

与本文最相关的发现：

| 发现 | 数字 | 含义 |
|---|---|---|
| 同模型换 scaffold，GAIA 分差 | **7–30 点**（HAL scaffold ~+30；HAL vs HF Open Deep Research 同 Opus 4 差 7 点） | 编排层（重试预算、上下文管理、工具包装）贡献了不亚于换模型的能力差异 |
| SWE-bench 上 scaffold 摆动 | 10–20 点（Anthropic 自家 harness 69.2% vs Scale SEAL 标准化 harness 51.9%，同家族模型） | 「模型得分」实际上是「模型 × harness」得分 |
| 失败任务的归因 | 60%+ 的失败 run 违反显式指令；环境错误（沙箱崩溃 / 网络超时）占 ~40% run | 大量「agent 失败」实为 **harness 失败**——评测基建本身的质量被系统性低估 |
| reasoning effort 反直觉 | 多数 run 中更高 reasoning effort 降低准确率（上下文膨胀 + 过度展开） | 上下文管理策略是框架层的核心质量变量 |

### 2.2 Binding Constraint Thesis

HAL 数据催生的立场论文（Tulane/Rutgers/Virginia Tech，arXiv 2605.23950）将其形式化：对前沿模型的长程 agentic 任务，**harness 配置对测量成绩的影响强于模型选择**——harness 是闭环系统的控制器，模型只是它治理的随机策略；方差分解显示 harness 诱发的方差超过模型诱发的方差，甚至出现两个模型仅因 scaffold 不同而交换排名的案例。

### 2.3 对 Pulse 的含义

这两项工作把「框架层质量」从营销话术变成了可测量对象。Pulse 的设计决策（Waterfall vs On 的语义分离、请求 scope 生命周期、记忆渐进披露）正是 harness 层变量——它们的效果应该、也可以被同一套方法论测量。这构成 Pulse 评测叙事的第一块基石：**不跟模型能力基准较劲，测框架层对可控变量的贡献**。

## 3. 评测维度参考系

### 3.1 CLEAR 五维

CLEAR（Mehta，arXiv 2511.14136，2025-11）对 12 个主流基准做系统分析后指出：准确率中心的评测掩盖了三个企业部署的真实变量——相似精度下成本波动 **50 倍**（每任务 $0.10–$5.00）、单次运行 60% 到 8 次一致 25% 的 **58% 可靠性退化**、以及安全/延迟/合规维度的缺失。其提出的五维框架：

| 维度 | 定义 | 测量 |
|---|---|---|
| Cost | token / API 调用 / 基础设施成本 | 每任务成本、每成功完成成本 |
| Latency | 完成时间 | P50 / P95 / P99 |
| Efficacy | 任务完成率 | 基准分、生产成功率 |
| Assurance | 安全、治理、合规 | 政策违反率、审计覆盖 |
| Reliability | 跨运行一致性 | N-run 一致性、回滚率 |

实证：只优化准确率的 agent 比成本感知替代方案贵 4.4–10.8 倍；CLEAR 对生产成功率的预测力（ρ=0.83）显著高于仅准确率口径（ρ=0.41）。

### 3.2 pass^k：把可靠性变成一等指标

τ-bench（Sierra）用 pass^k（同一任务 k 次全过的比例）衡量可靠性，并测政策遵循度（agent 订对航班但违反退改签政策 = 任务失败）。2026 年 tau2-bench 已扩展至 voice 与知识检索域。HAL 维护者在 2026 年公开声明暂停新增模型、转向可靠性测量——**单次准确率的信息价值正在耗尽，跨运行一致性、每正确答案成本、工具错误恢复成为下一代评测的主轴**。

### 3.3 基准信任危机：为什么第三方口径是前提

2026-04，UC Berkeley RDI 证实八大主流 agent 基准（SWE-bench、WebArena、GAIA、OSWorld 等）全部可被 reward-hack 攻破（部分 harness 会把标准答案泄漏给被测 agent）；METR 独立发现前沿模型在 30%+ 的评测 run 中出现 reward-hack 行为。OpenAI 已因评测集污染停止自报 SWE-bench Verified 分数。

结论：**任何评测叙事的公信力前提是 harness 代码公开、轨迹日志公开、第三方可复现**。HAL 的 25 亿 token 公开轨迹正是它成为参照系的原因。

## 4. Go 生态竞品格局

### 4.1 现状（截至 2026-09）

- **Eino**（CloudWeGo）是 Go 生态 agent 框架的头部（约 11.5K stars），组件化设计 + Graph/Chain/Workflow 编排 + ADK，背靠字节生产环境；
- LangChain / LlamaIndex / CrewAI / AutoGen 等 Python 阵营垄断了几乎所有框架横评与基准叙事——主流框架横评（如六框架基建对比）中 **Go 框架集体缺席**；
- Go 生态没有出现 HAL 式的标准化评测 harness，也没有框架层 CLEAR 口径的成本/可靠性数据。

### 4.2 共同短板与 Pulse 差异化

横评与基准缺失之外，Go 框架（含 Eino）与 Python 阵营有一个共同空白：**没有把记忆当作带治理语义的持久层**。记忆普遍退化为「历史消息列表 + 摘要」，缺少事件溯源（event-sourced 日志）、Supersede/Revoke 生命周期、taint 传播、审批面（HITL）这套治理语义。Pulse P2 记忆层正是按这套语义建的（§17 调研对齐 Letta/Mem0/Zep 后确认这是行业前沿而非标配）。

Pulse 评测叙事的差异化锚点由此确定：

1. **基建 benchmark**：Go 内战（Go 框架同口径对比）+ 跨语言参照，填补 Go 生态评测空白；
2. **工程能力 property tests**：崩溃恢复、token 效率、记忆投毒防护——竞品没有对应物，测了就是独有证据；
3. **治理语义评测**：Supersede/Revoke 正确性、审批 fail-closed、taint 不变性——把 P2 的设计不变式变成可执行断言。

## 5. 三步走评测策略

三步全部要做：第二步**不依赖**第一步（property tests 只吃进程内断言，无需 mock 基建，可并行甚至先行——#99 即证）；第三步依赖前两步建立的可信度，且成本高，排在最后。

### 5.1 第一步：基建 benchmark（不依赖真实 LLM）

**测什么**：框架自身的可控开销——同一任务在 Pulse vs 其它 Go 框架 vs 裸 HTTP 调用下的 token 重复注入量、每请求延迟叠加、内存占用、并发吞吐。

**怎么测**：mock LLM（ScriptedModel 已具备）+ 固定任务集 + 逐请求计数。生产路径优先原则：测量代码直接跑各框架的生产入口，不另写旁路工具；Pulse 侧已有 `observability.Record`（含 Duration/Usage）作为数据面。

**产出**：Go 框架第一份同口径成本/延迟对比表（CLEAR 的 Cost/Latency 维度），公开 harness 与原始数据。

**成本**：CI 内跑完，无 API 开销（mock LLM 零网络）；Go 内战对比票引入外部框架依赖时另评构建成本。

### 5.2 第二步：工程能力 property tests（确定性断言）

**测什么**：竞品没有对应物的能力，逐项转化为 property test——

- **崩溃恢复**：kill 进程后 Open 恢复出合法 surface（P2-A 已有测试基座，升级为跨实现对照 + 故障注入矩阵）；
- **token 效率**：compaction 后上下文预算守恒、fold 后引用不悬挂（§9.1 八步事务不变式）；
- **记忆治理**：Supersede 后旧值不可召回、taint 经审批不变（P2-D 已有语义链，升级为随机化 property test）；
- **审批 fail-closed**：任意输入不可读路径都拒绝放行（03 课手写 HITL 的语义，断言化）。

**怎么测**：Go 原生 property testing（快速生成器 + 不变式断言），CI 可跑、无需外部依赖。

**产出**：一套公开的「agent 框架工程可靠性检查表」——竞品可以直接拿来测自己，测不过的项就是 Pulse 的证据。这是把设计文档里的不变式变成行业口径的动作。

**成本**：CI 内跑完，无 API 开销（纯进程内断言，零外部依赖）。

### 5.3 第三步：能力基准（GAIA / tau-bench）

**测什么**：端到端能力——HAL harness 跑 GAIA（Generalist scaffold 位置）+ tau-bench（政策遵循 + pass^k 可靠性）。

**怎么测**：接入 HAL 公开 harness（轨迹与成本口径现成），固定 scaffold 披露（框架版本 / 重试预算 / 工具集全公开），成本控额对比。

**成本与顺序**：单次完整 GAIA 评估在 HAL 口径下需数十美元级 API 开销与数小时计算，且需要真实模型凭据；放在前两步建立可信度之后做，避免把高成本弹药打在无对照系的数字上。

## 6. 与既有决策的一致性

- **不引用厂商自测数字**（Issue #66 已定案：LongMemEval 厂商自测与独立评测差 45 分）：本文引用的 HAL/CLEAR/Berkeley 全部是第三方运行；Pulse 自身未来的评估默认公开 harness + 轨迹，走第三方可复现口径。
- **生产路径优先**（蜜蜂检测项目实证教训）：三步走全部以生产入口为测量对象，不维护第二套旁路测量工具。
- **列表与数据不静默截断**：评测原始数据以完整 JSONL 公开，聚合表只做汇总视图。
- **HITL 审批面 / 记忆治理**与 §17 检索补遗的结论（Letta 三级记忆、Mem0 ADD-only、Zep bi-temporal）保持同一事实源。

## 7. 后续票拆分（实现时逐票开）

包布局按实际落地更新（#99 拍板顶层扁平 `eval/`，不建子包）：

1. ~~工程能力 property test 套件~~——已落地（#99 → PR #100，`eval/` 四主题测试文件）；
2. 基建 benchmark：Pulse 分层开销基线（裸 Generate → Registry 装配 → loop.Agent → session 持久化，`eval/benchmark_test.go`）先行；Go 内战对比（Eino 等外部依赖）另票；
3. HAL / tau-bench 接入票（第三步，依赖 2 的口径与凭据管理）。

## 8. 来源清单

- HAL：Kapoor et al., *Holistic Agent Leaderboard*, arXiv 2510.11977, ICLR 2026（hal.cs.princeton.edu；25 亿 token 轨迹公开）
- Binding Constraint Thesis：*Stop Comparing LLM Agents Without Disclosing the Harness*, arXiv 2605.23950（2026-05）
- CLEAR：Mehta, *Beyond Accuracy: A Multi-Dimensional Framework for Evaluating Enterprise Agentic AI Systems*, arXiv 2511.14136（2025-11-18）
- τ-bench / tau2-bench：Sierra（政策遵循 + pass^k；2026-04 扩展 voice / 检索域）
- Berkeley reward-hack：UC Berkeley RDI（2026-04-12）；METR 独立发现
- Go 生态：Eino 仓库（约 11.5K stars，截至 2026-09）；主流框架横评中 Go 缺席（社区横评，2026）
