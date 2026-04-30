---
name: web_search
description: 搜索互联网获取最新信息
category: search
timeout: 30
tags: [network, search]
---

# Web Search

使用搜索引擎获取信息。

## 参数

- query: 搜索关键词
- num_results: 返回结果数量（默认5）

## 实现

```go
query := args["query"].(string)
numResults := 5
if n, ok := args["num_results"].(float64); ok {
    numResults = int(n)
}

// 调用搜索 API
results, err := searchAPI(query, numResults)
if err != nil {
    return nil, err
}
return map[string]any{"results": results}, nil
```
