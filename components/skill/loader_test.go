package skill_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/skill"
	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// 辅助函数
// ============================================================================

func setup(t *testing.T) (*skill.SkillRegistry, *tools.ToolRegistry, *skill.SkillLoader) {
	t.Helper()
	sr := skill.NewSkillRegistry()
	tr := tools.NewToolRegistry()
	loader := skill.NewSkillLoader(sr, tr)
	return sr, tr, loader
}

func writeSkillDir(t *testing.T, name, md string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	os.Mkdir(dir, 0755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0644)
	return filepath.Dir(dir) // 返回父目录（LoadFromDir 扫描的目录）
}

// ============================================================================
// ParseSkillMarkdown 函数测试
// ============================================================================

func TestParse_BasicInstruction(t *testing.T) {
	md := `---
name: search-doc
description: 搜索本地文档
license: MIT
category: file
timeout: 10
parameters:
  type: object
  properties:
    keyword:
      type: string
  required: [keyword]
---

# Search Doc

使用方法：输入关键词，返回结果。

` + "```go" + `
fmt.Println("这段代码不会被执行，只是文档示例")
` + "```"

	s, err := skill.ParseSkillMarkdown(md)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	if s.Name != "search-doc" {
		t.Errorf("name mismatch: got %s, want search-doc", s.Name)
	}
	if s.Description != "搜索本地文档" {
		t.Errorf("description mismatch: got %s, want 搜索本地文档", s.Description)
	}
	if s.License != "MIT" {
		t.Errorf("license mismatch: got %s, want MIT", s.License)
	}
	if s.Category != "file" {
		t.Errorf("category mismatch: got %s, want file", s.Category)
	}
	if !strings.Contains(s.Body, "fmt.Println") {
		t.Error("body should contain code block as text")
	}
	if !strings.Contains(s.Body, "使用方法") {
		t.Error("body should contain instruction text")
	}
}

func TestParse_CodeBlocksPreservedAsText(t *testing.T) {
	md := `---
name: multi-code
description: 多代码块
---
# 示例

` + "```python" + `
print("hello")
` + "```" + `

` + "```go" + `
fmt.Println("hello")
` + "```" + `

` + "```bash" + `
echo hello
` + "```" + `

三种语言的代码块都作为正文保留。`

	s, err := skill.ParseSkillMarkdown(md)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	for _, want := range []string{`print("hello")`, `fmt.Println("hello")`, `echo hello`, "三种语言"} {
		if !strings.Contains(s.Body, want) {
			t.Errorf("body missing content: %q", want)
		}
	}
}

func TestParse_MissingName(t *testing.T) {
	_, err := skill.ParseSkillMarkdown("---\ndescription: x\n---\nbody")
	if err == nil {
		t.Fatal("expected error when name is missing")
	}
}

