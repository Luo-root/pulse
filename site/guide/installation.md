# 安装与版本

本页帮助你选择与文档匹配的版本、建立 Go 模块，并确认实际使用的依赖。完成后可继续[快速开始](quickstart.md)。

## 环境要求

需要 **Go 1.25.0 或更新版本**。执行以下命令确认工具链：

```sh
go version
```

只有根包时，执行引擎的依赖闭包只包含标准库。使用 `pulse/yaml` 时会额外引入 `gopkg.in/yaml.v3`；`pulse/observe` 不增加第三方运行依赖。

## 选择版本

| 版本 | 适用情况 | 对应资料 |
| --- | --- | --- |
| 正式 tag `v0.4.1` | 需要已发布版本，并愿意使用该版本的 API | [v0.4.1 源码与说明](https://github.com/Luo-root/pulse/tree/v0.4.1) |
| 公开提交 `1f67045d6c42` | 按本站教程使用可复用 Graph、Execution、静态子图和检查点桥 | 本站指南与参考 |
| `main` | 参与开发，主动接受后续变化 | [main 源码](https://github.com/Luo-root/pulse) |

**本站对应开发 API，并不等同于 v0.4.1。** 教程固定到一份已合入 main 的公开提交，避免无版本安装自动选择旧 tag。Go 会将提交解析为伪版本并写入 `go.mod`，这是正常的模块版本形式。

尚未发布的目标版本以维护者实际创建的 tag / release 为准。0.x 期间，破坏性变更只随 minor 发布；升级前请查看[迁移说明](../reference/migration.md)。

## 建立示例模块

在准备保存示例的目录执行：

```sh
mkdir pulse-example
cd pulse-example
go mod init example.com/pulse-example
go get github.com/Luo-root/pulse@1f67045d6c42
```

将[快速开始](quickstart.md)中的程序保存为 `main.go`，然后执行：

```sh
go run .
```

`go.mod` 和 `go.sum` 应纳入应用的版本管理，使团队与 CI 使用同一份依赖。

## 核对实际依赖

```sh
go list -m github.com/Luo-root/pulse
```

输出应包含对应 `1f67045d6c42` 的伪版本。如果输出为 v0.4.1，再执行上面的固定版本安装命令。

如果输出显示 `=>`，说明当前模块或 `go.work` 使用了本地替换。检查应用的 `go.mod` 和 `go.work`，确认它们是否有意指向另一份源码。独立验收公共依赖时，可使用 `go env -w` 之外的进程级 `GOWORK=off` 设置；具体环境变量语法依 shell 而定。

## 可选包

各包都属于同一 Go 模块，安装根模块后即可导入：

```go
import (
	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)
```

这段列出了可选导入路径，实际程序只保留使用到的包，避免 Go 的未使用导入错误。如何选择与使用可选包见[包参考](../packages/index.md)。

## 常见问题

**编译器提示 `New` 或 `Run` 参数不匹配。** 先用 `go list -m` 核对版本。旧 tag 与本站开发 API 的对象模型不同，混用两份文档会出现签名错误。

**工具链下载失败。** 确认本地有 Go 1.25+，或者按所在环境配置 Go 的工具链和模块下载网络。Pulse 运行本身不需要在线服务或凭据。

**需要跨进程恢复。** 继续阅读[检查点桥](../reference/checkpoint.md)，并在独立的 [Durable 仓库](https://github.com/Luo-root/pulse-durable)按其安装说明引入恢复层。Pulse 核心不负责保存历史执行记录。

下一步：[运行第一个工作流](quickstart.md)。
