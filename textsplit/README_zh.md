# textsplit

独立的文本分块模块（Issue #86，D1.5）：按尺寸预算把长文本切成带原文 offset 的 Chunk 列表。**纯函数、零依赖、零 IO**——刻意不绑定 memory/index：当前消费方是 [memory/index/openai](../memory/index/README_zh.md) 的超长输入截断；D3 candidate extractor（切长 episode）、未来文档/文件摄取等模块同样按需 import。

## 接口

```go
chunks, err := textsplit.Split(text, textsplit.Options{
    MaxLen:  800,   // 单块尺寸上限（按 Size 度量），必填
    Overlap: 100,   // 相邻块重叠预算，0 = 不重叠；必须 < MaxLen
    // Size: myTokenizer, // 尺寸度量，nil = rune 计数；假定对前缀单调不减
})
// Chunk{Text string, Start, End int} —— 字节 offset
```

## 口径

- **字节 offset**：`text[Start:End]` 恒等于 `Chunk.Text`（Go 字节切片语义）；rune 安全由切分边界保证——切点永远落在 rune 边界，不由 offset 单位保证。
- **分隔符优先级**：段落（连续空行）→ 句读（。．！？…!?；v1 启发式，`.` 会切分 `3.14`）→ 空白 → rune 硬切；分隔符保留在左侧块尾；每级取预算内最大切点。
- **Size 注入**：nil = rune 计数；精确 token 预算由宿主换自己的度量（不引 tokenizer 依赖，对齐 CharMeter 先例）。
- **Overlap**：重叠区按 Size ≤ Overlap 取最大、对齐分隔符边界；无可用边界的段落退化为不重叠。
- **退化保证**：单个 rune 超预算独立成块（进度保证，不死循环）；空文本返回空切片。

## 测试

```bash
go test -race -count=1 ./textsplit/...
```
