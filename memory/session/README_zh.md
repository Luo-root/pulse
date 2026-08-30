# memory/session

P2 记忆层的会话事件日志：append-only 日志是唯一真相，`Surface() []*llm.Message` 只是投影。
包文档（godoc）见 `doc.go`；设计事实源 [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §6/§7/§9；实现票 #68（A1）、#70（A2）、#73（P2-B Replace fold）。

## 接口面

```go
store := session.NewMemoryStore()               // A1：内存，进程内单写者
store, err := session.NewJSONLStore(root)       // A2：JSONL + blobs + 文件锁

sess, err := store.Create(ctx, session.SessionHeader{})
sess, err := store.Open(ctx, id)                // 非 live 打开 = 冷恢复入口
page, next, err := store.List(ctx, session.SessionFilter{After: cursor})
err := store.Delete(ctx, id)

sess.Header()                                   // SessionHeader 快照
env, err := sess.Append(ctx, session.EventDraft{Type, Data, Surface, Ignorable})
events, err := sess.Events(ctx, fromSeq)        // Seq >= fromSeq 的拷贝
msgs, err := sess.Surface(ctx)                  // []*llm.Message（不含 system）
child, err := sess.Fork(ctx, atSeq)             // 切 tool 组中间 → 拒绝
err := sess.Flush(ctx)                          // 内存版 no-op；JSONL 版 fsync
reg := sess.Registry()                          // 事件 codec 环境（FoldTrace 用）
```

- `Seq`/`Time` 由 store 分配；`Append` **非幂等**——Flush 失败后不要原样重放同一批事件。
- `Open` 即冷恢复（无独立 Recover 方法）：未闭合 turn/step、unpaired ToolCall 合成闭合事件**真实写回日志**后再 fold；live 会话不冷补；恢复幂等。
- JSONL 实现额外提供 `Close() error`（类型断言使用）：释放文件锁与句柄，幂等。

## 两个 backend

| | MemoryStore | JSONLStore |
|---|---|---|
| Flush | 成功空操作（语义占位） | `f.Sync()`，崩溃只保证 Flush 点之前 |
| 单写者 | 进程内锁（同 ID 并发 Create 恰好一个胜出） | 进程内锁 + 文件锁（`O_EXCL`，stale 抢占，`Flush` 兼作心跳） |
| 撕裂恢复 | 不适用（无文件） | 无换行尾行丢弃并物理截断；中部坏行 / seq 断链 → `ErrCorruptLog` |
| 持久 header | — | 写 `compaction.checkpoint` 后同步重写（FormatVersion 抬 2） |
| List | 内存排序 + 游标 | 扫描 `{root}/*/header.json` + 游标 |

JSONL 落盘布局：`{root}/{sessionID}/header.json` + `events.jsonl`（每行一条信封）+ `blobs/{sha256}`（>32KiB 内联字节溢出，内容寻址去重、sha 自校验、缺失即加载错误）+ `lock`。**JSONL 为明文：文件即密钥面、路径宿主拥有。** `blob:` URL 前缀为本包保留，宿主自带 `blob:` URL 需换 scheme。

## 事件分级与裁决

每个 `EventType` 在 `Registry` 绑定 payload 校验与分级；裁决表（§6.3 评审定案）：

| 情况 | 行为 |
|---|---|
| 已知 Required（生命周期/消息/工具/compaction.*） | 永不跳过（忽略信封 flag） |
| 已知 Ignorable（chunk / request.header / route / usage） | 可跳过；fold 不读它 |
| 未知扩展 + `Ignorable=true` | 写入并 fold 跳过 |
| 未知 + flag 默认 false | **Append 即拒绝**（fail closed） |

Ignorable ≠ 可以不记：`request.header` 仍必须由写入方发（system + ToolDef + model 三样），写入方是 session→loop 的装配层桥。

## 常见错误

| 哨兵 | 语义 |
|---|---|
| `ErrSessionExists` / `ErrSessionNotFound` | Create 撞 ID（拒绝第二写者）/ Open、Delete 不存在 |
| `ErrWriterBusy` | 文件锁被持有（fail-fast；stale 阈值后可抢占） |
| `ErrUnknownEvent` / `ErrUnknownRequired` | 写入未知 required / 恢复时日志含未知 required |
| `ErrSurfaceNotAllowed` / `ErrReplaceNotSupported` / `ErrReplaceRange` | 非 surface 类型带 Surface / Replace 类型未注册 / 窗口反向、越界或造成 pairing 孤儿 |
| `ErrCorruptLog` | 持久日志损坏（中部坏行、seq 断链、blob checksum 不符） |
| `ErrForkSplitToolGroup` / `ErrForkBadAt` | Fork 切在 tool 组中间 / 切点越界 |
| `ErrFormatVersion` | header 版本不兼容（只认 1 与 2，不猜测迁移） |
| `ErrSessionClosed` / `ErrInvalidSessionID` / `ErrCursorStale` / `ErrDeleted` | Close 后写入 / ID 非法（须匹配 `[A-Za-z0-9_-]{1,128}`）/ List 游标失效 / 已删除会话写入 |

## 测试

```bash
go test -race -count=1 ./memory/session/...
```
