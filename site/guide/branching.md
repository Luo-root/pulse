# 分支与汇聚

本页面向需要按条件选择处理路径、并合并多路结果的开发者。需要 Go 1.25+，并已了解[输入与输出](./inputs-outputs.md)。本例使用尚未发布的开发版 API。

分支通过输出的值或跳过状态表达。汇聚节点先等所有输入到达，再根据自己的策略处理有值与无值的输入。正常跳过不导致整轮失败。

## 完整示例：选择一路并汇聚

把以下程序保存为一个空目录中的 `main.go`。它根据输入选择 `left` 或 `right`，并把有值的那一路收集成报告。

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Luo-root/pulse"
)

func run() error {
	request := pulse.NewKey[string]("request")
	left := pulse.NewKey[string]("left")
	right := pulse.NewKey[string]("right")
	report := pulse.NewKey[string]("report")
	g, err := pulse.New("branch-demo", "选择一路并收集结果")
	if err != nil {
		return err
	}
	if err := g.DeclareInput(pulse.NewPort(request)); err != nil {
		return err
	}
	if err := g.DeclareOutput(pulse.NewPort(report)); err != nil {
		return err
	}
	if err := g.Add(pulse.NewNode("route", "选择处理路径",
		pulse.Requires(request), pulse.Provides(left, right),
		func(rc *pulse.RunCtx) error {
			value, err := pulse.Get(rc, request)
			if err != nil {
				return err
			}
			if strings.HasPrefix(value, "urgent:") {
				return pulse.Only(rc, left, strings.TrimPrefix(value, "urgent:"))
			}
			return pulse.Only(rc, right, value)
		})); err != nil {
		return err
	}
	if err := pulse.Join(g, "collect", pulse.Keys(left, right), report,
		func(rc *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
			return strings.Join(batch.Values(), ", "), nil
		}); err != nil {
		return err
	}
	if err := g.Validate(); err != nil {
		return err
	}
	var in pulse.Inputs
	if err := pulse.Seed(&in, request, "urgent:review"); err != nil {
		return err
	}
	e, err := g.NewExecution(context.Background(), in)
	if err != nil {
		return err
	}
	defer e.Cancel()
	if err := e.Start(); err != nil {
		return err
	}
	if err := e.Wait(); err != nil {
		return err
	}
	value, state, err := pulse.ReadOutput(e, report)
	if err != nil {
		return err
	}
	fmt.Printf("report=%s state=%s\n", value, state)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

在保存文件的目录中执行：

```sh
go mod init example.com/pulse-branches
go get github.com/Luo-root/pulse@1f67045d6c42
go run .
```

预期输出：

```text
report=review state=ready
```

`Only` 写出选中的键，并跳过本节点其他全部输出。`collect` 看到 `left=ready`、`right=skipped`，两条都到达后进入回调。`Values` 只返回有值项，所以报告包含 `review`。

将输入改为 `pulse.SkipSeed(&in, request)` 时，`route` 和 `collect` 都不进入业务体，报告为 `skipped`，整轮仍成功。输入值为 `""` 时，它仍然是有值的输入，不会因零值自动跳过。

## 汇聚的两步判断

“到几个收几个”描述处理策略，不表示收到第一个值就立即执行。假设节点依赖 `a`、`b`、`c`：

| 输入状态 | 业务体是否进入 | 说明 |
|---|---|---|
| `ready`、`pending`、`skipped` | 否 | `b` 还未到达 |
| `ready`、`ready`、`skipped` | 是 | 全部到达，至少一条有值 |
| `skipped`、`skipped`、`skipped` | 否 | 节点自身正常跳过 |

对严格汇聚，需要在处理前显式检查所有输入都有值。以下回调可以替换完整示例中的 `Join` 回调：

```go
func(rc *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
	if err := batch.WaitAll(); err != nil {
		return "", err
	}
	return strings.Join(batch.Values(), ", "), nil
}
```

示例是排他分支，始终有一条缺值；使用严格回调会让报告正常跳过。这是调用方声明的策略。普通 `NewNode` 可以用 `pulse.WaitAll(rc, pulse.Deps(pulse.Requires(a), pulse.Requires(b))...)` 实现相同检查；`a`、`b` 必须是该节点已声明的输入。

## 读取 Batch 与选择分支方式

`Batch.Items` 按输入声明顺序保留每一项，包括 `Present=false` 的缺值项。`Len` 统计有值项，`Values` 返回有值项的值，`Missing` 返回跳过项的键名。需要特定来源时用 `batch.Get(key)`，避免用切片位置猜测同类型结果的来源。

`Batch.Get` 对跳过项返回 `*SkipError`；把它返回出去会让本节点跳过。传入不属于该批次的键返回 `ErrUndeclared`，这是调用错误。不同类型的输入应使用 `NewNode` 与 `Deps`，而不是强行放入同一个 `Batch[T]`。

| 操作 | 适用情况 | 节点结果 |
|---|---|---|
| `Only(rc, key, value)` | 从本节点输出中只选择一条 | 返回 nil 时为 `completed` |
| `Set` 与 `Skip` | 需要同时输出多条，并明确缺值项 | 返回 nil 时为 `completed` |
| `NoValue()` | 整个节点本次没有结果 | `skipped` |

`Only` 应替代该分支中的 `Set`，不要先写其他输出再调用它。`Skip` 只影响单条输出；节点返回 nil 时仍是完成。没有写出的输出会自动跳过，但显式选择能避免遗漏所需的 `Set`。

## 并行实例：FanOut

`FanOut` 在装配期按输出数量创建 `id-1` 至 `id-N` 节点，分片号从 1 开始。普通值输入由每个实例完整读取，函数自己按分片号选择工作。它不会自动把切片拆成几份。

以下片段放在返回 `error` 的装配函数内，已有 `g`、`docs`（`Key[[]string]`）与 `r1`、`r2`（`Key[string]`）：

```go
if err := pulse.FanOut(g, "worker", docs, pulse.Keys(r1, r2),
	func(rc *pulse.RunCtx, shard int, values []string) (string, error) {
		if shard > len(values) {
			return "", pulse.NoValue()
		}
		return strings.ToUpper(values[shard-1]), nil
	}); err != nil {
	return err
}
```

每个实例的错误都能使执行失败；正常无值可以交给后续 `Join` 收集。实例数固定，装配失败不会留下半批节点。channel 输入具有竞争消费语义，详见[流式处理](./streams.md)。

## 常见错误与下一步

不要把 `SkipError` 当作需要重试的故障，也不要把真正失败改写为 `NoValue`。严格汇聚应先检查再写输出；已经发布的普通输出不会因稍后返回跳过而撤回。

需要控制重试或时限，继续阅读[重试、超时与取消](./retry-cancellation.md)；需要复用一段分支流程，阅读[静态子图](./subgraphs.md)。
