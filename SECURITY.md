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

Anything in this repository:

- kernel lifecycle and reversible effects, service visibility and dependency resolution;
- event dispatch (`Emit` / `EmitLocal` / waterfalls) and the `flow` slot semantics;
- session storage, recovery and compaction; the long-term memory store, assembly and the
  self-edit tool surface;
- the tool registry, the built-in tools, and the MCP source;
- the observability envelope, sinks and the per-request collector;
- the provider adapters' request vocabulary and error classification.

## Out of scope

- **Third-party dependencies** — report those upstream (a heads-up here is still appreciated).
- **Anything requiring an attacker who already controls** the process, the machine, the
  environment, or the credentials configured in it.
- **Secrets committed in a fork** — secret scanning runs on this repository, and `.env*` is
  gitignored.
- **Model output quality** — prompt injection that merely makes a model say something unhelpful
  is not a vulnerability in this library. A flaw that turns model output into code execution,
  credential exposure or data exfiltration *is*.

## What is already in place

- Secret scanning and push protection are enabled on this repository.
- `.env`, `.env.*`, `*.pem` and `*.secrets` are gitignored.
- `observability.Record` has no `map[string]any` escape hatch, and `Attrs` only accepts scalars
  (`~string | ~int64 | ~float64 | ~bool`) — prompts, message payloads, attachments and
  chain-of-thought cannot enter an observation record by construction.
- Provider live tests are gated by environment variables, so running the default test suite
  requires no credentials.

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

本仓库里的一切：

- kernel 生命周期与可逆效应、服务可见性与依赖解析；
- 事件派发（`Emit` / `EmitLocal` / waterfall）与 `flow` 槽位语义；
- 会话存储、冷恢复与压缩；长期记忆存储、上下文装配与 self-edit 工具面；
- 工具注册表、内置工具、MCP 来源；
- 观测信封、出口与请求级 collector；
- provider 适配器的请求词汇表与错误分类。

### 不在范围内

- **第三方依赖漏洞**——请上报给上游（顺手告知我们一声仍然欢迎）。
- **需要攻击者已经控制**进程、机器、运行环境或其中配置的凭据的场景。
- **fork 里提交的密钥**——本仓库已开启 secret scanning，且 `.env*` 已被忽略。
- **模型输出质量**——仅仅是让模型说错话的提示注入不算本库的漏洞；但如果模型输出能变成代码
  执行、凭据泄露或数据外泄，那就**算**。

### 已经做了哪些加固

- 仓库已开启 secret scanning 与 push protection。
- `.env`、`.env.*`、`*.pem`、`*.secrets` 均在 .gitignore 中。
- `observability.Record` 没有 `map[string]any` 逃生舱，`Attrs` 只接受标量
  （`~string | ~int64 | ~float64 | ~bool`）——prompt、消息载荷、附件与思维链在类型上就无法进入
  观测记录。
- provider 的 live 测试由环境变量门控，跑默认测试集不需要任何凭据。
