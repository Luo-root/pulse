// Package kernel 是 pulse v2 的插件化内核：一切皆插件的底座。
//
// 内核只有五个概念，全部围绕"动态可组合性"的两个维度展开
// （参见 cordiverse/paper《A Programming Paradigm for Spatiotemporal
// Composability》，本包是其思想在 Go 下的工程化落地，而非逐字移植）：
//
//   - 时间可组合性（卸载即还原）：组件对环境的每一次修改都以
//     [Context.Effect] 登记，并携带撤销函数；作用域销毁时所有效应
//     按 LIFO 自动 unwind。注册了什么，销毁时就收回什么，不靠
//     开发者自觉清理。
//
//   - 空间可组合性（依赖响应式）：组件通过 [Plugin.Inject] 声明
//     自己依赖哪些服务；依赖满足则装载，依赖消失则自动卸载，
//     依赖恢复则自动重新装载。加载顺序由依赖关系表达，
//     不存在手工排布的启动序列。
//
// 五个概念对应五个源文件：
//
//	context.go  [Context]：服务仓库 + 效应跟踪器 + 作用域树节点
//	service.go  [ServiceKey]：类型安全的服务键，Provide / Get
//	events.go   [EventKey]：事件总线——全树 Emit/Waterfall/Parallel，
//	            另有请求级 EmitLocal/WaterfallLocal（只本 scope）；
//	            Go 的同步调用天然串行累积，不设独立的 serial 入口
//	plugin.go   [Plugin]/[Fiber]：插件声明与惯性生命周期状态机
//	loader.go   [Loader]：声明式配置树 + 增量调和
//	diagnostics.go / snapshot.go：fiber_state / loader_action 与只读快照
//
// # 与 Cordis（TypeScript）的有意差异
//
//   - 服务读取不用 Proxy 属性访问，而是泛型 Get：依赖关系编译期可见；
//   - 效应就是 Go 惯用的 dispose 闭包，不引入额外的迭代器协议；
//   - 不做 realm/isolate 多租户隔离（当前无场景，保留扩展点）；
//   - 不做代码级 HMR（Go 无法卸载已加载代码），Loader 的"重载"
//     是状态级的：dispose 旧 Fiber 再由同一工厂重建；
//   - waterfall 监听器的契约是注册顺序（不支持 prepend）。
package kernel
