---
name: web-researcher
description: 联网研究工具，搜索并整理信息。需要 SERPER_API_KEY 环境变量
category: research
language: python
timeout: 30
allowed-tools: web_search get_work_dir file_write
env_vars: SERPER_API_KEY
tags:
  - web
  - research
  - network
parameters:
  type: object
  properties:
    topic:
      type: string
      description: 研究主题
    max_results:
      type: integer
      description: 最大结果数（默认 5）
  required: [topic]
---

# Web Researcher

```python
import json, os

args = json.loads(os.environ.get("SKILL_ARGS", "{}"))
topic = args.get("topic", "")
max_results = args.get("max_results", 5)

# 这个 Skill 本身不做网络请求
# 它的 allowed-tools 声明了 web_search
# 实际搜索由 Agent 通过 web_search 工具完成
# 这里返回结构化的研究指令

result = {
    "status": "ready",
    "topic": topic,
    "max_results": max_results,
    "instructions": f"请使用 web_search 工具搜索 '{topic}'，返回前 {max_results} 条结果，然后整理成摘要。",
    "api_key_configured": bool(os.environ.get("SERPER_API_KEY")),
}
print(json.dumps(result, ensure_ascii=False))
```