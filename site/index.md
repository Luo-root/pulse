---
layout: home

hero:
  name: Pulse
  text: 一切皆插件，卸载即还原
  tagline: Go AI Agent 框架——以可逆效应与依赖响应式为基座的插件内核，v2 核心已以 v0.1.0 预览发布
  actions:
    - theme: brand
      text: 快速开始
      link: /guide/quickstart
    - theme: alt
      text: 包文档
      link: /packages/
    - theme: alt
      text: GitHub
      link: https://github.com/Luo-root/pulse

features:
  - title: 插件内核
    details: kernel.Context 五件套——可逆 Effect（卸载即还原）、类型安全 ServiceKey、四模式事件、Plugin / Fiber / Loader 依赖响应式装载。
    link: /packages/kernel/
    linkText: 查看 kernel
  - title: provider 中立模型层
    details: llm 消息词汇表只收跨 provider 稳定语义的字段；无对应线格式显式 ErrBadRequest，绝不静默吞参数。OpenAI / Anthropic 官方适配器。
    link: /packages/llm/
    linkText: 查看 llm
  - title: 无状态 ReAct
    details: loop.Agent 只执行一个回合——工具调用、HITL 决策事件、Waterfall 拦截点；历史与会话交由记忆层承担。
    link: /packages/loop/
    linkText: 查看 loop
  - title: 编排与记忆
    details: kernel/flow 槽位三态节点图（AND 汇聚、Skip、Observer）+ YAML 声明式装图；memory 九子包覆盖会话、压缩、长期存储、上下文装配到反思管线。
    link: /guide/flow
    linkText: 编排指南
  - title: 可观测
    details: observability.Bootstrap + Record + Sink，只依赖 kernel；trace 分 host / trace 两层，旁路订阅不进拦截链。
    link: /guide/observability
    linkText: 观测指南
  - title: 评测基建
    details: 分层基准 L0–L3、19 条 property 不变式、跨框架对比 eval/war——编排 fan-out 免费、纯运行 ~10× 差距都有数字与口径。
    link: /eval
    linkText: 看数字
---
