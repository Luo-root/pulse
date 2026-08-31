// Package selfedit 是 P2-C 的 self-edit 记忆工具组：把 memory/store 的写
// 路径（Put/Supersede/Revoke）适配成三个模型可见工具，显式 opt-in 注册。
//
// # 定位（设计文 §17.1 / §17.7-4）
//
// 记忆管理权在宿主管线 + 审批：self-edit 本质是一组**写工具**，天然挂现有
// Preview/审批面（#56 W2：Registration.PreviewFn + before_tool_call HITL），
// 不开专用审批通道。本包不做读工具——检索归 memory/assemble（§8）。
//
// # 不变式
//
//   - scope 防污染：namespace/SourceRefs/Taint/Confidence/Status/Revision
//     全部由 Options 钉死或固定，模型参数最小化（kind/content/structured/
//     id/reason）——scope 是存储层边界，不是提示词约定；
//   - 回链强制：写入 SourceRefs 只来自 Options.OriginFn()（session 回链），
//     缺省 Register 直接失败，不静默降级为无来源；
//   - 写权限口径：supersede/revoke 先 Get(Namespace, id)（不可见即不存在），
//     且要求 item.Namespace 与 env.Namespace **完全相等**——store 前缀
//     可见性是读口径（向下可见），写入钉死 env.ns：父 scope 工具不得
//     下钻改写子 scope item（ErrOutsideScope）；
//   - taint 保守默认：写入默认 TaintUntrustedExt（§17.7 ASI06 对位——
//     self-edit 是模型复述工具/外部内容的通道，不得默认与宿主权威写入
//     同级；before_tool_call 审批是晋升闸，宿主可显式覆盖）；
//   - 禁物理 DELETE：只有 supersede（留痕替代）与 revoke（幂等作废），
//     状态机错误原样透传 store 哨兵；
//   - Preview 只读：三工具卡片 kind=opaque/action=write（envelope #56 已
//     冻结，不加第五种 kind），Preview 不落盘。
//
// # 接入姿态
//
//	selfedit.Register(scope, reg, selfedit.Options{
//	    Store:     memStore,
//	    Namespace: scope.Namespace(),            // store.MemoryScope 展开
//	    OriginFn:  func() store.SourceRef { … }, // 当前 session+seq 回链
//	})
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §12/§17；
// 实现票 #82。使用细节见 README_zh.md。
package selfedit
