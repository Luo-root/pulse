// Package lsp 把外部语言服务器（gopls、typescript-language-server、
// pyright…）挂成 toolset 的单只读工具（Issue #64）。可选包：依赖外部
// 进程、按语言显式配置，不进 builtins。
//
// # 形态
//
// 单工具 `lsp`（RiskReadonly），op 分发：diagnostics / definition /
// references / hover。`line`/`column` 0 基，column 是 LSP 原生 UTF-16
// code units。参数与行为详见 README_zh.md。
//
// # lazy 生命周期与清理双路
//
// 首次调用按扩展名 spawn（strings.Fields 分词）→ initialize/initialized
// → didOpen；进程常驻到 dispose。启动/握手失败只对该语言报错，下次调用
// 重试。清理双路：显式 dispose() 与 scope Dispose（独立 Effect）都
// shutdown → exit → 树杀。
//
// # 只读 op 面，内容同步双向
//
// 不做 formatting / rename / codeAction 等写类 op；但每次调用把磁盘最新
// 内容同步给 server（首次 didOpen，之后按内容 hash 变化 didChange 全量、
// version++）——edit/apply_patch 改完再调 diagnostics 即得最新诊断。
//
// # 零依赖与测试缝
//
// 手写 JSON-RPC stdio 分帧（Content-Length header），不引
// golang.org/x/tools。spawnServer 包内变量是测试缝：注入内存 fake 钉
// 协议序（initialize → initialized → didOpen → request）。
//
// 接入：lsp.Register(host, reg, lsp.Options{Root, Servers})，返回的
// dispose 释放全部 server；契约要点、刻意不做与测试见 README_zh.md。
package lsp
