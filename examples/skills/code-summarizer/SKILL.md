---
name: code-summarizer
description: 分析代码片段，返回代码的行数、函数数量、导入列表等统计信息
category: analysis
language: python
timeout: 15
tags:
  - code
  - analysis
  - safe
parameters:
  type: object
  properties:
    code:
      type: string
      description: 要分析的代码内容
    language_hint:
      type: string
      description: 代码语言提示（python/go/javascript）
  required: [code]
---

# Code Summarizer

```python
import json, os, re

args = json.loads(os.environ.get("SKILL_ARGS", "{}"))
code = args.get("code", "")
lang = args.get("language_hint", "unknown")

lines = code.strip().split("\n")
total = len(lines)
blank = sum(1 for l in lines if l.strip() == "")
comment = 0
for l in lines:
    stripped = l.strip()
    if stripped.startswith("#") or stripped.startswith("//") or stripped.startswith("/*"):
        comment += 1

funcs = len(re.findall(r'\b(func|def|function|async\s+function)\b', code))
imports = re.findall(r'(?:import|from|require)\s+[\w"./]+', code)

result = {
    "language": lang,
    "total_lines": total,
    "blank_lines": blank,
    "comment_lines": comment,
    "code_lines": total - blank - comment,
    "functions": funcs,
    "imports": imports,
}
print(json.dumps(result, ensure_ascii=False))
```