func TestParse_MissingDescription(t *testing.T) {
	_, err := skill.ParseSkillMarkdown("---\nname: x\n---\nbody")
	if err == nil {
		t.Fatal("expected error when description is missing")
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	_, err := skill.ParseSkillMarkdown("# just a doc")
	if err == nil {
		t.Fatal("expected error when frontmatter is missing")
	}
}

func TestParse_EmptyBody(t *testing.T) {
	s, err := skill.ParseSkillMarkdown("---\nname: empty\ndescription: test\n---\n")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if s.Body != "" {
		t.Errorf("expected empty body, got %q", s.Body)
	}
}

func TestParse_DefaultParameters(t *testing.T) {
	s, _ := skill.ParseSkillMarkdown("---\nname: x\ndescription: y\n---\nbody")
	if s.Parameters["type"] != "object" {
		t.Errorf("expected default params type=object, got %v", s.Parameters)
	}
}

func TestParse_AllowedTools(t *testing.T) {
	s, _ := skill.ParseSkillMarkdown("---\nname: x\ndescription: y\nallowed-tools: web_search file_read\n---\nbody")
	if len(s.AllowedTools) != 2 {
		t.Fatalf("expected 2 allowed tools, got %d", len(s.AllowedTools))
	}
}

func TestParse_EnvVars(t *testing.T) {
	s, _ := skill.ParseSkillMarkdown("---\nname: x\ndescription: y\nenv_vars: API_KEY SECRET\n---\nbody")
	if len(s.EnvVars) != 2 || s.EnvVars[0] != "API_KEY" {
		t.Errorf("env vars mismatch: got %v", s.EnvVars)
	}
}

func TestParse_TagsAndMetadata(t *testing.T) {
	md := `---
name: x
description: y
category: test
tags:
  - a
  - b
metadata:
  author: me
  timeout: 15
---
body`

	s, _ := skill.ParseSkillMarkdown(md)
	if s.Category != "test" {
		t.Errorf("category mismatch: got %s, want test", s.Category)
	}
	if len(s.Tags) != 2 {
		t.Errorf("tags count mismatch: got %v", s.Tags)
	}
	if s.Metadata["author"] != "me" {
		t.Errorf("metadata author mismatch: got %v", s.Metadata)
	}
}

func TestParse_EscapedQuotes(t *testing.T) {
	md := `---
name: x
description: "Trigger when user mentions \"deck\", \"slides\""
---
body`

	s, err := skill.ParseSkillMarkdown(md)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !strings.Contains(s.Description, "deck") {
		t.Errorf("description mismatch: got %s", s.Description)
	}
}

func TestParse_NameTooLong(t *testing.T) {
	longName := strings.Repeat("a", 65)
	md := "---\nname: " + longName + "\ndescription: test\n---\nbody"
	_, err := skill.ParseSkillMarkdown(md)
	if err == nil {
		t.Fatal("expected error for name longer than 64 characters")
	}
}

// ============================================================================
// 技能注册与执行测试
// ============================================================================

func TestRegister_Execute(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: greet
description: 打招呼
parameters:
  type: object
  properties:
    name:
      type: string
  required: [name]
---

输入用户名，返回个性化问候。`

	if err := loader.LoadFromString(md); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	tool, ok := tr.Get("greet")
	if !ok {
		t.Fatal("skill not registered in tool registry")
	}
	if tool.Metadata.Name != "greet" {
		t.Errorf("tool name mismatch: got %s, want greet", tool.Metadata.Name)
	}

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name:      "greet",
			Arguments: `{"name":"World"}`,
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "个性化问候") {
		t.Errorf("content mismatch: got %s", res.Content)
	}
}

func TestRegister_InstructionReturnsBody(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: guide
description: 操作指南
---
# 步骤

1. 做这个
2. 做那个`

	loader.LoadFromString(md)

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name: "guide",
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "做这个") {
		t.Errorf("content mismatch: got %s", res.Content)
	}
}

