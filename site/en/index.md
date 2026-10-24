---
layout: page
title: Workflow execution for Go
description: Install Pulse and learn data-driven workflows, subgraphs, retries, and observation.
home:
  eyebrow: PULSE · GO WORKFLOW EXECUTION
  title: Build workflows from data dependencies
  lead: Define a workflow once and create an independent execution for each request. Compose Go functions through typed data, with explicit parallel work, branches, and normal skips.
  primary: { text: Get started, link: /en/guide/quickstart }
  secondary: { text: Core concepts, link: /en/guide/concepts }
  version: Pulse v0.5.1 · Go 1.26.0+
  flowTitle: One definition, one execution
  flowLabel: An input arrives, computation runs, and a declared output becomes ready
  flow:
    - { name: input, detail: This request's input, state: '21 · ready' }
    - { name: double, detail: Read input and write output, state: completed }
    - { name: output, detail: Read from the Execution, state: '42 · ready' }
  flowCaption: Data declarations define dependencies. Every run has its own state.
  pathsLabel: DOCUMENTATION
  pathsTitle: Start with your task
  paths:
    - { title: Your first run, description: Choose a version and run a complete program that reads its output., action: Read the introduction, link: /en/guide/ }
    - { title: Build a workflow, description: Supply inputs and compose branches, retries, subgraphs, and streams., action: Choose a guide, link: /en/guide/orchestration }
    - { title: Check a guarantee, description: Look up lifecycle rules, errors, the checkpoint bridge, and migration., action: Open the reference, link: /en/reference/ }
  companion:
    title: Need to continue after a restart?
    description: Pulse Durable saves confirmed node results and re-enters unconfirmed steps on recovery. Application code provides idempotency or transactions for external side effects.
    action: Explore Durable
    link: https://github.com/Luo-root/pulse-durable
---

<Home />
