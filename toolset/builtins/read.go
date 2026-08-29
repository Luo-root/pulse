package builtins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regRead() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "read",
			Description: "Read a text file with optional line offset/limit. Lines are prefixed with 1-based numbers. Use offset to continue after a truncated page.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"File path (relative to workspace Root or absolute under Root)"},
    "offset":{"type":"integer","description":"0-based line offset (default 0)","minimum":0},
    "limit":{"type":"integer","description":"Max lines to return (default from Options)","minimum":1}
  },
  "required":["path"]
}`),
		},
		Fn:   e.read,
		Risk: toolset.RiskReadonly,
	}
}

func (e *env) read(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/read: invalid args: %w", err)
	}
	abs, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return "", err
	}
	if err := confineRead(e.opt.Root, e.opt.ForbidRead, abs); err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("builtins/read: %w", err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("builtins/read: %s is a directory — use ls", abs)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = e.opt.ReadLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("builtins/read: %w", err)
	}
	defer f.Close()

	peek := make([]byte, 8*1024)
	n, _ := io.ReadFull(f, peek)
	peek = peek[:n]
	if bytes.IndexByte(peek, 0) >= 0 {
		return "", fmt.Errorf("builtins/read: binary file (NUL detected): %s", abs)
	}
	src := io.MultiReader(bytes.NewReader(peek), f)

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	lineNo := 0
	byteSize := 0
	hasMore := false
	for sc.Scan() {
		lineNo++
		if lineNo <= p.Offset {
			continue
		}
		text := clipLine(sc.Text(), e.opt.MaxLineRunes)
		proj := byteSize + len(text) + 1
		if len(lines) >= limit || proj > e.opt.MaxReadBytes {
			hasMore = true
			break
		}
		lines = append(lines, text)
		byteSize = proj
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("builtins/read: scan: %w", err)
	}
	if lineNo == 0 {
		e.tracker.mark(abs, time.Now())
		return "(empty file)", nil
	}
	if len(lines) == 0 {
		return fmt.Sprintf("(offset %d past EOF — file has %d lines)", p.Offset, lineNo), nil
	}

	w := len(fmt.Sprint(p.Offset + len(lines)))
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%*d|%s\n", w, p.Offset+i+1, line)
	}
	if hasMore {
		fmt.Fprintf(&b, "\n[truncated: more lines below; pass offset=%d limit=%d to continue]\n",
			p.Offset+len(lines), limit)
	}
	e.tracker.mark(abs, time.Now())
	return b.String(), nil
}