func TestRegister_EmptyArgs(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: info
description: 信息
---
some info here`

	loader.LoadFromString(md)

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name:      "info",
			Arguments: `{}`,
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "some info") {
		t.Errorf("content mismatch: got %s", res.Content)
	}
}

// ============================================================================
// 技能卸载测试
// ============================================================================

func TestUnload(t *testing.T) {
	sr, tr, loader := setup(t)

	md := "---\nname: temp\ndescription: temp skill\n---\nbody"
	loader.LoadFromString(md)

	if _, ok := sr.Get("temp"); !ok {
		t.Fatal("skill not in skill registry")
	}
	if _, ok := tr.Get("temp"); !ok {
		t.Fatal("skill not in tool registry")
	}

	loader.Unload("temp")

	if _, ok := sr.Get("temp"); ok {
		t.Fatal("skill still exists in skill registry after unload")
	}
	if _, ok := tr.Get("temp"); ok {
		t.Fatal("skill still exists in tool registry after unload")
	}
}

func TestUnload_Nonexistent(t *testing.T) {
	_, _, loader := setup(t)
	// 卸载不存在的技能不应panic
	_ = loader.Unload("nope")
}

// ============================================================================
// 从目录加载技能测试
// ============================================================================

func TestLoadFromDir_Single(t *testing.T) {
	parent := writeSkillDir(t, "echo", `---
name: echo
description: 回显
---
返回输入内容`)

	_, tr, loader := setup(t)
	if err := loader.LoadFromDir(parent); err != nil {
		t.Fatalf("load from dir failed: %v", err)
	}

	tool, ok := tr.Get("echo")
	if !ok {
		t.Fatal("skill not loaded from directory")
	}
	if tool.Metadata.Description != "回显" {
		t.Errorf("description mismatch: got %s, want 回显", tool.Metadata.Description)
	}
}

func TestLoadFromDir_Multiple(t *testing.T) {
	tmpDir := t.TempDir()
	testCases := []struct {
		name string
		desc string
		body string
	}{
		{name: "skill-a", desc: "A", body: "内容 A"},
		{name: "skill-b", desc: "B", body: "内容 B"},
		{name: "skill-c", desc: "C", body: "内容 C"},
	}

	for _, tc := range testCases {
		dir := filepath.Join(tmpDir, tc.name)
		os.Mkdir(dir, 0755)
		md := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", tc.name, tc.desc, tc.body)
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0644)
	}

	sr, _, loader := setup(t)
	if err := loader.LoadFromDir(tmpDir); err != nil {
		t.Fatalf("load from dir failed: %v", err)
	}

	names := sr.Names()
	if len(names) != 3 {
		t.Fatalf("expected 3 skills, got %d: %v", len(names), names)
	}
}

func TestLoadFromDir_SkipFiles(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# not a skill"), 0644)

	_, _, loader := setup(t)
	if err := loader.LoadFromDir(tmpDir); err != nil {
		t.Fatalf("load from dir failed: %v", err)
	}
}

func TestLoadFromDir_Nonexistent(t *testing.T) {
	_, _, loader := setup(t)
	if err := loader.LoadFromDir("/no/such/dir/12345"); err == nil {
		t.Fatal("expected error when loading from nonexistent directory")
	}
}

func TestLoadFromDir_InvalidSkill(t *testing.T) {
	tmpDir := t.TempDir()
	dir := filepath.Join(tmpDir, "bad")
	os.Mkdir(dir, 0755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: no name\n---\n"), 0644)

	_, _, loader := setup(t)
	err := loader.LoadFromDir(tmpDir)
	if err == nil {
		t.Fatal("expected error for invalid skill file")
	}
}

// ============================================================================
// 元数据传播测试
// ============================================================================

func TestMetadata_Propagation(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: meta
description: 元数据测试
category: testing
tags:
  - test
  - meta
timeout: 15
parameters:
  type: object
  properties:
    input:
      type: string
---
body`

	loader.LoadFromString(md)

	tool, ok := tr.Get("meta")
	if !ok {
		t.Fatal("skill not registered")
	}
	if tool.Metadata.Category != "testing" {
		t.Errorf("category mismatch: got %s, want testing", tool.Metadata.Category)
	}

	hasTest, hasMeta := false, false
	for _, tag := range tool.Metadata.Tags {
		if tag == "test" {
			hasTest = true
		}
		if tag == "meta" {
			hasMeta = true
		}
	}
	if !hasTest || !hasMeta {
		t.Errorf("tags mismatch: got %v", tool.Metadata.Tags)
	}
}

// ============================================================================
// 允许工具钩子测试
// ============================================================================

func TestAllowedTools_Execution(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: restricted
description: 受限
allowed-tools: web_search get_work_dir
---
body`

	loader.LoadFromString(md)

	// 自身执行应成功
	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name: "restricted",
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}
}

func TestAllowedTools_UnloadCleansHooks(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: temp-restricted
description: 临时
allowed-tools: web_search
---
body`

	loader.LoadFromString(md)

	if _, ok := tr.Get("temp-restricted"); !ok {
		t.Fatal("skill not registered")
	}

	loader.Unload("temp-restricted")

	if _, ok := tr.Get("temp-restricted"); ok {
		t.Fatal("skill still registered after unload")
	}
}

// ============================================================================
// 环境变量测试
// ============================================================================

