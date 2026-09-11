package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/skills"
	"github.com/Luo-root/pulse/toolset"
)

// SkillTools 把 skills.Loader 适配为一个 ToolSource：注册 list_skills /
// load_skill 两个只读工具（07 课验证过的渐进披露形态——短表在 list，
// 正文与资源清单按需 load）。Skill 本身是规程包不是 Tool；这两个工具是
// 「读取规程」的工具面，不把 Skill 升格为可执行件。
//
// 用法：Options.Tools = append(o.Tools, host.SkillTools(loader))。
func SkillTools(loader skills.Loader) ToolSource {
	return func(c *kernel.Context, reg *toolset.Registry) error {
		if _, err := reg.Register(c, toolset.Registration{
			Def: llm.ToolDef{
				Name:        "list_skills",
				Description: "列出已发现 Skills（名称 + 描述目录；Skill 是规程包，不是工具）",
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
			Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
				metas, err := loader.List(ctx)
				if err != nil {
					return "", err
				}
				// Catalog 只给 name + description，不把本机路径倒进模型上下文。
				b, err := json.Marshal(skills.Catalog(metas))
				if err != nil {
					return "", err
				}
				return string(b), nil
			},
			Source: "skills.list",
			Risk:   toolset.RiskReadonly,
		}); err != nil {
			return fmt.Errorf("host: register list_skills: %w", err)
		}
		if _, err := reg.Register(c, toolset.Registration{
			Def: llm.ToolDef{
				Name:        "load_skill",
				Description: "加载 Skill 激活结果（正文 + 目录 + 资源清单；相对路径脚本以 Directory 为根）",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			},
			Fn: func(ctx context.Context, args json.RawMessage) (string, error) {
				var p struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return "", err
				}
				content, err := loader.Load(ctx, p.Name)
				if err != nil {
					return "", err
				}
				// 直接消费 skills.Content；目录给通用命令行工具拼相对路径，
				// 不再造专用 script 工具。
				b, err := json.Marshal(content)
				if err != nil {
					return "", err
				}
				return string(b), nil
			},
			Source: "skills.load",
			Risk:   toolset.RiskReadonly,
		}); err != nil {
			return fmt.Errorf("host: register load_skill: %w", err)
		}
		return nil
	}
}
