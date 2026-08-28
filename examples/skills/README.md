# examples/skills

示例 Skill 材料（原仓库根 `skills/`，已迁出以免与公开包 `skills/` 撞名）。

格式目标对齐 [agentskills.io](https://agentskills.io/specification)。部分历史文件仍含私有 frontmatter 键（`category` / `parameters` / `timeout` 等）——公开装载器会**忽略**这些键，不升格为规范。

```bash
go test -race ./skills/...
# 或手动：
# loader, _ := skills.Open("examples/skills")
```
