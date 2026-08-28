# skills

Agent Skills 装载器（Accepted：[`docs/design/skills-v1-design.md`](../docs/design/skills-v1-design.md) I1）。

扫描技能根目录，解析 [agentskills.io](https://agentskills.io/specification) frontmatter，提供 `List` / `Load` / `ReadFile`。  
**不**执行脚本、**不**实现 `loop.ToolSet`、**不**往 `pulse.tools` 注册工具。

## 刻意不做（本包 I1）

| 不做 | 归谁 |
|---|---|
| Discovery 短表写入 system | I2 装配层 |
| `load_skill` 宿主工具 | I3 |
| 解析 `allowed-tools` | 不解析 |
| 独立脚本执行面 | 须映射已注册 Tool |

## 上手

```go
loader, err := skills.Open("/path/to/skill-root")
metas, err := loader.List(ctx)           // 按 name 排序
body, err := loader.Load(ctx, "pdf-processing") // Markdown 正文（已剥 frontmatter）
b, err := loader.ReadFile(ctx, "pdf-processing", "references/FORMS.md")
```

每个 skill 是子目录 + `SKILL.md`；`name` 必须与目录名一致。

**装配语义**：某个子目录有 `SKILL.md` 但 frontmatter 非法 → **整个 `Open` 失败**（尽早暴露）。没有 `SKILL.md` 的子目录会被跳过。`List` 与 `Load` 共用 `Open` 时的扫描快照；磁盘上改了文件要再 `Open` 才生效。`ReadFile` 仍按需读盘，但路径必须落在扫描到的 skill 目录内。

## 示例材料

仓库示例在 [`examples/skills/`](../examples/skills/)：目前保留 `frontend-design`、`pptx` 两套可用规程包（不含 OOXML schema 树）。

## 测试

```bash
go test -race ./skills/...
```
