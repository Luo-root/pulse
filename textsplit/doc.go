// Package textsplit 是独立的文本分块模块：按尺寸预算把长文本切成带
// 原文 offset 的 Chunk 列表。纯函数、零依赖、零 IO——不绑定任何上层。
//
// # 定位
//
// 刻意不埋进任何具体消费方：当前消费方是 memory/index/openai 的超长
// 输入截断；D3 candidate extractor（切长 episode）、未来文档/文件摄取
// 等模块同样按需 import——谁需要谁拿，不被迫拉一整个 provider 适配器。
//
// # 口径
//
//   - Chunk.Start/End 是**字节 offset**（Go 切片语义：原文[Start:End]
//     恒等于 Chunk.Text）；rune 安全由切分边界保证——切点永远落在
//     rune 边界，不由 offset 单位保证；
//   - 切点按分隔符优先级取预算内最大边界：段落（连续空行）→ 句读
//     （。．！？…!?）→ 空白 → rune 硬切；分隔符保留在左侧块尾；
//   - 尺寸函数注入（Options.Size，默认 rune 计数）——精确 token 预算
//     由宿主换自己的度量；Size 假定对前缀单调不减；
//   - Overlap > 0 时相邻块重叠（重叠区对齐分隔符边界，Size ≤ Overlap
//     取最大）——滑窗切块的基础形态；
//   - 退化保证：单个 rune 超预算时该 rune 独立成块（进度保证，不死
//     循环）；空文本返回空切片。
package textsplit
