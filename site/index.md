---
layout: page
title: Go 工作流执行文档
description: 安装 Pulse，学习数据驱动的工作流、子图、重试与观测。
home:
  eyebrow: PULSE · GO WORKFLOW EXECUTION
  title: 用数据依赖组织工作流
  lead: 定义一次流程，为每个请求创建独立执行。通过类型化数据组合 Go 函数，清楚地表达并行、分支与正常跳过。
  primary: { text: 开始使用, link: /guide/quickstart }
  secondary: { text: 了解核心概念, link: /guide/concepts }
  version: Pulse v0.5.1 · Go 1.26.0+
  flowTitle: 一份定义，一次执行
  flowLabel: 输入到达后计算，并产生声明出口
  flow:
    - { name: input, detail: 本次请求的输入, state: '21 · ready' }
    - { name: double, detail: 读取 input，写入 output, state: completed }
    - { name: output, detail: 从 Execution 读取结果, state: '42 · ready' }
  flowCaption: 依赖来自数据声明。每次运行拥有独立状态。
  pathsLabel: DOCUMENTATION
  pathsTitle: 从你要完成的事开始
  paths:
    - { title: 第一次运行, description: 确认版本、创建模块，运行完整示例并读取结果。, action: 阅读入门指南, link: /guide/ }
    - { title: 构建实际流程, description: 提供输入、组织分支、配置重试，组合子图与流。, action: 选择使用任务, link: /guide/orchestration }
    - { title: 核对具体行为, description: 查阅生命周期、错误、检查点桥和版本迁移规则。, action: 打开参考索引, link: /reference/ }
  companion:
    title: 需要在重启后继续执行？
    description: Pulse Durable 保存已确认的节点成果，并在恢复时重新进入未确认步骤。外部副作用由业务提供幂等或事务。
    action: 了解 Durable
    link: https://github.com/Luo-root/pulse-durable
---

<Home />
