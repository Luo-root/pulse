# 静态子图

本页面向需要将一段流程封装为可复用步骤的开发者。需要 Go 1.26+，并了解[输入输出端口](./inputs-outputs.md)与[汇聚策略](./branching.md)。本例使用开发版 API。

`Sub` 在装配期把现成的子图定义挂到父图中。子图只能通过声明端口交换数据；运行前，父图就可以查看和校验完整子树。

## 完整示例：同一子图挂载两次

下面的程序给同一份标题加不同前缀，再汇聚两个结果。两处挂载共享定义，每处的输入、输出和子执行分别隔离。观察者装在父图的一次执行上，子执行自动继承。

将程序保存为一个空目录中的 `main.go`：

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

func run() error {
	topic := pulse.NewKey[string]("topic")
	childTopic := pulse.NewKey[string]("child.topic")
	prefix := pulse.NewKey[string]("child.prefix")
	label := pulse.NewKey[string]("child.label")
	first := pulse.NewKey[string]("first")
	second := pulse.NewKey[string]("second")
	report := pulse.NewKey[string]("report")
	child, err := pulse.New("labeler", "为标题加前缀")
	if err != nil {
		return err
	}
	if err := child.DeclareInput(pulse.NewPort(childTopic), pulse.NewPort(prefix)); err != nil {
		return err
	}
	if err := child.DeclareOutput(pulse.NewPort(label)); err != nil {
		return err
	}
	if err := child.Add(pulse.NewNode("label", "生成标签",
		pulse.Requires(childTopic, prefix), pulse.Provides(label),
		func(rc *pulse.RunCtx) error {
			if err := pulse.WaitAll(rc, pulse.Requires(childTopic, prefix)...); err != nil {
				return err
			}
			value, err := pulse.Get(rc, childTopic)
			if err != nil {
				return err
			}
			p, err := pulse.Get(rc, prefix)
			if err != nil {
				return err
			}
			return pulse.Set(rc, label, p+value)
		})); err != nil {
		return err
	}
	parent, err := pulse.New("sub-demo", "复用标签子图", pulse.WithMaxRunning(2))
	if err != nil {
		return err
	}
	if err := parent.DeclareInput(pulse.NewPort(topic)); err != nil {
		return err
	}
	if err := parent.DeclareOutput(pulse.NewPort(report)); err != nil {
		return err
	}
	if err := pulse.Sub(parent, "first", "生成第一份标签", []pulse.SubBind{
		pulse.In(topic, childTopic), pulse.Fix(prefix, "A: "), pulse.Out(label, first),
	}, child); err != nil {
		return err
	}
	if err := pulse.Sub(parent, "second", "生成第二份标签", []pulse.SubBind{
		pulse.In(topic, childTopic), pulse.Fix(prefix, "B: "), pulse.Out(label, second),
	}, child); err != nil {
		return err
	}
	if err := pulse.Join(parent, "collect", "汇总两份标签", pulse.Keys(first, second), report,
		func(rc *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
			if err := batch.WaitAll(); err != nil {
				return "", err
			}
			return strings.Join(batch.Values(), " | "), nil
		}); err != nil {
		return err
	}
	if err := parent.Validate(); err != nil {
		return err
	}
	var in pulse.Inputs
	if err := pulse.Seed(&in, topic, "Pulse"); err != nil {
		return err
	}
	sink := &observe.MemorySink{}
	obs, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink: sink, HostID: "docs", TraceID: observe.NewTraceID(),
	})
	if err != nil {
		return err
	}
	e, err := parent.NewExecution(context.Background(), in, pulse.WithExecObserver(obs))
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
	fmt.Printf("records=%d\n", sink.Len())
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

在该目录运行。运行未合并分支的示例时，按[安装页的本地替换步骤](./installation.md)使用对应源码；以下 `@main` 命令只获取已合入主线的 API。

```sh
go mod init example.com/pulse-subgraphs
go get github.com/Luo-root/pulse@main
go run .
```

预期输出：

```text
report=A: Pulse | B: Pulse state=ready
records=16
```

