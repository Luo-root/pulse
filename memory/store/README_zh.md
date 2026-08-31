# memory/store

P2-C 的长期记忆 canonical store：`MemoryItem` 的 Put/Get/Search/Supersede/Revoke。
包文档（godoc）见 `doc.go`；设计事实源 [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §6.5/§10/§13.1；实现票 #76（C1）。SQLite + FTS 在 C2，Context Assembler 在 C3。

## 接口面

```go
store := store.NewMemoryStore() // C1：内存实现
store, err := store.NewSQLiteStore(ctx, "file:/path/to/memory.db") // C2：SQLite + FTS5（CGO-free）
scope := store.MemoryScope{TenantID: "acme", UserID: "u1"}

it, err := store.Put(ctx, item, store.PutMemoryOptions{ExpectedRevision: 0}) // 0=新建，>0=CAS 更新
got, err := store.Get(ctx, scope.Namespace(), "d1")
hits, err := store.Search(ctx, store.MemoryQuery{Namespace: scope.Namespace(), Query: "toml"})
next, err := store.Supersede(ctx, "d1", newItem)
err := store.Revoke(ctx, "d1", "policy changed")
audit := s.AuditLog() // 实现特有：Supersede/Revoke 审计（reason 落点）
```

- `Revision / KnownAt / CreatedAt / UpdatedAt` 由 store 分配，调用方不填；更新时不可变字段保留、`Revision` 前进。
- `ExpectedRevision`：0 = 新建（撞 ID → `ErrItemExists`）；>0 = CAS（不匹配 → `ErrRevisionConflict` 且数据不变）。

## Namespace 可见性（canonical 键）

- `MemoryScope` 只是 helper：按固定顺序展开成自描述层级 `tenant:<id>` → `user:<id>` → `project:<id>` → `workspace:<id>` → `agent:<id>`（空字段跳过）；新维度追加层级即可。
- 可见性 = **前缀匹配**：查询 namespace 是 item namespace 的前缀即可见——父 scope 读得到子 scope，**兄弟 scope 绝不互见**（验收核心）；查询为空 = 全局。

## 状态机（禁物理 DELETE）

```
Active ──Supersede──▶ 新 item（Active）；旧 item ──▶ Superseded
Active/Pending ──Revoke──▶ Revoked（终态；reason 走 store 审计）
```

- 接口无 Delete：Superseded/Revoked 的 item 永久可查（`IncludeInactive`）——审计与回滚的前提。
- **状态迁移只走 Supersede/Revoke**：Put 更新禁止改变 Status（`ErrStatusTransition`）——否则 active→pending 绕过 P2-D 的 taint gate、active→superseded 绕过替代链。
- Revoked 是终态：不可再 Supersede；Revoke 幂等；对 Superseded item Revoke → `ErrRevokeSuperseded`（操作对象错了，先找生效版本）。
- `StatusPending` 的产生与晋升不在本包：本包只定义其存储语义——写入路径是 `candidate.Extract`（自动提炼），Pending 对默认 Search（只 Active）**不可见**，晋升走 Supersede 不走 Put（宿主盖章）；语义全貌见 [candidate README](../candidate/README_zh.md) 与[根 README](../README_zh.md)「状态机总览」。

## 写入校验（fail closed）

- `SourceRefs` 至少一条：session 来源必须带 `SessionID`+`Seq`（可定位 canonical event），manual/external 必须带 `Ref`——**没有来源的模型推断不进入 active memory**（§10.2）。
- `StatusActive` 必须显式给 `Confidence > 0`（P2-C 无 scoring 产出方，排序不得依赖没人写的值；「默认 1.0」是文档建议值）。
- Kind/Taint 是 open string（宿主自定义放行）；Status 未知即拒；`Structured` 必须合法 JSON；`ValidUntil` 不得早于 `ValidFrom`。
- 审计与隔离优先：`Revoke` 的 reason 不进 `MemoryItem`（§6.5 设计冻结），落 store 审计面——C2 落 SQLite audit 表。

## Search 语义

- Namespace 前缀 + `Kinds` 过滤 + 关键词（**仅匹配 Content**，大小写不敏感子串；Structured 是否入检索域由 C2 FTS 定）+ 状态开关（默认只 Active）。
- 排序 = UpdatedAt 降序 + ID tiebreak（稳定，不依赖 Confidence——没人写的值不参与排序）；`Limit` 是硬上限（超页场景 C2 定 keyset）。
- 未命中返回空切片，**不伪造**。

## 错误速查

| 哨兵 | 语义 |
|---|---|
| `ErrItemExists` / `ErrItemNotFound` | Put 新建撞 ID / Get、Supersede、Revoke 目标不存在（含 namespace 不互见） |
| `ErrRevisionConflict` | CAS 失败，数据不变 |
| `ErrInvalidItem` / `ErrInvalidQuery` | item 校验失败（形状/来源/置信度）/ Search 条件非法 |
| `ErrSupersedeRevoked` / `ErrSupersedeSelf` | 对 Revoked item Supersede（终态）/ next.ID 与 oldID 相同 |
| `ErrRevokeSuperseded` / `ErrStatusTransition` | 对 Superseded item Revoke / Put 更新试图改 Status |

## 测试

```bash
go test -race -count=1 ./memory/store/...
```

## SQLite backend（C2，#78）

- **CGO-free**：`modernc.org/sqlite`（FTS5 默认启用）；`sqlite.go`/`sqlite_test.go` 带 `//go:build !plan9 && !js` 构建约束——plan9/js 下 SQLite backend 缺席但 **store 主包照常编译**（core 不被锁死，内存实现可用）。
- **落盘**：`memory_items` 表（namespace 以 `\x1f` join 成 `ns_key`，前缀匹配按元素边界安全）+ **FTS5 外部内容表**（`content=`，触发器随增删改同步）+ `memory_audit` 表（reason 落点）。
- **schema 版本**：`PRAGMA user_version`，不兼容拒绝加载（不猜测迁移）；`NewSQLiteStore` 自动建表。
- **Search 与内存实现同语义**（子串 LIKE 转义、状态过滤、UpdatedAt 降序 + ID tiebreak、Limit 硬上限）；**大小写折叠统一仅 ASCII**（SQLite `lower()` 与 Go 侧 `asciiFold` 同口径——重音等非 ASCII 大写不折叠，两实现可替换不惊异）；**FTS 走实现特有 `SearchFTS(ctx, ns, match, limit)`**（token 前缀 `"t"* AND "c"*` 形式，C3 Assembler 的召回入口，类型断言使用，不在 §7.1 接口面）。
- **并发取舍**：`MaxOpenConns(1)` + `busy_timeout` + `_txlock=immediate`（BeginTx 即 BEGIN IMMEDIATE，消除 deferred 升级窗口）——SQLite 写锁下最稳的正确性，单机吞吐不敏感；WAL 等调优由宿主 DSN 自定义。
- **Supersede / Revoke 均为事务写**：item 写入与 audit 插入同事务，中间崩溃不留半态；新建路径的 INSERT PK 冲突映射为 `ErrItemExists`（并发新建 TOCTOU 的错误收口）。
- **Supersede 两写在 `BEGIN IMMEDIATE` 事务内**——替代链不断。
