# skills

Agent Skills 装载器（Accepted：[`docs/design/skills-v1-design.md`](../docs/design/skills-v1-design.md) I1）。

扫描技能根目录，解析 [agentskills.io](https://agentskills.io/specification) frontmatter，提供 `List` / `Load` / `ReadFile`。  
**不**执行脚本、**不**实现 `loop.ToolSet`、**不**往 `pulse.tools` 注册工具。

两层披露对齐 [装载器指南](https://agentskills.io/client-implementation/adding-skills-support.md)：

| 层 | API | 给模型什么 |
|---|---|---|
| Discovery Catalog | `Catalog(List(...))` | 仅 `name` + `description` |
| Activation | `Load(name)` → `Content` | `Body` + **`Directory`** + 资源首页；超限用 `ResourcesNext` + `ListResources` 翻页 |

`Directory` 只在激活结果里返回：SKILL.md 里的 `scripts/...` 等相对路径以此为根；通用命令行工具设 working directory 即可跑脚本，**不必**再造专用 script 执行工具。

## 刻意不做（本包 I1）

| 不做 | 归谁 |
|---|---|
| Discovery 短表写入 system | I2 装配层 |
| `load_skill` 宿主工具 | I3 |
| 解析 `allowed-tools` | 不解析 |
| 独立脚本执行面 | 通用命令行 / 已注册 Tool |

## 上手

```go
loader, err := skills.Open("/path/to/skill-root")
metas, err := loader.List(ctx)                 // 宿主完整 Meta（含 Dir/Location）
catalog := skills.Catalog(metas)               // 短表：name+description
content, err := loader.Load(ctx, "pdf")        // Content{Body, Directory, Resources...}
if content.ResourcesNext != "" {
    page, err := loader.ListResources(ctx, "pdf", content.ResourcesNext, 0)
    _ = page
}
b, err := loader.ReadFile(ctx, "pdf", "references/FORMS.md")
```

每个 skill 是子目录 + `SKILL.md`；`name` 必须与目录名一致。

**装配语义**：某个子目录有 `SKILL.md` 但 frontmatter 非法 → **整个 `Open` 失败**（尽早暴露）。没有 `SKILL.md` 的子目录会被跳过。`List` 与 `Load` 的正文共用 `Open` 扫描快照；磁盘改了要再 `Open`。`Load` 激活时枚举资源相对路径：**先收集并字典序排序，再切首页**（默认 64）；超限设 `ResourcesNext`，用 `ListResources(name, after, limit)` 翻页。跳过 `SKILL.md` / `.git` / `node_modules`，不预读文件内容。`ReadFile` 仍按需读盘，路径必须落在扫描到的 skill 目录内。

## 示例材料

仓库示例在 [`examples/skills/`](../examples/skills/)：目前保留 `frontend-design`、`pptx` 两套可用规程包（不含 OOXML schema 树）。

## 测试

```bash
go test -race ./skills/...
```
