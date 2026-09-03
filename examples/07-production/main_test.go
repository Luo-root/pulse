package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

func TestThreeSourcesDefinitionsAndRound(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()

	demo, err := setupDemo(host)
	if err != nil {
		t.Fatal(err)
	}
	defer demo.Close()

	names := map[string]bool{}
	for _, d := range demo.Reg.AsToolSet().Definitions() {
		names[d.Name] = true
	}
	for _, want := range []string{"lookup", "delete_file", "mcp_echo", "list_skills", "load_skill"} {
		if !names[want] {
			t.Fatalf("missing tool %q in Definitions: %v", want, names)
		}
	}
	for _, ban := range []string{"frontend-design", "pptx"} {
		if names[ban] {
			t.Fatalf("skill name %q must not appear in Definitions", ban)
		}
	}

	model := llm.NewScripted(
		llm.RespToolCalls(
			llm.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"topic":"pulse"}`)},
			llm.ToolCall{ID: "c2", Name: "mcp_echo", Arguments: json.RawMessage(`{"msg":"hi"}`)},
			llm.ToolCall{ID: "c3", Name: "list_skills", Arguments: json.RawMessage(`{}`)},
		),
		llm.RespToolCalls(
			llm.ToolCall{ID: "c4", Name: "load_skill", Arguments: json.RawMessage(`{"name":"frontend-design"}`)},
		),
		llm.Resp("done"),
	)
	agent, err := loop.NewAgent(model,
		loop.WithToolSet(demo.Reg.AsToolSet()),
		loop.WithSystemPrompt(buildSystem(demo.Metas)),
		loop.WithEventScope(host),
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), nil, llm.UserText("demo"))
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]string{}
	isErr := map[string]bool{}
	for _, m := range res.Messages {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == llm.PartToolResult && p.ToolResultValue != nil {
				byID[p.ToolResultValue.ToolCallID] = partText(p.ToolResultValue.Content)
				isErr[p.ToolResultValue.ToolCallID] = p.ToolResultValue.IsError
			}
		}
	}
	if isErr["c1"] || !strings.Contains(byID["c1"], "local.lookup") {
		t.Fatalf("lookup: %q err=%v", byID["c1"], isErr["c1"])
	}
	if isErr["c2"] || !strings.Contains(byID["c2"], "mcp-echo:hi") {
		t.Fatalf("mcp_echo: %q err=%v", byID["c2"], isErr["c2"])
	}
	listOut := byID["c3"]
	if isErr["c3"] {
		t.Fatalf("list_skills err: %q", listOut)
	}
	// 不泄漏绝对路径：Windows 由 "X:\"（含 ":\") 兜底，Unix 由 /home/、/Users/ 兜底。
	// 注意不能用 filepath.VolumeName(Dir)+"\\" 拼盘符前缀——Unix 上 VolumeName 返回空串，
	// 该条件会退化为「输出不含反斜杠字符」，被 JSON 转义序列（如 \"）误触发。
	if strings.Contains(listOut, `"directory"`) || strings.Contains(listOut, `Dir`) ||
		strings.Contains(listOut, ":\\") || strings.Contains(listOut, "/Users/") || strings.Contains(listOut, "/home/") {
		t.Fatalf("list_skills must not leak absolute Dir: %s", listOut)
	}
	if !strings.Contains(listOut, `"name":"frontend-design"`) {
		t.Fatalf("list_skills missing frontend-design: %s", listOut)
	}

	loadOut := byID["c4"]
	if isErr["c4"] {
		t.Fatalf("load_skill err: %q", loadOut)
	}
	var content struct {
		Name      string   `json:"name"`
		Body      string   `json:"body"`
		Directory string   `json:"directory"`
		Location  string   `json:"location"`
		Resources []string `json:"resources"`
	}
	if err := json.Unmarshal([]byte(loadOut), &content); err != nil {
		t.Fatalf("load_skill should return skills.Content JSON: %v raw=%q", err, loadOut)
	}
	if content.Name != "frontend-design" {
		t.Fatalf("load name=%q", content.Name)
	}
	wantDir := ""
	for _, m := range demo.Metas {
		if m.Name == "frontend-design" {
			wantDir = m.Dir
			break
		}
	}
	if wantDir == "" || content.Directory != wantDir {
		t.Fatalf("Directory=%q want %q", content.Directory, wantDir)
	}
	if content.Location != filepath.Join(wantDir, "SKILL.md") {
		t.Fatalf("Location=%q", content.Location)
	}
	if strings.Contains(content.Body, "name: frontend-design\n") || strings.HasPrefix(strings.TrimSpace(content.Body), "---") {
		t.Fatalf("body should not include frontmatter: %q", content.Body)
	}
	if !strings.Contains(content.Body, "distinctive") && !strings.Contains(content.Body, "frontend") {
		t.Fatalf("body missing frontend-design content: %q", content.Body)
	}
}