func TestEnvVars_Missing(t *testing.T) {
	_, tr, loader := setup(t)

	md := `---
name: env-check
description: 需要环境变量
env_vars: PULSE_TEST_MUST_NOT_EXIST_12345
---
body`

	loader.LoadFromString(md)

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name:      "env-check",
			Arguments: `{}`,
		},
	})
	if !res.IsError {
		t.Fatal("expected error when required environment variables are missing")
	}
	if !strings.Contains(res.Content, "environment variables") {
		t.Errorf("error message mismatch: got %s", res.Content)
	}
}

func TestEnvVars_Present(t *testing.T) {
	os.Setenv("PULSE_TEST_OK_VAR", "hello")
	defer os.Unsetenv("PULSE_TEST_OK_VAR")

	_, tr, loader := setup(t)

	md := `---
name: env-ok
description: 有环境变量
env_vars: PULSE_TEST_OK_VAR
---
body`

	loader.LoadFromString(md)

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name:      "env-ok",
			Arguments: `{}`,
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}
}

func TestEnvVars_None(t *testing.T) {
	_, tr, loader := setup(t)

	md := "---\nname: no-env\ndescription: no env\n---\nbody"
	loader.LoadFromString(md)

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name:      "no-env",
			Arguments: `{}`,
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}
}

// ============================================================================
// Skill.IsValid 方法测试
// ============================================================================

func TestIsValid_NameRequired(t *testing.T) {
	s := skill.Skill{Description: "test"}
	if err := s.IsValid(); err == nil {
		t.Fatal("expected error when name is missing")
	}
}

func TestIsValid_DescriptionRequired(t *testing.T) {
	s := skill.Skill{Name: "test"}
	if err := s.IsValid(); err == nil {
		t.Fatal("expected error when description is missing")
	}
}

func TestIsValid_NameTooLong(t *testing.T) {
	s := skill.Skill{Name: strings.Repeat("a", 65), Description: "test"}
	if err := s.IsValid(); err == nil {
		t.Fatal("expected error when name is too long")
	}
}

func TestIsValid_DescriptionTooLong(t *testing.T) {
	s := skill.Skill{Name: "test", Description: strings.Repeat("a", 1025)}
	if err := s.IsValid(); err == nil {
		t.Fatal("expected error when description is too long")
	}
}

