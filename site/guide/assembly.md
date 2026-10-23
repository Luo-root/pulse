# YAML 装配

本页面向希望把流程拓扑放入配置文件、同时保留 Go 业务实现的开发者。需要 Go 1.26+，并了解[节点读写声明](./orchestration.md)与[静态子图](./subgraphs.md)。示例使用开发版 API。

`pulse/yaml` 将 YAML 装配为 `Graph` 和 `SeedPlan`。YAML 声明节点、依赖与子图接线；注册表提供键的真实类型和具名业务函数。注册的函数只负责 Run，不返回 Node，也不决定拓扑位置。

## 完整示例：装配子图并读取结果

把程序保存为一个空目录中的 `main.go`。YAML 的根输入来自 literal，子图有自己的固定前缀；宿主将计划转换为本次输入，再通过声明出口读取报告。

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/yaml"
)

const document = `
version: 1
description: Assemble a reusable summary step
seeds:
  - key: {name: topic, type: string}
    from: {kind: literal, value: Pulse}
graphs:
  summarize:
    description: Add a prefix
    seeds:
      - key: {name: child.prefix, type: string}
        from: {kind: literal, value: "about "}
    nodes:
      - id: format
        uses: demo.format
        requires:
          - {name: child.topic, type: string}
          - {name: child.prefix, type: string}
        provides: [{name: child.summary, type: string}]
nodes:
  - id: summary
    description: Run the summary step
    graph: summarize
    in: {child.topic: topic}
    out: {child.summary: report}
    timeout: 2s
    retry: {attempts: 3, delay: 10ms}
`

