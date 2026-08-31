package textsplit

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Chunk 是一个分块：Text 恒等于原文的 [Start:End] 字节切片。
type Chunk struct {
	// Text 是块内容（原文字节切片，无拷贝语义成本）。
	Text string
	// Start 是块起始字节 offset（含）。
	Start int
	// End 是块结束字节 offset（不含）。
	End int
}

// Options 是切分参数。
type Options struct {
	// MaxLen 是单块尺寸上限（按 Size 度量），必须 > 0。
	MaxLen int
	// Overlap 是相邻块的重叠预算（按 Size 度量），0 = 不重叠；必须
	// < MaxLen。重叠区对齐分隔符边界（不从词中间开始）；无可用边界的
	// 段落退化为不重叠。
	Overlap int
	// Size 度量文本尺寸（nil = rune 计数）。假定对前缀单调不减
	//（rune/token 计数天然满足）；精确 token 预算由宿主换自己的度量。
	Size func(string) int
}

// 包内哨兵错误。调用方用 errors.Is 判别。
var (
	// ErrMaxLen：MaxLen 必须为正。
	ErrMaxLen = errors.New("textsplit: max len must be positive")
	// ErrOverlap：Overlap 必须 < MaxLen。
	ErrOverlap = errors.New("textsplit: overlap must be less than max len")
)

// 分隔符优先级：越小越高。段落 > 句读 > 空白 > rune 硬切。
const (
	prioParagraph  = 0
	prioSentence   = 1
	prioWhitespace = 2
)

// boundary 是一个候选切点：pos 是切分字节 offset（chunk =
// text[start:pos]，分隔符保留在左侧块尾），prio 越小优先级越高。
type boundary struct {
	pos  int
	prio int
}

// isSentencePunct 报告 r 是否句读（句末标点）。v1 启发式：'.' 会切分
// 小数（3.14）——已知取舍，注释声明。
func isSentencePunct(r rune) bool {
	switch r {
	case '。', '．', '！', '？', '…', '.', '!', '?':
		return true
	}
	return false
}

// scanBoundaries 一次线性扫描收集全文候选切点（段落/句读/空白），
// pos 递增。
func scanBoundaries(text string) []boundary {
	var out []boundary
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == '\n' {
			// 段落：连续换行（2 个及以上）结束处。
			j := i
			for j < len(text) && text[j] == '\n' {
				j++
			}
			if j-i >= 2 {
				out = append(out, boundary{pos: j, prio: prioParagraph})
			}
			i = j
			continue
		}
		switch {
		case isSentencePunct(r):
			out = append(out, boundary{pos: i + size, prio: prioSentence})
		case unicode.IsSpace(r):
			out = append(out, boundary{pos: i + size, prio: prioWhitespace})
		}
		i += size
	}
	return out
}

// runeBounds 返回 text 的全部 rune 边界字节 offset（升序，含 0 与
// len(text)）——硬切只落在这些位置，rune 安全由此保证。
func runeBounds(text string) []int {
	bs := make([]int, 0, utf8.RuneCountInString(text)+1)
	for i := range text {
		bs = append(bs, i)
	}
	return append(bs, len(text))
}

// maxPrefixLen 在 rune 边界中找最大的 L > start 使
// Size(text[start:L]) <= maxLen（Size 对前缀单调不减，二分成立）。
// 首个 rune 即超预算时取 start 后第一个 rune 边界（进度保证）。
func maxPrefixLen(text string, rb []int, start int, size func(string) int, maxLen int) int {
	lo, hi := 0, len(rb)-1
	res := -1
	for lo <= hi {
		mid := (lo + hi) / 2
		if rb[mid] <= start {
			lo = mid + 1
			continue
		}
		if size(text[start:rb[mid]]) <= maxLen {
			res = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if res >= 0 {
		return rb[res]
	}
	for _, b := range rb {
		if b > start {
			return b
		}
	}
	return len(text)
}

// bestCut 在 (start, limit] 内按分隔符优先级取最大切点：先试最高
// 优先级类（取该类内 ≤ limit 的最大 pos），该类无候选再降级；全部
// 无候选则硬切 limit（rune 边界）。
func bestCut(bounds []boundary, start, limit int) int {
	for prio := prioParagraph; prio <= prioWhitespace; prio++ {
		best := -1
		for _, b := range bounds {
			if b.prio == prio && b.pos > best && b.pos <= limit && b.pos > start {
				best = b.pos
			}
		}
		if best > start {
			return best
		}
	}
	return limit
}

// overlapStart 在 (start, cut] 内找下一块起点：重叠区 text[s:cut] 按
// Size ≤ Overlap 取最大（对齐分隔符边界）；无可用边界则 s = cut
// （空重叠——该步退化为不重叠）。进度由调用方兜底（仅采用 s > start）。
func overlapStart(bounds []boundary, text string, start, cut int, size func(string) int, overlap int) int {
	best := cut
	bestSize := 0
	for _, b := range bounds {
		if b.pos <= start || b.pos > cut {
			continue
		}
		sz := size(text[b.pos:cut])
		if sz <= overlap && sz >= bestSize {
			best = b.pos
			bestSize = sz
		}
	}
	return best
}

// Split 按预算把 text 切成 Chunk 列表（见包文档口径）。
func Split(text string, opts Options) ([]Chunk, error) {
	if opts.MaxLen <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrMaxLen, opts.MaxLen)
	}
	if opts.Overlap >= opts.MaxLen {
		return nil, fmt.Errorf("%w: overlap %d >= max len %d", ErrOverlap, opts.Overlap, opts.MaxLen)
	}
	if opts.Size == nil {
		opts.Size = func(s string) int { return utf8.RuneCountInString(s) }
	}
	if text == "" {
		return nil, nil
	}
	rb := runeBounds(text)
	bounds := scanBoundaries(text)
	var chunks []Chunk
	start := 0
	for start < len(text) {
		if opts.Size(text[start:]) <= opts.MaxLen {
			chunks = append(chunks, Chunk{Text: text[start:], Start: start, End: len(text)})
			break
		}
		limit := maxPrefixLen(text, rb, start, opts.Size, opts.MaxLen)
		cut := bestCut(bounds, start, limit)
		if cut <= start { // 防御：bestCut 已保证 > start，这里兜底进度
			cut = limit
		}
		chunks = append(chunks, Chunk{Text: text[start:cut], Start: start, End: cut})
		next := cut
		if opts.Overlap > 0 {
			if s := overlapStart(bounds, text, start, cut, opts.Size, opts.Overlap); s > start {
				next = s
			}
		}
		if next <= start { // 进度保证（不死循环）
			next = cut
		}
		start = next
	}
	return chunks, nil
}
