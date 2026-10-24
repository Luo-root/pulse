# 安装与版本

本页帮助你选择与文档匹配的版本、建立 Go 模块，并确认实际使用的依赖。完成后可继续[快速开始](quickstart.md)。

## 环境要求

当前发布版本 **v0.5.1** 需要 **Go 1.26.0 或更新版本**，与 Durable 统一。最低工具链升级有兼容性影响，但公开 API 和运行语义未变；v0.5.0 的 Go 1.25.0+ 要求仅作为历史兼容信息保留。执行以下命令确认工具链：

```sh
go version
```

只有根包时，执行引擎的依赖闭包只包含标准库。使用 `pulse/yaml` 时会额外引入 `gopkg.in/yaml.v3`；`pulse/observe` 不增加第三方运行依赖。

## 选择版本

| 版本 | 适用情况 | 对应资料 |
| --- | --- | --- |
| `v0.5.1` | 当前源码与发布准备基线 | [迁移步骤](../reference/migration.md) · [统一生产性能对比](../reference/production-comparison.md) |
| `v0.5.0` | 维护已固定的旧版本 | [统一生产性能对比](../reference/production-comparison.md) |
| 历史 tag `v0.4.1` | 维护旧 API，尚未迁移 | [v0.4.1 源码与说明](https://github.com/Luo-root/pulse/tree/v0.4.1) |
| `main` | 使用已合入主线的开发 API | [main 源码](https://github.com/Luo-root/pulse) |
| 与文档一起检出的本地源码 | 试用尚未合并的修复或参与开发 | 本站指南与参考；使用下方本地替换步骤 |

**本站以 v0.5.1 为 API 基线，不适用于 v0.4.1。** 默认使用固定 tag；只有需要后续开发改动时才选择 `go get …@main`。后者会把当时的主线解析为具体版本并写入 `go.mod`；提交该文件后，团队使用的是解析出的版本，不会每次运行自动更新。

v0.5.1 包含受管 Stream、实时 Sub、消息尝试与增量检查点，可用于运行本站示例。试用尚未合并的修改时，再按下方步骤使用对应本地源码。旧公开快照 `1f67045d6c42` 不包含语法糖的 `description` 参数，不能用于编译新的语法糖示例。

尚未发布的目标版本以维护者实际创建的 tag / release 为准。0.x 期间，破坏性变更只随 minor 发布；升级前请查看[迁移说明](../reference/migration.md)。

## 建立示例模块

在准备保存示例的目录执行：

```sh
mkdir pulse-example
cd pulse-example
go mod init example.com/pulse-example
go get github.com/Luo-root/pulse@v0.5.1
```

将[快速开始](quickstart.md)中的程序保存为 `main.go`，然后执行：

```sh
go run .
```

`go.mod` 和 `go.sum` 应纳入应用的版本管理，使团队与 CI 使用同一份依赖。

## 试用尚未合并的源码

先取得包含所需改动、并与本文档一起提供的源码检出。下面假设目录名为 `pulse`，示例模块 `pulse-example` 与它相邻。在示例模块中设置本地替换：

```sh
go mod edit -replace=github.com/Luo-root/pulse=../pulse
```

保存示例 `main.go` 后执行：

```sh
go mod tidy
go list -m github.com/Luo-root/pulse
go run .
```

此时输出中的 `=> ../pulse` 才说明实际使用了这份本地源码；`go.mod` 左侧即使保留旧版本，也不代表运行的还是旧实现。相对路径按你的目录布局调整。正式应用不要提交只在个人机器上存在的替换路径；改动合入后先移除替换，再选择包含该改动的公开提交并提交解析出的版本：

```sh
go mod edit -dropreplace=github.com/Luo-root/pulse
go get github.com/Luo-root/pulse@main
```

## 核对实际依赖

```sh
go list -m -json github.com/Luo-root/pulse
```

无本地替换时，核对 Version 对应的公开提交；有替换时，核对 Replace.Dir 是否指向与文档匹配的检出。另检查 `go.work` 是否有意覆盖模块选择。独立验收公共依赖时，可设置进程级 `GOWORK=off`，但它不会移除 `go.mod` 内的 replace，二者须分别检查；不要为一次检查修改全局 Go 设置。

## 可选包

各包都属于同一 Go 模块，安装根模块后即可导入：

```go
import (
	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)
```

实际程序只保留使用到的包，避免 Go 的未使用导入错误。如何选择与使用可选包见[包参考](../packages/index.md)。

## 常见问题

**编译器提示构造函数或 `Run` 参数不匹配。** 先核对实际源码。旧 tag、早期开发快照与本站 API 不同；五个节点语法糖现在都在 `id` 后接 `description`，允许空字符串。尚未合并的改动必须使用对应本地检出。

**工具链下载失败。** 当前源码需 Go 1.26.0+（已发布的 v0.5.0 原要求为 Go 1.25.0+）；确认选择了对应工具链，或者按所在环境配置 Go 的工具链和模块下载网络。Pulse 运行本身不需要在线服务或凭据。

**需要跨进程恢复。** 继续阅读[检查点桥](../reference/checkpoint.md)，并在独立的 [Durable 仓库](https://github.com/Luo-root/pulse-durable)按其安装说明引入恢复层。Pulse 核心不负责保存历史执行记录。

下一步：[运行第一个工作流](quickstart.md)。
