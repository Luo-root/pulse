---
name: system-info
description: 获取当前系统信息，包括操作系统、CPU、内存、磁盘等
category: system
language: go
timeout: 10
tags:
  - system
  - info
  - safe
parameters:
  type: object
  properties:
    detail:
      type: string
      description: 详情级别（basic/full），默认 basic
---

# System Info

```go
package skill

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
)

func Handler(ctx context.Context, args map[string]any) (any, error) {
	detail := "basic"
	if d, ok := args["detail"].(string); ok && d != "" {
		detail = d
	}

	info := map[string]any{
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"go":         runtime.Version(),
		"cpus":       runtime.NumCPU(),
		"goroutines": runtime.NumGoroutine(),
	}

	if detail == "full" {
		hostname, _ := os.Hostname()
		wd, _ := os.Getwd()
		info["hostname"] = hostname
		info["work_dir"] = wd
		info["env_count"] = len(os.Environ())
		info["user"] = os.Getenv("USER")
		if info["user"] == "" {
			info["user"] = os.Getenv("USERNAME")
		}
	}

	data, _ := json.Marshal(info)
	return string(data), nil
}
```