根执行有两条运行记录和三个节点各两条记录；两个子执行各有两条运行记录和一个节点的两条记录，合计 16 条。并发节点之间的记录先后顺序不固定。子记录的 `pulse.graph` 都是 `labeler`，`pulse.path` 分别是 `first`、`second`，`pulse.execution` 各不相同。

## 理解绑定方向

| 绑定 | 数据方向 | 父侧声明 |
|---|---|---|
| `In(parentKey, childKey)` | 父值进入子入口 | 自动成为挂载节点的 `Requires` |
| `Out(childKey, parentKey)` | 普通值在 child.Wait 成功后桥回；Stream / 原生 channel 提前绑定 | 自动成为挂载节点的 `Provides` |
| `Fix(childKey, value)` | 为这一处挂载提供固定输入 | 不占用父侧键 |
| `FixSkip(childKey)` | 为这一处挂载提供正常跳过 | 不占用父侧键 |

`In`、`Out` 均为来源在前、目标在后。泛型绑定要求两端类型相同；动态的 `InRef`、`OutRef` 在装配期比较真实 Go 类型。所有子侧键必须是对应方向的声明端口。

每条必需子入口在每处挂载都要绑定一次。`In` 与 `Fix` 不能同时给同一子入口赋值。一条普通父值可以送往两个不同的子入口；一条子出口也可以送往两个不同的父输出键。

同一子键同时声明为输入和输出，可以透传；若挂载点用 `Out` 接走这条透传出口，就必须用 `In`、`Fix` 或 `FixSkip` 提供其输入。未使用的可选入口可以不绑定。所有挂载都需要至少一条绑定，挂载节点 ID 不能包含路径分隔符 `/`。

固定值属于挂载点，在每次运行中作为该处的输入。它不会深拷贝可变对象；需要每次计算的新值时，由节点生产，不要共享可变的 Fix 值。

## 校验、封闭与复用

`parent.Topology()` 展示静态子树、端口和绑定，不运行任何节点，也不封闭定义。挂载路径由位置推导，嵌套路径如 `first/inner`。

`parent.Validate()` 递归校验整棵子树，成功后封闭所有层；成功 Start 也执行同样的封闭。封闭后不能再添加节点、端口或子图，但定义仍能运行多次，已封闭的子图也能挂到其他未封闭的父图。校验失败一层都不封闭。

每处挂载在运行时创建独立子执行。重复或并发使用定义不会共享槽位、首错和取消域；业务闭包捕获的可变对象仍是应用的共享状态，需要自行同步。组合环会在装配时拒绝，复用同一子图于不同位置不属于环。

## 运行、切面与额度

Sub 不等待所有父输入到齐，也不因全入口跳过而省略 child。In 绑定稳定来源槽，可以仍 pending；各内部节点按自己的 Requires 激活，独立自产分支仍可运行。Stream 与原生 channel 的 Out 在 child.Wait 之前绑定，跨层实时读写共用端点；普通值仍在 child.Wait 成功后桥回。挂载步骤本身仍等待 child 最终完成，真实失败保留原原因。绑定输出全部 skipped 时，挂载正常跳过。

子上下文从挂载节点派生，父取消和挂载点 Timeout 传入。实际写端完整 Aspect 链决定受管流 Done/终态，祖先挂载不是 EOF 边界；child 正常清理不撤销已接纳尾部。[流指南](./streams.md)提供跨 Sub 与宿主订阅片段。

挂载点 Retry 每次创建新子执行。完整有限流水线全部封在 child 内、只桥普通结果时可整体重做；实际外接流入口或出口在 I/O 前拒绝普通 Retry，确定跳过的无数据路径仍正常。不要把 Retry 继承到内部流节点。默认子节点继承父观察者与额度池；挂载本身不占额度，受管流等待暂时让额。子图声明额度则使用独立池。

## 常见错误与下一步

内部键未声明为出口就绑定、漏绑必需入口、重复绑定同一入口、把 `In` 与 `Out` 参数写反，都会在装配或校验时报错。先完成子图接口，再挂载与校验，能使错误在启动之前出现。

声明文件中的子图用法见 [YAML 装配](./assembly.md)；连续数据跨边界还需要了解[流式处理](./streams.md)中的完成与桥回规则。需要恢复子进度时阅读[检查点参考](../reference/checkpoint.md)。
