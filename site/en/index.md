---
layout: home

hero:
  name: Pulse
  text: Everything is a plugin, unload to restore
  tagline: Go AI Agent framework — a plugin kernel grounded in reversible effects and dependency-reactive loading; the v2 core ships as v0.1.0 preview
  actions:
    - theme: brand
      text: Quick start
      link: /en/guide/quickstart
    - theme: alt
      text: Package docs
      link: /en/packages/
    - theme: alt
      text: GitHub
      link: https://github.com/Luo-root/pulse

features:
  - title: Plugin kernel
    details: kernel.Context five-piece — reversible Effects (unload restores), typed ServiceKey, four-mode events, Plugin/Fiber/Loader with dependency-reactive loading.
    link: /en/packages/kernel/
    linkText: Read kernel docs
  - title: Provider-neutral model layer
    details: The llm vocabulary only carries fields with stable cross-provider semantics; missing wire counterparts return ErrBadRequest — parameters are never silently dropped. Official OpenAI / Anthropic adapters.
    link: /en/packages/llm/
    linkText: Read llm docs
  - title: Stateless ReAct
    details: loop.Agent runs exactly one turn — tool calls, HITL decision events, waterfall interception points; history and sessions belong to the memory layer.
    link: /en/packages/loop/
    linkText: Read loop docs
  - title: Orchestration & memory
    details: kernel/flow three-state slot node graph (AND joins, Skip, Observer) + YAML declarative loading; nine memory sub-packages from sessions and compaction to reflection.
    link: /en/guide/flow
    linkText: Orchestration guide
  - title: Observability
    details: observability.Bootstrap + Record + Sink, kernel-only dependency; two-tier trace (host / request), side-band subscriptions that never touch interception chains.
    link: /en/guide/observability
    linkText: Observability guide
  - title: Evaluation infra
    details: Layered benchmarks L0–L3, 19 property invariants, cross-framework suite eval/war — free fan-out and the ~10× pure-runtime gap, with numbers and accounting.
    link: /en/eval
    linkText: See the numbers
---
