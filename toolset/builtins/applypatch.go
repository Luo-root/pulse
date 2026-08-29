package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regApplyPatch() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "apply_patch",
			Description: "Apply a multi-file patch in V4A text format (*** Begin Patch ... *** End Patch with *** Add File / *** Update File / *** Delete File sections). The whole patch is verified (context anchors, read-before-write, write roots) before anything is written; one failure writes nothing. Update/Delete require a prior read of the target in this process.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "patch":{"type":"string","description":"V4A patch text starting with *** Begin Patch and ending with *** End Patch"}
  },
  "required":["patch"]
}`),
		},
		Fn:        e.applyPatch,
		Risk:      toolset.RiskReadWrite,
		PreviewFn: e.previewApplyPatch,
	}
}

func (e *env) previewApplyPatch(ctx context.Context, args json.RawMessage) (toolset.Preview, error) {
	if err := ctx.Err(); err != nil {
		return toolset.Preview{}, err
	}
	patch, err := parseApplyPatchArgs(args)
	if err != nil {
		return toolset.Preview{}, err
	}
	plans, err := e.planPatch(patch, false)
	if err != nil {
		return toolset.Preview{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s):", len(plans))
	for _, pl := range plans {
		fmt.Fprintf(&b, "\n%s %s (+%d/-%d)", pl.op, pl.abs, pl.added, pl.removed)
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionWrite,
		Subject: fmt.Sprintf("%d file(s)", len(plans)),
		Opaque: &toolset.OpaqueChange{
			Summary:     b.String(),
			ArgsExcerpt: excerpt(patch, 512),
		},
	}, nil
}

type applyPatchArgs struct {
	Patch string `json:"patch"`
}

func parseApplyPatchArgs(args json.RawMessage) (string, error) {
	var p applyPatchArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/apply_patch: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Patch) == "" {
		return "", fmt.Errorf("builtins/apply_patch: patch is required")
	}
	return p.Patch, nil
}

func (e *env) applyPatch(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	patch, err := parseApplyPatchArgs(args)
	if err != nil {
		return "", err
	}
	plans, err := e.planPatch(patch, true)
	if err != nil {
		return "", err
	}
	return e.commitPatch(plans)
}

// patchLine 是 Update 块里的一行：op 为 ' '（context）、'-'（removed）、'+'（added）。
type patchLine struct {
	op   byte
	text string
}

// patchOp 是一个文件级指令。
type patchOp struct {
	kind   string // add|update|delete
	path   string
	added  []string      // add：内容行
	blocks [][]patchLine // update：顺序变更块
}

// parseV4A 严格解析 V4A 文本：Begin/End 成对；只认 Add/Update/Delete 三种文件头。
func parseV4A(patch string) ([]patchOp, error) {
	lines := splitLines(patch)
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || lines[i] != "*** Begin Patch" {
		return nil, fmt.Errorf("builtins/apply_patch: patch must start with a '*** Begin Patch' line")
	}
	i++
	end := -1
	for j := i; j < len(lines); j++ {
		if lines[j] == "*** End Patch" {
			end = j
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("builtins/apply_patch: missing '*** End Patch'")
	}

	var ops []patchOp
	for i < end {
		l := lines[i]
		if l == "" {
			i++
			continue
		}
		if !strings.HasPrefix(l, "*** ") {
			return nil, fmt.Errorf("builtins/apply_patch: line %d: expected a '*** ' directive, got %.40q", i+1, l)
		}
		kind, path, err := parsePatchDirective(l, i+1)
		if err != nil {
			return nil, err
		}
		op := patchOp{kind: kind, path: path}
		i++
		switch kind {
		case "add":
			op.added, i, err = parseAddBody(lines, i, end, path)
		case "update":
			op.blocks, i, err = parseUpdateBody(lines, i, end, path)
		case "delete":
			// Delete 无体；内容行会在下一轮循环里报错。
		}
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("builtins/apply_patch: patch has no file sections")
	}
	return ops, nil
}

func parsePatchDirective(l string, lineNo int) (kind, path string, err error) {
	for _, d := range []struct{ name, kind string }{
		{"*** Add File:", "add"},
		{"*** Update File:", "update"},
		{"*** Delete File:", "delete"},
	} {
		if strings.HasPrefix(l, d.name) {
			path = strings.TrimSpace(strings.TrimPrefix(l, d.name))
			if path == "" {
				return "", "", fmt.Errorf("builtins/apply_patch: line %d: %s needs a path", lineNo, d.name)
			}
			return d.kind, path, nil
		}
	}
	if strings.HasPrefix(l, "*** Move to:") {
		return "", "", fmt.Errorf("builtins/apply_patch: line %d: '*** Move to:' (rename) is not supported", lineNo)
	}
	return "", "", fmt.Errorf("builtins/apply_patch: line %d: unsupported directive %.40q", lineNo, l)
}

func parseAddBody(lines []string, i, end int, path string) (content []string, next int, err error) {
	for i < end {
		l := lines[i]
		if strings.HasPrefix(l, "*** ") {
			break
		}
		if l == "" || !strings.HasPrefix(l, "+") {
			return nil, i, fmt.Errorf("builtins/apply_patch: line %d: Add File %s body must be lines prefixed with '+'", i+1, path)
		}
		content = append(content, strings.TrimPrefix(l, "+"))
		i++
	}
	if len(content) == 0 {
		return nil, i, fmt.Errorf("builtins/apply_patch: Add File %s has no lines", path)
	}
	return content, i, nil
}

func parseUpdateBody(lines []string, i, end int, path string) (blocks [][]patchLine, next int, err error) {
	var cur []patchLine
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = nil
		}
	}
	for i < end {
		l := lines[i]
		if strings.HasPrefix(l, "*** ") {
			if strings.HasPrefix(l, "*** Move to:") {
				return nil, i, fmt.Errorf("builtins/apply_patch: line %d: '*** Move to:' (rename) is not supported", i+1)
			}
			break
		}
		switch {
		case strings.HasPrefix(l, "@@"):
			flush()
		case l == "":
			cur = append(cur, patchLine{op: ' '})
		case strings.HasPrefix(l, " "):
			cur = append(cur, patchLine{op: ' ', text: l[1:]})
		case strings.HasPrefix(l, "-"):
			cur = append(cur, patchLine{op: '-', text: l[1:]})
		case strings.HasPrefix(l, "+"):
			cur = append(cur, patchLine{op: '+', text: l[1:]})
		default:
			return nil, i, fmt.Errorf("builtins/apply_patch: line %d: Update File %s body lines must start with ' ', '-' or '+'", i+1, path)
		}
		i++
	}
	flush()
	if len(blocks) == 0 {
		return nil, i, fmt.Errorf("builtins/apply_patch: Update File %s has no change blocks", path)
	}
	for bi, b := range blocks {
		anchor := false
		for _, pl := range b {
			if pl.op != '+' {
				anchor = true
				break
			}
		}
		if !anchor {
			return nil, i, fmt.Errorf("builtins/apply_patch: Update File %s block %d has no context anchor (need ' ' or '-' lines)", path, bi+1)
		}
	}
	return blocks, i, nil
}

// patchPlan 是一个已验证、待写入的文件变更。
type patchPlan struct {
	abs     string
	op      string // create|modify|delete
	old     string
	next    string
	added   int
	removed int
}

// planPatch 把 patch 解析并验证成写盘计划。checkStale=false 时不查 read-before-write
// （预览口径：卡片照算，执行门槛留给 Execute）。
func (e *env) planPatch(patch string, checkStale bool) ([]patchPlan, error) {
	ops, err := parseV4A(patch)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var plans []patchPlan
	for _, o := range ops {
		abs, err := resolveUnderRoot(e.opt.Root, o.path)
		if err != nil {
			return nil, err
		}
		if err := confineWrite(e.opt.WriteRoots, abs); err != nil {
			return nil, err
		}
		if seen[abs] {
			return nil, fmt.Errorf("builtins/apply_patch: duplicate target file: %s", o.path)
		}
		seen[abs] = true

		switch o.kind {
		case "add":
			if _, err := os.Stat(abs); err == nil {
				return nil, fmt.Errorf("builtins/apply_patch: Add File but it already exists: %s", abs)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("builtins/apply_patch: %w", err)
			}
			content := strings.Join(o.added, "\n")
			if bytes.IndexByte([]byte(content), 0) >= 0 {
				return nil, fmt.Errorf("builtins/apply_patch: Add File %s: binary content (NUL) not supported", o.path)
			}
			plans = append(plans, patchPlan{abs: abs, op: "create", next: content, added: len(o.added)})
		case "update":
			if checkStale {
				if err := e.requireFreshRead(abs); err != nil {
					return nil, err
				}
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return nil, fmt.Errorf("builtins/apply_patch: %w", err)
			}
			if bytes.IndexByte(raw, 0) >= 0 {
				return nil, fmt.Errorf("builtins/apply_patch: Update File %s: binary file (NUL detected)", o.path)
			}
			text, isCRLF := toUnix(string(raw))
			hadTrailingNL := strings.HasSuffix(text, "\n")
			next, added, removed, err := applyBlocks(splitLines(text), o.blocks, o.path)
			if err != nil {
				return nil, err
			}
			out := strings.Join(next, "\n")
			if hadTrailingNL {
				out += "\n"
			}
			if out == text {
				return nil, fmt.Errorf("builtins/apply_patch: Update File %s: patch makes no changes", o.path)
			}
			if bytes.IndexByte([]byte(out), 0) >= 0 {
				return nil, fmt.Errorf("builtins/apply_patch: Update File %s: result contains NUL", o.path)
			}
			if isCRLF {
				out = strings.ReplaceAll(out, "\n", "\r\n")
			}
			plans = append(plans, patchPlan{abs: abs, op: "modify", old: text, next: out, added: added, removed: removed})
		case "delete":
			if checkStale {
				if err := e.requireFreshRead(abs); err != nil {
					return nil, err
				}
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return nil, fmt.Errorf("builtins/apply_patch: %w", err)
			}
			plans = append(plans, patchPlan{abs: abs, op: "delete", old: string(raw), removed: len(splitLines(string(raw)))})
		}
	}
	return plans, nil
}

// applyBlocks 顺序应用变更块：每块以 context+removed 行序列为锚，
// 从上一次消费位置之后找第一次出现，替换后继续（不重叠）。
func applyBlocks(old []string, blocks [][]patchLine, name string) (out []string, added, removed int, err error) {
	out = make([]string, 0, len(old))
	cursor := 0
	for bi, b := range blocks {
		var anchor []string
		for _, pl := range b {
			if pl.op != '+' {
				anchor = append(anchor, pl.text)
			}
		}
		idx := indexSeq(old, anchor, cursor)
		if idx < 0 {
			return nil, 0, 0, fmt.Errorf("builtins/apply_patch: Update File %s block %d: context not found (searched from line %d)", name, bi+1, cursor+1)
		}
		out = append(out, old[cursor:idx]...)
		for _, pl := range b {
			switch pl.op {
			case ' ':
				out = append(out, pl.text)
			case '+':
				out = append(out, pl.text)
				added++
			case '-':
				removed++
			}
		}
		cursor = idx + len(anchor)
	}
	out = append(out, old[cursor:]...)
	return out, added, removed, nil
}

func indexSeq(hay, needle []string, from int) int {
	if len(needle) == 0 {
		return -1
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// commitPatch 落盘已验证的计划。任一写失败立即中止并列出已写入文件
// （verify 阶段已挡住内容级失败；这里只剩 IO 错误，不做自动回滚）。
func (e *env) commitPatch(plans []patchPlan) (string, error) {
	var b strings.Builder
	var done []string
	for _, pl := range plans {
		var err error
		switch pl.op {
		case "create":
			if err = os.MkdirAll(filepath.Dir(pl.abs), 0o755); err == nil {
				err = os.WriteFile(pl.abs, []byte(pl.next), 0o644)
			}
		case "modify":
			err = os.WriteFile(pl.abs, []byte(pl.next), 0o644)
		case "delete":
			err = os.Remove(pl.abs)
		}
		if err != nil {
			return "", fmt.Errorf("builtins/apply_patch: write failed at %s (%s): %w; already written: %v",
				pl.abs, pl.op, err, done)
		}
		done = append(done, pl.abs)
		if pl.op != "delete" {
			e.tracker.mark(pl.abs, time.Now())
		}
		fmt.Fprintf(&b, "%s %s (+%d/-%d)\n", pl.op, pl.abs, pl.added, pl.removed)
	}
	return fmt.Sprintf("applied patch: %d file(s)\n%s", len(plans), b.String()), nil
}

func excerpt(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