func TestIsValid_OK(t *testing.T) {
	s := skill.Skill{Name: "test", Description: "test desc"}
	if err := s.IsValid(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestToToolMetadata(t *testing.T) {
	s := skill.Skill{
		Name:        "test",
		Description: "desc",
		Category:    "cat",
		Tags:        []string{"a", "b"},
		Timeout:     10,
	}

	meta := s.ToToolMetadata()
	if meta.Name != "test" {
		t.Errorf("name mismatch: got %s, want test", meta.Name)
	}
	if meta.Category != "cat" {
		t.Errorf("category mismatch: got %s, want cat", meta.Category)
	}
	if len(meta.Tags) != 2 {
		t.Errorf("tags count mismatch: got %v", meta.Tags)
	}
}

func TestToToolMetadata_DefaultTimeout(t *testing.T) {
	s := skill.Skill{Name: "x", Description: "y"}
	meta := s.ToToolMetadata()
	if meta.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", meta.Timeout)
	}
}

// ============================================================================
// 技能注册表测试
// ============================================================================

func TestRegistry_Basic(t *testing.T) {
	sr := skill.NewSkillRegistry()

	s := &skill.Skill{Name: "test", Description: "test"}
	sr.Register(s)

	got, ok := sr.Get("test")
	if !ok || got.Name != "test" {
		t.Fatal("get skill failed")
	}

	names := sr.Names()
	if len(names) != 1 || names[0] != "test" {
		t.Errorf("names mismatch: got %v", names)
	}

	list := sr.List()
	if len(list) != 1 {
		t.Errorf("list count mismatch: got %d", len(list))
	}

	sr.Remove("test")
	if _, ok := sr.Get("test"); ok {
		t.Fatal("skill still exists after remove")
	}
}

func TestRegistry_Overwrite(t *testing.T) {
	sr := skill.NewSkillRegistry()

	sr.Register(&skill.Skill{Name: "x", Description: "v1"})
	sr.Register(&skill.Skill{Name: "x", Description: "v2"})

	got, _ := sr.Get("x")
	if got.Description != "v2" {
		t.Errorf("expected v2, got %s", got.Description)
	}
}

func TestRegistry_GetNonexistent(t *testing.T) {
	sr := skill.NewSkillRegistry()
	if _, ok := sr.Get("nope"); ok {
		t.Fatal("should not return nonexistent skill")
	}
}

func TestRegistry_NamesSorted(t *testing.T) {
	sr := skill.NewSkillRegistry()
	sr.Register(&skill.Skill{Name: "c", Description: "c"})
	sr.Register(&skill.Skill{Name: "a", Description: "a"})
	sr.Register(&skill.Skill{Name: "b", Description: "b"})

	names := sr.Names()
	if names[0] != "a" || names[1] != "b" || names[2] != "c" {
		t.Errorf("names not sorted: got %v", names)
	}
}

// ============================================================================
// 集成测试
// ============================================================================

func findSkillsDir(t *testing.T) string {
	t.Helper()
	candidates := []string{"../../skills", "../../../skills", "skills"}
	wd, _ := os.Getwd()

	for _, rel := range candidates {
		dir := filepath.Join(wd, rel)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.IsDir() {
					return dir
				}
			}
		}
	}

	// 尝试从 go.mod 推断项目根目录
	modDir := findModuleRoot()
	if modDir != "" {
		dir := filepath.Join(modDir, "skills")
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}

	t.Skip("skills directory not found, skipping integration test")
	return ""
}

func findModuleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func TestIntegration_LoadFromDir(t *testing.T) {
	sr, tr, loader := setup(t)
	skillsDir := findSkillsDir(t)

	if err := loader.LoadFromDir(skillsDir); err != nil {
		t.Fatalf("load skills from dir failed: %v", err)
	}

	names := sr.Names()
	t.Logf("loaded %d skills: %v", len(names), names)

	for _, name := range names {
		if _, ok := sr.Get(name); !ok {
			t.Errorf("%s not found in skill registry", name)
		}
		if _, ok := tr.Get(name); !ok {
			t.Errorf("%s not found in tool registry", name)
		}
	}
}

func TestIntegration_AllSkillsReturnContent(t *testing.T) {
	_, tr, loader := setup(t)
	skillsDir := findSkillsDir(t)

	loader.LoadFromDir(skillsDir)

	for _, tool := range tr.GetEnabledTools() {
		res := tr.Execute(context.Background(), schema.ToolCall{
			ID: "call_" + tool.Name,
			Function: schema.FunctionCall{
				Name: tool.Name,
			},
		})

		if res.IsError {
			if strings.Contains(res.Content, "environment variables") {
				t.Logf("%s: requires environment variables (skipped)", tool.Name)
				continue
			}
			t.Errorf("%s execute failed: %s", tool.Name, res.Content)
		} else if res.Content == "" {
			t.Errorf("%s returned empty content", tool.Name)
		} else {
			t.Logf("%s: returned %d characters", tool.Name, len(res.Content))
		}
	}
}

func TestIntegration_FrontendDesign(t *testing.T) {
	_, tr, loader := setup(t)
	skillsDir := findSkillsDir(t)

	loader.LoadFromDir(skillsDir)

	if _, ok := tr.Get("frontend-design"); !ok {
		t.Skip("frontend-design skill not found, skipping test")
	}

	res := tr.Execute(context.Background(), schema.ToolCall{
		ID: "c1",
		Function: schema.FunctionCall{
			Name: "frontend-design",
		},
	})
	if res.IsError {
		t.Fatalf("execute failed: %s", res.Content)
	}

	for _, want := range []string{"Design Thinking", "Typography"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing expected content: %q", want)
		}
	}
}
