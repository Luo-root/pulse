# examples/skills

与公开包 `skills/` 错开路径的示例规程包（[agentskills.io](https://agentskills.io/specification) 风格）。

当前保留：

| Skill | 用途 |
|---|---|
| `frontend-design` | 前端界面设计规程（Typography / Color / Motion 等指导） |
| `pptx` | PPTX 创建/编辑/读取规程 + scripts / references |

已移除旧式「伪工具」示例（依赖 `SKILL_ARGS`、私有 `parameters` 执行面，或缺环境无法用）：
`web-researcher`、`code-summarizer`、`data-transformer`、`git-log-analyzer`、`system-info`。

```bash
go test -race ./skills/...
# loader, _ := skills.Open("examples/skills")
```
