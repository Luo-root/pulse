// Package yaml 把声明式流程图装成 kernel/flow.Graph（E2 拓扑归属 A）。
//
// YAML 解码留在本子包，flow 核心保持只依赖标准库。
// Key 经 flow.Registry 的 name+type 登记表解析，本包不 import llm / observability。
package yaml
