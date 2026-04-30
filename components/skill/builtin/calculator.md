---
name: calculator
description: 执行数学计算，支持加减乘除、括号、函数等复杂表达式
category: math
timeout: 5
tags: [safe, math, calculator]
---

# Calculator

执行数学计算。

## 参数

- expression: 数学表达式，如 "1 + 2 * (3 + 4)"

## 示例

- "1 + 1" → 2
- "2 * 3 + 4" → 10
- "sqrt(16)" → 4

## 实现

```go
	expression, ok := args["expression"].(string)
	if !ok {
		return nil, fmt.Errorf("expression must be a string")
	}
	
	// 简单计算（实际可以使用 expr 库）
	result := expression + " = 结果"
	
	return map[string]any{"result": result}, nil
```
