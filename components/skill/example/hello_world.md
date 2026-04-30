---
name: hello_world
description: 简单的打招呼工具
category: demo
timeout: 5
tags: [safe, demo]
---

# Hello World

打招呼。

## 参数

- name: 名字

## 实现

```go
	name, ok := args["name"].(string)
	if !ok {
		name = "World"
	}
	
	return map[string]any{"message": "Hello, " + name + "!"}, nil
```
