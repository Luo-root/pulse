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
	if strings.Contains(listOut, `Dir`) || strings.Contains(listOut, filepath.VolumeName(demo.Metas[0].Dir)+`\`) ||
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
	if !strings.Contains(loadOut, "Skill directory:") {
		t.Fatalf("load_skill must include skill directory: %q", loadOut)
	}
	if !strings.Contains(loadOut, demo.Metas[0].Dir) && !strings.Contains(loadOut, "frontend-design") {
		// directory path for frontend-design should appear
		found := false
		for _, m := range demo.Metas {
			if m.Name == "frontend-design" && strings.Contains(loadOut, m.Dir) {
				found = true
			}
		}
		if !found {
			t.Fatalf("load_skill missing absolute skill dir: %q", loadOut)
		}
	}
	for _, m := range demo.Metas {
		if m.Name == "frontend-design" && !strings.Contains(loadOut, m.Dir) {
			t.Fatalf("load_skill should include Dir %q, got %q", m.Dir, loadOut)
		}
	}
	if strings.Contains(loadOut, "\nname:") || strings.HasPrefix(strings.TrimSpace(loadOut), "---") {
		// body should not re-include YAML frontmatter block as loaded content start;
		// header lines come first, then body without frontmatter.
	}
	if strings.Contains(loadOut, "name: frontend-design\n") {
		t.Fatalf("load_skill body should not include frontmatter name field: %q", loadOut)
	}
	if !strings.Contains(loadOut, "distinctive") && !strings.Contains(loadOut, "frontend") {
		t.Fatalf("load_skill body missing frontend-design content: %q", loadOut)
	}
}
