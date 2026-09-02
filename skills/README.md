[English](README.md) | [中文](README_zh.md)

# skills

Agent Skills loader (Accepted: [`docs/design/skills-v1-design.md`](../docs/design/skills-v1-design.md) I1).

It scans the skill root directory, parses [agentskills.io](https://agentskills.io/specification) frontmatter, and provides `List` / `Load` / `ReadFile`.  
It does **not** execute scripts, does **not** implement `loop.ToolSet`, and does **not** register tools into `pulse.tools`.

The two disclosure layers align with the [loader guide](https://agentskills.io/client-implementation/adding-skills-support.md):

| Layer | API | What the model gets |
|---|---|---|
| Discovery Catalog | `Catalog(List(...))` | Only `name` + `description` |
| Activation | `Load(name)` → `Content` | `Body` + **`Directory`** + the resources front page; when over the limit, paginate with `ResourcesNext` + `ListResources` |

`Directory` is returned only in the activation result: relative paths such as `scripts/...` inside SKILL.md are rooted there; a generic command-line tool can run the scripts by setting its working directory, so there is **no** need to build a dedicated script-execution tool.

## Deliberately out of scope (this package, I1)

| Not doing | Belongs to |
|---|---|
| Writing the Discovery short table into system | I2 assembly layer |
| The `load_skill` host tool | I3 |
| Parsing `allowed-tools` | Not parsed |
| A standalone script execution surface | Generic command line / registered Tools |

## Getting started

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

Each skill is a subdirectory + `SKILL.md`; `name` must match the directory name.

**Assembly semantics**: if a subdirectory has a `SKILL.md` with invalid frontmatter → **the whole `Open` fails** (exposed early). Subdirectories without a `SKILL.md` are skipped. The bodies of `List` and `Load` share the scan snapshot taken by `Open`; if the disk changed, `Open` again. When `Load` activates, it enumerates resource relative paths: **collect and sort lexicographically first, then slice the front page** (default 64); when over the limit it sets `ResourcesNext`, paginated with `ListResources(name, after, limit)`. `SKILL.md` / `.git` / `node_modules` are skipped, and file contents are not pre-read. `ReadFile` still reads from disk on demand, and the path must fall inside a scanned skill directory.

## Example material

The repo examples live in [`examples/skills/`](../examples/skills/): two usable procedure packages are kept for now, `frontend-design` and `pptx` (without the OOXML schema tree).

## Tests

```bash
go test -race ./skills/...
```