func run() error {
	topic := pulse.NewKey[string]("topic")
	childTopic := pulse.NewKey[string]("child.topic")
	prefix := pulse.NewKey[string]("child.prefix")
	summary := pulse.NewKey[string]("child.summary")
	report := pulse.NewKey[string]("report")
	reg := pulse.NewRegistry()
	for _, key := range pulse.Keys(topic, childTopic, prefix, summary, report) {
		if err := pulse.RegisterKey(reg, key); err != nil {
			return err
		}
	}
	if err := reg.Register("demo.format", func(rc *pulse.RunCtx) error {
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
		return pulse.Set(rc, summary, p+value)
	}); err != nil {
		return err
	}
	g, plan, err := yaml.Load([]byte(document), reg, yaml.LoadOptions{
		GraphID: "yaml-demo", MaxRunning: 2,
	})
	if err != nil {
		return err
	}
	if err := g.DeclareOutput(pulse.NewPort(report)); err != nil {
		return err
	}
	if err := g.Validate(); err != nil {
		return err
	}
	in, err := plan.Inputs(nil)
	if err != nil {
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

在该目录运行：

```sh
go mod init example.com/pulse-yaml
go get github.com/Luo-root/pulse@main
go run .
```

预期输出：

```text
report=about Pulse state=ready
```

`Load` 查注册表并装配定义，不运行业务体，也不读取外部输入。`plan.Inputs` 解析根层输入计划；子图 literal 在挂载处形成 Fix。宿主在 Validate 前补充根出口，并由 NewExecution/Start/Wait 控制本次运行。

## 文档字段

| 字段 | 含义 |
|---|---|
| `version` | 缺省、0 或 1，其他版本拒绝 |
| `description` | 根图说明；`graphs.<name>.description` 为子图说明 |
| `seeds[].key` | `{name, type}`，必须与注册表对应 |
| `seeds[].from` | `kind` 为 literal、env、file、context；缺省 kind 按 literal 处理 |
| `seeds[].skip` | true 表示本次正常跳过，该条 from 不取值 |
| `nodes[].id` / `description` | 节点身份与说明 |
| `nodes[].uses` | 注册表中的业务函数名 |
| `nodes[].requires` / `provides` | 工厂节点的键声明列表 |
| `graphs` / `nodes[].graph` | 可复用子图表与引用，graph 与 uses 二选一 |
| `nodes[].in` / `out` | 子图绑定，均写作 `子键: 父键` |
| `nodes[].timeout` | Go 时长形式，例如 2s；装为外层 Timeout |
| `nodes[].retry` | attempts 和 delay；装为内层 Retry |
| `observer` | 文档提示字段，Load 忽略；观察者通过选项配置 |

`version` 和各层节点的 `retry.attempts` 填写数值时，必须是本机 Go `int` 范围内的 YAML 整数；`1.9`、`1.0`、`2e0`、数字字符串和越界值都会报错。省略或 `null` 按未设置处理；version 的 0 / 1、attempts <= 0 按一次尝试的规则不变。业务 `from.value` 不受配置整数限制。

`type` 是注册键的 `reflect.Type.String()`，不是任意类型别名。使用 `Registry.TypeTagOf` 可以查询期望标签；同名跨类型、缺标签、未注册键与未注册 uses 都会报错。YAML 没有独立流 runner 或专用流糖语法。登记同一 `Key[pulse.Stream[T]]`，工厂只返回 Run，在 Run 内用 OpenStream、Receive、HandleMessage 等 core API；Requires/Provides 仍由 YAML 显式声明，子图 in/out 复用实时绑定契约。不能把 channel 藏进工厂后声称拓扑已声明齐全，Analyze 也不会执行工厂推断它。

## 外部输入由宿主解析

对于 env、file、context 等非 literal 输入，宿主把 resolver 传给 `plan.Inputs`。下面片段放在返回 `error` 的宿主函数内，需要 `os`、`fmt` 和 `yaml` 导入，已有 `plan` 与 `g`：

```go
in, err := plan.Inputs(func(from yaml.SeedFrom) (any, error) {
	if from.Kind != "env" {
		return nil, fmt.Errorf("unsupported seed kind %q", from.Kind)
	}
	value, ok := os.LookupEnv(from.Env)
	if !ok {
		return nil, fmt.Errorf("environment variable %q is not set", from.Env)
	}
	return value, nil
})
if err != nil {
	return err
}
if err := g.Run(ctx, in); err != nil {
	return err
}
```

这里 `ctx` 为宿主已有的执行上下文。resolver 返回值仍要与键类型相符。literal 的通用切片和映射会递归对齐到登记的容器类型；不会猜测 map 到 struct 的字段关系，也不会进行字符串与数字互转、浮点截断或越界收窄。需要业务转换时，由 resolver 返回正确类型。

## 子图与校验范围

子图节点的接口由 `in`、`out` 和子图 seeds 推导，不能同时写 requires/provides；工厂节点也不能写 in/out。绑定两端真实类型要一致，子入口不能由内部节点再次生产。子图实际取值的 seed 只支持 literal；skip seed 不取 from。

每处引用在 Load 期装配一份子图定义，并按该处的输入输出检查；同一 spec 挂载两次是两个位置。运行时每处又创建独立子执行。引用环、空子图、无效接线和被引用子树的来源问题会在 Load 报出挂载路径。

未引用 spec 没有确定的接口，因此不按闭合图要求所有输入已有来源；已知来源冲突、重复 ID、环等仍检查。根图可在 Load 之后由宿主补充声明，根层的静态问题留给 Validate/Start，本次必需输入留给 NewExecution/Start。不要把 Load 成功等同于完整根图已经可启动。

## 只检查声明：Analyze

下面片段需要已有 YAML 字节 `data`、`fmt` 与 `yaml` 导入，放在返回 `error` 的函数内：

```go
topology, err := yaml.Analyze(data, yaml.LoadOptions{GraphID: "preview"})
if err != nil {
	return err
}
for _, problem := range topology.Problems {
	fmt.Println(problem)
}
fmt.Println(topology.Mermaid())
```

Analyze 不需要 Registry，不执行工厂、切面、resolver 或 ObserverFor。它复用 Load 的装配规则及不依赖真实键类型的静态判据；这部分根层问题保留在 Problems，子图或文档装配问题返回 error。返回的 Type 是 YAML 标签，无法推导的接线键标签为空；它不验证实际 Go 类型、工厂是否登记或 literal 是否可赋值。

**类型标签不等于流静态安全证明。** Analyze 装配时所有键都用 `any` 占位，最后恢复标签，因此即使显式声明 `pulse.Stream[int]` 或 channel，也不会检查依赖真实类型的流消费者、共享额度或生命周期约束。例如，只有一个节点 provides `pulse.Stream[int]` 而无人读取，Analyze 的 Problems 仍可为空；用真实流键登记后 Load，根图的 Topology.Problems 会报告缺失读取者，Validate 也会拒绝，无需执行工厂。预览通过后仍须经有 Registry 的 Load、Inputs 和 Validate / Start 检查。无流图在标签可知、身份、接线与静态选项一致时可逐字段比较拓扑；流图的 Problems 不承诺一致。

## 额度、观察者与常见错误

用 `LoadOptions.MaxRunning` 配置整棵声明树的额度；默认不限。避免同时在 `LoadOptions.Graph` 写 WithMaxRunning：只有 MaxRunning 大于零才覆盖该声明，零值不会把已有声明重置为不限。

根观察者放在 `LoadOptions.Graph`；子图默认继承。只有不同挂载需要不同目的地时才提供 `ObserverFor(path)`。它在真实 Load 装配每处子图时调用一次，根层不调用，校验与 Analyze 也不调用。

超时和重试放在子图引用节点时，作用于整个子执行；放在子图内部节点时，只作用于那一步。顺序固定为 Timeout 在外、Retry 在内。调用出错时应先区分装配、输入解析还是运行失败，再按[错误参考](../reference/errors.md)处理，避免将错误替换为跳过。

## 下一步

[观测与诊断](./observability.md)展示静态预览和运行状态叠加；[生命周期参考](../reference/lifecycle.md)给出校验与启动边界。[包文档](../packages/index.md)提供进一步的公开 API 入口。
