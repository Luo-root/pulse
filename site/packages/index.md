# 包参考

Pulse 提供图执行核心，以及可按需引入的观测和 YAML 装配包。本页按职责列出入口，便于查询配置、事件、字段和严格语义。

## 包与依赖

| 导入路径 | 职责 | 依赖 | 参考入口 |
|---|---|---|---|
| `github.com/Luo-root/pulse` | 图定义、运行输入、Execution、节点与静态子图、切面和状态视图 | 标准库 | [Go API 文档](https://pkg.go.dev/github.com/Luo-root/pulse) · [当前源码](https://github.com/Luo-root/pulse) |
| `github.com/Luo-root/pulse/observe` | 将 Observer 回调转换为结构化 Record，输出文本、slog、内存或异步记录 | `pulse` + 标准库 | [observe 包参考](/packages/observe/) |
| `github.com/Luo-root/pulse/yaml` | 按登记的 Run 执行体装配 YAML 图，生成输入计划或静态拓扑 | `pulse` + `gopkg.in/yaml.v3` + 标准库 | [yaml 包参考](/packages/yaml/) |

依赖方向为 `pulse` ← `observe` / `yaml`。根包不会反向引入这些包；只导入根包时，依赖闭包仅含标准库。观测与 YAML 装配可独立选用。

## 按问题查阅

| 要查什么 | 入口 |
|---|---|
| 图定义与一次执行有什么区别，如何声明输入、读取输出？ | [核心概念](/guide/concepts) · [输入与输出](/guide/inputs-outputs) |
| 图怎样连接节点、挂载子图、处理重试与取消？ | [编排](/guide/orchestration) · [静态子图](/guide/subgraphs) · [重试与取消](/guide/retry-cancellation) |
| 某条观测事件何时产生，TraceID 与 Execution 如何区分，出口什么时候刷新？ | [observe 包参考](/packages/observe/) |
| YAML 有哪些字段，怎样登记工厂与类型，Load 和 Analyze 分别校验什么？ | [yaml 包参考](/packages/yaml/) |
| 从头接入观测或声明式装图？ | [图观测指南](/guide/observability) · [声明式装图指南](/guide/assembly) |

包参考提供完整的最小程序和查阅表；使用指南按步骤说明接入过程。observe 与 yaml 的站点正文来自仓库对应的双语包 README，源码链接可用于核对当前契约。

## 文档对应的版本

本网站以 `v0.5.0` 为 API 基线，以下命令固定安装该版本。后续开发改动或尚未合并的分支按[安装页](../guide/installation.md)选择主线或对应本地源码：

```bash
go get github.com/Luo-root/pulse@v0.5.0
```

需要 Go 1.25.0 或更高版本。发布版本的 Go API 文档与当前源码可能不同，请对照所使用的提交阅读。
