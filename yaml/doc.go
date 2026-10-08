// Package yaml 把声明式流程图装成 pulse.Graph。
//
// 拓扑归 YAML：节点必填 id / uses / requires / provides；uses 对应
// pulse.Registry 上的具名 Run 工厂——工厂只给 Run，不返回 *Node。
// Key 经 pulse.Registry 的 name+type 登记表解析。
//
// YAML 解码留在本子包，pulse 根包保持只依赖标准库。
package yaml
