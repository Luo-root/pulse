# Security Policy

Pulse is pre-1.0 (`v0.x`). This file covers how to report a vulnerability and what is in scope.
Both halves are equivalent: English first, 中文在后.

## Supported versions

Fixes land on `main` and in the newest release. Older `0.x` releases are not maintained.

| Version | Supported |
| --- | --- |
| `main` | ✅ |
| newest `v0.x` release (see [Releases](https://github.com/Luo-root/pulse/releases)) | ✅ |
| older `0.x` releases | ❌ |

If you are on an older release, the fix will be in the newest one — there are no backports
during the `0.x` line.

## Reporting a vulnerability

**Do not open a public issue.** A public issue is visible to everyone long before a fix exists.

Report privately by email to **3029295957@qq.com**, including:

- the affected version, tag or commit;
- what the problem is and why it matters — the impact, not just the symptom;
- a minimal reproduction: code, configuration, or exact steps;
- any fix or mitigation you already have in mind;
- how you want to be credited, or say that you would rather not be.

GitHub's private vulnerability reporting is **not enabled** on this repository, so email is the
private channel. If that changes, this section will be updated.

## What to expect

- An acknowledgement within a few days.
- An assessment — is it a vulnerability, and how severe — plus a decision on the fix.
- Credit in the release notes when the fix ships, if you want it.
- **No bug bounty.** This is an unfunded open-source project; there is nothing to pay out, and
  we would rather say so up front than leave it implied.

## In scope

Anything in this repository — the graph engine (`pulse`), graph observation (`pulse/observe`)
and declarative assembly (`pulse/yaml`):

- the slot contract (three states, skip-is-arrival) and the failure path: the first node error
  cancels the graph, cancellation wins over arrival, a skip is never rewritten into a failure;
- graph lifecycle: one-shot semantics (`Add` / `Seed` after start are rejected), `WithMaxRunning`
  admission, and the aspect chain (`Timeout` / `Retry`) including its re-entrancy latch;
- the observation envelope: `Record` / `Attrs`, the built-in sinks (`LineSink`, `SlogSink`,
  `MemorySink`, `MultiSink`, `AsyncSink`) and the six encoding primitives — including anything
  that would let a payload reach a record's non-`Attrs` fields;
- the YAML assembly path in `pulse/yaml`: document decoding, the key registry's name/type
  resolution, and literal shape alignment.

## Out of scope

- **Third-party dependencies** — report those upstream (a heads-up here is still appreciated).
- **Anything requiring an attacker who already controls** the process, the machine, the
  environment, or the credentials configured in it.
- **Secrets committed in a fork** — secret scanning runs on this repository, and `.env*` is
  gitignored.
- **What a host builds on top of Pulse** — prompt handling, model output quality and any other
  domain logic above the engine are out of scope here; a flaw *inside* this library that turns
  host input into code execution, credential exposure or data exfiltration is in scope.

## What is already in place

- Secret scanning and push protection are enabled on this repository.
- `.env`, `.env.*`, `*.pem` and `*.secrets` are gitignored.
- `observe.Record` has no `map[string]any` escape hatch, and `Attrs` only accepts scalars
  (`~string | ~int64 | ~float64 | ~bool`) — payloads cannot enter an observation record by
  construction, only keys and scalar values can.
- The suite runs offline and needs no credentials: `go.mod` requires only `gopkg.in/yaml.v3`,
  and no test performs network or filesystem IO beyond what it creates itself.

---

## 中文

Pulse 目前仍在 1.0 之前（`v0.x`）。本文件说明漏洞上报方式与适用范围。

### 支持范围

修复只落在 `main` 与最新一版 release 上；更早的 `0.x` 版本不再维护。

| 版本 | 是否支持 |
| --- | --- |
| `main` | ✅ |
| 最新的 `v0.x` release（见 [Releases](https://github.com/Luo-root/pulse/releases)） | ✅ |
| 更早的 `0.x` 版本 | ❌ |

如果你还在旧版本上，修复会出现在最新版里——`0.x` 期间不做 backport。

### 上报漏洞

**不要开公开 Issue。**公开 Issue 在修复出现之前就对所有人可见。

请发邮件到 **3029295957@qq.com** 私密上报，内容尽量包含：

- 受影响的版本、tag 或 commit；
- 问题是什么、为什么重要（说影响，不只是现象）；
- 最小复现：代码、配置或确切步骤；
- 你已经有思路的修复或缓解方式；
- 是否愿意被致谢、以什么名义。

本仓库**未启用** GitHub 私密漏洞报告，所以邮箱是当前的私密渠道。若之后启用了，本节会同步更新。

### 你会得到什么

- 几天内的确认。
- 一个评估（是不是漏洞、严重程度如何）以及是否修、怎么修的决定。
- 修复随版本发布时在 Release notes 中致谢（如果你愿意）。
- **没有漏洞奖金。**这是一个没有经费的开源项目；与其含糊，不如直接说明。

### 在范围内

本仓库里的一切——图引擎（`pulse`）、图观测（`pulse/observe`）与声明式装图（`pulse/yaml`）：

- 槽位契约（三态、「跳过是到达」）与失败路径：节点首错取消整图、取消优先于到达、跳过绝不
  被改写成失败；
- 图生命周期：一次性语义（启动后的 `Add` / `Seed` 被拒）、`WithMaxRunning` 名额准入、
  切面链（`Timeout` / `Retry`）与它的重入门闩；
- 观测信封：`Record` / `Attrs`、内置出口（`LineSink` / `SlogSink` / `MemorySink` /
  `MultiSink` / `AsyncSink`）与六条编码原语——包括任何「让载荷从非 `Attrs` 字段混进记录」
  的可能；
- `pulse/yaml` 的装配路径：文档解码、Key 登记表的 name/type 对账、字面量形状对齐。

### 不在范围内

- **第三方依赖漏洞**——请上报给上游（顺手告知我们一声仍然欢迎）。
- **需要攻击者已经控制**进程、机器、运行环境或其中配置的凭据的场景。
- **fork 里提交的密钥**——本仓库已开启 secret scanning，且 `.env*` 已被忽略。
- **宿主在 Pulse 之上做的事**——prompt 处理、模型输出质量以及引擎之上的任何领域逻辑都不在
  本仓库范围内；但库**内部**把宿主输入变成代码执行、凭据泄露或数据外泄的缺陷**在**范围内。

### 已经做了哪些加固

- 仓库已开启 secret scanning 与 push protection。
- `.env`、`.env.*`、`*.pem`、`*.secrets` 均在 .gitignore 中。
- `observe.Record` 没有 `map[string]any` 逃生舱，`Attrs` 只接受标量
  （`~string | ~int64 | ~float64 | ~bool`）——载荷在类型上就无法进入观测记录，能进的只有
  key 与标量值。
- 整套测试离线可跑、不需要凭据：`go.mod` 只 require `gopkg.in/yaml.v3`，且没有任何测试
  去做它自己没创建的 IO。
