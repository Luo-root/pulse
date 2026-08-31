package textsplit

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func runes(s string) int { return utf8.RuneCountInString(s) }

// checkRoundtrip 断言全部块满足字节 offset 回溯等式与 rune 安全。
func checkRoundtrip(t *testing.T, text string, chunks []Chunk) {
	t.Helper()
	for i, c := range chunks {
		if text[c.Start:c.End] != c.Text {
			t.Fatalf("chunk[%d] text[c.Start:c.End] != c.Text（字节 offset 回溯破坏）", i)
		}
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk[%d] not valid utf8（切点落在 rune 中间）", i)
		}
	}
}

// TestShortAndEmpty：预算内单块；空文本返回空切片。
func TestShortAndEmpty(t *testing.T) {
	text := "hello 世界"
	chunks, err := Split(text, Options{MaxLen: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].Text != text || chunks[0].Start != 0 || chunks[0].End != len(text) {
		t.Fatalf("chunks = %+v, want single identity chunk", chunks)
	}
	checkRoundtrip(t, text, chunks)

	chunks, err = Split("", Options{MaxLen: 10})
	if err != nil || chunks != nil {
		t.Fatalf("empty text = %v, %v; want nil, nil", chunks, err)
	}
}

// TestParagraphPriority：预算内存在段落边界 → 优先段落切（即便更晚
// 还有句读边界在预算内）。
func TestParagraphPriority(t *testing.T) {
	text := "AAA\n\nBBB。CCC。DDD。EEE" // 20 runes；段落边界 byte 5，句读 byte 11/17/23
	chunks, err := Split(text, Options{MaxLen: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].Text != "AAA\n\n" {
		t.Fatalf("chunk[0] = %q, want paragraph cut（分隔符保留在左侧块尾）", chunks[0].Text)
	}
	if chunks[1].Text != text[5:] {
		t.Fatalf("chunk[1] = %q", chunks[1].Text)
	}
	checkRoundtrip(t, text, chunks)
}

// TestSentencePriority：无段落 → 句读边界优先于空白边界。
func TestSentencePriority(t *testing.T) {
	text := "AAA BBB。CCC DDD。EEE" // 18 runes；句读 byte 10/20，空白 byte 4/14
	chunks, err := Split(text, Options{MaxLen: 15})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Text != "AAA BBB。" {
		t.Fatalf("chunks = %+v, want cut after sentence punct", chunks)
	}
	if chunks[1].Text != text[10:] {
		t.Fatalf("chunk[1] = %q", chunks[1].Text)
	}
	checkRoundtrip(t, text, chunks)
}

// TestWhitespaceFallback：无段落无句读 → 空白边界；无重叠时块拼接
// 还原原文。
func TestWhitespaceFallback(t *testing.T) {
	text := "word word word word word"
	chunks, err := Split(text, Options{MaxLen: 8})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for i, c := range chunks {
		if runes(c.Text) > 8 {
			t.Fatalf("chunk[%d] = %d runes, want <= 8", i, runes(c.Text))
		}
		joined += c.Text
	}
	if joined != text {
		t.Fatal("chunks must partition the text（无重叠时拼接还原）")
	}
	checkRoundtrip(t, text, chunks)
}

// TestHardCutRuneBoundary：全无分隔符 → rune 硬切，切点落在 rune
// 边界，预算被尊重。
func TestHardCutRuneBoundary(t *testing.T) {
	text := strings.Repeat("啊", 25)
	chunks, err := Split(text, Options{MaxLen: 10})
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{10, 10, 5}
	if len(chunks) != len(sizes) {
		t.Fatalf("chunks = %d, want %d", len(chunks), len(sizes))
	}
	for i, c := range chunks {
		if runes(c.Text) != sizes[i] {
			t.Fatalf("chunk[%d] = %d runes, want %d", i, runes(c.Text), sizes[i])
		}
	}
	checkRoundtrip(t, text, chunks)
}

// TestByteOffsetRoundtripMultibyte：多字节文本下字节 offset 回溯等式
// 逐块成立（验收钉：text[Start:End] == Chunk.Text 是字节语义）。
func TestByteOffsetRoundtripMultibyte(t *testing.T) {
	text := "第一段落。\n\n第二段有 emoji 🎈 和 English。第三段尾巴"
	chunks, err := Split(text, Options{MaxLen: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want multiple", len(chunks))
	}
	checkRoundtrip(t, text, chunks)
}

// TestOverlap：滑窗重叠——重叠区 Size ≤ Overlap 且对齐边界；块序列
// 覆盖到文末、严格前进（不死循环）。
func TestOverlap(t *testing.T) {
	text := "aa bb cc dd ee ff gg hh"
	chunks, err := Split(text, Options{MaxLen: 8, Overlap: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 手推：滑窗每次前进 3 字节（一个词 + 空白），末块到文末。
	want := []Chunk{
		{Text: text[0:6], Start: 0, End: 6},
		{Text: text[3:9], Start: 3, End: 9},
		{Text: text[6:12], Start: 6, End: 12},
		{Text: text[9:15], Start: 9, End: 15},
		{Text: text[12:18], Start: 12, End: 18},
		{Text: text[15:23], Start: 15, End: 23},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %d, want %d", len(chunks), len(want))
	}
	for i, c := range chunks {
		if c != want[i] {
			t.Fatalf("chunk[%d] = %+v, want %+v", i, c, want[i])
		}
		if runes(c.Text) > 8 {
			t.Fatalf("chunk[%d] exceeds budget", i)
		}
	}
	for i := 0; i+1 < len(chunks); i++ {
		if chunks[i+1].Start >= chunks[i].End {
			t.Fatalf("chunks %d→%d must overlap（Overlap > 0）", i, i+1)
		}
		if ov := runes(text[chunks[i+1].Start:chunks[i].End]); ov > 3 {
			t.Fatalf("overlap %d > 3", ov)
		}
	}
	checkRoundtrip(t, text, chunks)
}

// TestValidation：MaxLen 必须为正；Overlap 必须 < MaxLen。
func TestValidation(t *testing.T) {
	if _, err := Split("x", Options{}); !errors.Is(err, ErrMaxLen) {
		t.Fatalf("err = %v, want ErrMaxLen", err)
	}
	if _, err := Split("x", Options{MaxLen: 4, Overlap: 4}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("err = %v, want ErrOverlap", err)
	}
	if _, err := Split("x", Options{MaxLen: 4, Overlap: 5}); !errors.Is(err, ErrOverlap) {
		t.Fatalf("err = %v, want ErrOverlap", err)
	}
	// Overlap = MaxLen-1 合法。
	if _, err := Split("x", Options{MaxLen: 4, Overlap: 3}); err != nil {
		t.Fatalf("overlap < max len must be accepted: %v", err)
	}
}

// TestCustomSize：注入字节度量（非默认 rune 计数）——预算按字节走，
// 多字节字符被按字节预算切开（Size 注入方的口径自担）。
func TestCustomSize(t *testing.T) {
	text := "啊哦额" // 9 bytes / 3 runes
	chunks, err := Split(text, Options{MaxLen: 4, Size: func(s string) int { return len(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3（byte Size 下每 rune 一块）", len(chunks))
	}
	for _, c := range chunks {
		if len(c.Text) > 4 {
			t.Fatalf("chunk %q = %d bytes, want <= 4", c.Text, len(c.Text))
		}
	}
	checkRoundtrip(t, text, chunks)
}

// TestDegenerateProgress：Size 函数病态（任何非空文本都超预算）——
// 退化路径必须保证进度（每 rune 一块）且不死循环。
func TestDegenerateProgress(t *testing.T) {
	text := "啊哦额"
	chunks, err := Split(text, Options{
		MaxLen: 50,
		Size: func(s string) int {
			if s == "" {
				return 0
			}
			return 100
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3（退化单 rune 成块，不死循环）", len(chunks))
	}
	checkRoundtrip(t, text, chunks)
}
