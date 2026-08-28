---
name: git-log-analyzer
description: 分析最近的 Git 提交记录，返回提交统计信息
category: devops
language: python
timeout: 10
tags:
  - git
  - devops
  - safe
parameters:
  type: object
  properties:
    count:
      type: integer
      description: 分析最近多少条提交（默认 10）
---

# Git Log Analyzer


```python
import sys, json, os, subprocess
from collections import Counter

# 确保 Windows 下使用 UTF-8 输出
if sys.platform == 'win32':
    import codecs
    sys.stdout = codecs.getwriter('utf-8')(sys.stdout.buffer, 'strict')

args = json.loads(os.environ.get("SKILL_ARGS", "{}"))
count = args.get("count", 10)

def run_git(cmd):
    """执行 git 命令并返回输出"""
    try:
        result = subprocess.run(
            cmd,
            shell=True,
            capture_output=True,
            text=True,
            timeout=5
        )
        return result.stdout.strip() if result.returncode == 0 else ""
    except Exception:
        return ""

print(f"=== 最近 {count} 条提交 ===")
output = run_git(f"git log --oneline -n {count}")
print(output if output else "Not a git repository")

print("")
print("=== 提交者统计 ===")
output = run_git(f"git shortlog -sn --no-merges -n {count}")
print(output if output else "N/A")

print("")
print("=== 文件变更最多的文件 ===")
output = run_git(f"git log --pretty=format: --name-only -n {count}")
if output:
    files = [f for f in output.split('\n') if f.strip()]
    counter = Counter(files)
    for file, cnt in counter.most_common(10):
        print(f"{cnt:>4} {file}")
else:
    print("N/A")
```