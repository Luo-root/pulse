package demoapp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		kind string
	}{
		{"hello", "text"},
		{"/exit", "exit"},
		{"/image https://example.com/a.png", "image"},
		{"/file D:\\a.pdf application/pdf", "file"},
		{"/send", "send"},
		{"\uFEFF/exit", "exit"},
				{"", "empty"},
		{"/nope", "unknown"},
	}
	for _, c := range cases {
		got := ParseCommand(c.in)
		if got.Kind != c.kind {
			t.Fatalf("%q: kind=%s want %s", c.in, got.Kind, c.kind)
		}
	}
}

func TestLoopMultiTurnHistory(t *testing.T) {
	var history []*llm.Message
	in := strings.NewReader("第一轮\n/history\n第二轮\n/history\n/exit\n")
	var out bytes.Buffer
	err := Loop(in, &out, func(msg *llm.Message) ([]*llm.Message, error) {
		reply := llm.AssistantText("收到：" + msg.Text())
		history = append(history, msg, reply)
		return []*llm.Message{reply}, nil
	}, func() int { return len(history) })
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("history=%d want 4", len(history))
	}
	s := out.String()
	if !strings.Contains(s, "history=2") || !strings.Contains(s, "history=4") {
		t.Fatalf("output missing history lines: %s", s)
	}
}
