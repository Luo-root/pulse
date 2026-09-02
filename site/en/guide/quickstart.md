# Quick start

Pulse is a Go AI Agent framework shipping its v2 core as a preview (v0.1.0). This page walks the shortest path: **plugin kernel + model layer + a ReAct tool round**.

## Requirements

- **Go 1.25+** (the toolchain downloads itself when missing)
- An API key for an OpenAI-compatible model (via environment variables — never hard-code credentials)

## Install

```bash
go get github.com/Luo-root/pulse
```

## Shortest path: model + ReAct tool round

The example below is full production assembly: kernel host → llm.Registry (observed wrapper) → named model instance → MemToolSet tool registration → Agent with a request scope.

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/llm/openai"
	"github.com/Luo-root/pulse/loop"
)

func main() {
	host := kernel.New()
	defer host.Dispose()

	reg := llm.NewRegistry(host)
	if err := openai.Register(host, reg); err != nil {
		panic(err)
	}
	if err := reg.Declare("main", llm.Config{
		Provider: openai.ProviderCompletions,
		Model:    "gpt-4o-mini",
		APIKey:   os.Getenv("OPENAI_API_KEY"),
	}); err != nil {
		panic(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		panic(err)
	}

	tools := loop.NewMemToolSet()
	_ = tools.Register(llm.ToolDef{
		Name:        "echo",
		Description: "echoes the arguments back",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}, func(ctx context.Context, args json.RawMessage) (string, error) {
		return string(args), nil
	})

	agent, err := loop.NewAgent(model,
		loop.WithToolSet(tools),
		loop.WithSystemPrompt("You are a concise assistant."),
		loop.WithEventScope(host),
	)
	if err != nil {
		panic(err)
	}

	res, err := agent.Run(context.Background(), nil, llm.UserText("call the echo tool with text=hello"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Final.Text())
}
```

Save as `main.go`, set `OPENAI_API_KEY`, then run:

```bash
go run ./main.go
```

## Next steps

- **Core concepts**: Effect / ServiceKey / events and the loading model → [Core concepts](/en/guide/concepts)
- **Orchestration**: three-state slot node graphs and YAML loading → [flow orchestration](/en/guide/flow)
- **Memory**: sessions, compaction, long-term store, assembly → [Memory layer](/en/guide/memory)
- **Per-package docs**: full bilingual docs for all 27 packages → [Packages](/en/packages/)
- **Progressive lessons**: 8 lessons from kernel ground to production → [Examples](/en/examples)
