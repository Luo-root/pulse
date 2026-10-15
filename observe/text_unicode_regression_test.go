package observe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func textUnicodeCases() []struct{ name, value string } {
	return []struct{ name, value string }{
		{"empty", ""},
		{"ascii", "plain/path-42"},
		{"ascii-space", "a b"},
		{"ascii-control", "a\x00\t\n\x7fb"},
		{"equals-and-quote", "a=\"b"},
		{"cjk", "中文日志"},
		{"emoji", "a🪵🚀b"},
		{"combining", "e\u0301"},
		{"replacement-rune", "a\ufffdb"},
		{"next-line", "a\u0085b"},
		{"csi", "a\u009bb"},
		{"line-separator", "a\u2028b"},
		{"no-break-space", "a\u00a0b"},
		{"zero-width-space", "a\u200bb"},
		{"soft-hyphen", "a\u00adb"},
		{"unassigned", "a\u0378b"},
		{"private-use", "a\ue000b"},
		{"noncharacter", "a\ufdd0b"},
		{"non-bmp-format", "a\U000e0001b"},
		{"ascii-control-after-unicode", "中\n"},
		{"invalid-after-unicode", "中\xff"},
		{"invalid-byte", "a\xffb"},
		{"continuation", "a\x80b"},
		{"overlong", "a\xc0\xafb"},
		{"truncated", "a\xe2\x82"},
		{"surrogate", "a\xed\xa0\x80b"},
		{"above-max-rune", "a\xf4\x90\x80\x80b"},
	}
}

// 使用公开 TextHandler 作为独立的编码对照，不复制 needsQuoting 的实现。
func slogTextFragment(t *testing.T, key, value string) string {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.LevelKey || attr.Key == slog.MessageKey {
				return slog.Attr{}
			}
			return attr
		},
	})
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "", 0)
	rec.AddAttrs(slog.String(key, value))
	if err := handler.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func checkTextValueAgainstSlog(t *testing.T, value string) {
	t.Helper()
	want := strings.TrimPrefix(slogTextFragment(t, "value", value), "value=")
	got := string(AppendTextValue([]byte("prefix:"), value))
	if got != "prefix:"+want {
		t.Fatalf("AppendTextValue(%q) = %q, slog.TextHandler wants %q", value, got, "prefix:"+want)
	}
	encoded := strings.TrimPrefix(got, "prefix:")
	if !utf8.ValidString(encoded) {
		t.Fatalf("encoded text is invalid UTF-8: %q", encoded)
	}
	decoded := encoded
	if strings.HasPrefix(encoded, `"`) {
		var err error
		decoded, err = strconv.Unquote(encoded)
		if err != nil {
			t.Fatalf("encoded text is not a Go string literal: %q: %v", encoded, err)
		}
	}
	if decoded != value {
		t.Fatalf("text round trip changed bytes: got %q, want %q", decoded, value)
	}
}

// #399：包括非法 UTF-8 与合法 U+FFFD 的区别，中文、emoji、组合记号保持原样。
func TestAppendTextValueUnicodeMatchesSlog(t *testing.T) {
	for _, tc := range textUnicodeCases() {
		t.Run(tc.name, func(t *testing.T) { checkTextValueAgainstSlog(t, tc.value) })
	}
}

func TestAppendTextValueASCIICompatibility(t *testing.T) {
	for b := byte(0); b < utf8.RuneSelf; b++ {
		// DEL 沿用已有冻结格式，比 TextHandler 更保守；其余 ASCII 同形。
		if b == 0x7f {
			if got := string(AppendTextValue(nil, "a\x7fb")); got != `"a\x7fb"` {
				t.Fatalf("ASCII DEL quoting changed: %q", got)
			}
			continue
		}
		checkTextValueAgainstSlog(t, "a"+string([]byte{b})+"b")
	}
}

// 扫描 Unicode 控制/格式/分隔符类别，而非只对票中列出的四个码点打补丁。
func TestAppendTextValueUnicodeCategoriesMatchSlog(t *testing.T) {
	checked := 0
	for r := rune(utf8.RuneSelf); r <= unicode.MaxRune; r++ {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			checkTextValueAgainstSlog(t, "a"+string(r)+"b")
			checked++
		}
	}
	if checked < 200 {
		t.Fatalf("Unicode category corpus unexpectedly small: %d", checked)
	}
	t.Logf("matched slog for %d non-ASCII control, space, and format characters", checked)
}

func TestTextUnicodePrimitivesMatchLineSink(t *testing.T) {
	for _, tc := range textUnicodeCases() {
		t.Run(tc.name, func(t *testing.T) {
			key := "app." + tc.value
			rec := Record{
				Time:    time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
				Event:   "event",
				Source:  Source("source-" + tc.value),
				HostID:  tc.value,
				Err:     errors.New(tc.value),
				TraceID: tc.value,
			}
			Set(&rec.Attrs, key, tc.value)
			Set(&rec.Attrs, "app.keep", true)
			wantAttr := slogTextFragment(t, key, tc.value)
			if got := string(AppendAttrs(nil, rec.Attrs)); got != wantAttr+" app.keep=true" {
				t.Fatalf("AppendAttrs = %q, want %q", got, wantAttr+" app.keep=true")
			}
			if got := string(AppendAttrsExcept(nil, rec.Attrs, "app.keep")); got != wantAttr {
				t.Fatalf("AppendAttrsExcept = %q, want %q", got, wantAttr)
			}
			render := func(dst []byte, r Record, _ bool) []byte {
				dst = append(dst, " | source="...)
				dst = AppendTextValue(dst, string(r.Source))
				dst = append(dst, " | "...)
				dst = AppendAttrs(dst, r.Attrs)
				if r.HostID != "" {
					dst = append(dst, " | host="...)
					dst = AppendTextValue(dst, r.HostID)
				}
				dst = append(dst, " | err="...)
				dst = AppendTextValue(dst, r.Err.Error())
				if r.TraceID != "" {
					dst = append(dst, " | trace="...)
					dst = AppendTextValue(dst, r.TraceID)
				}
				return dst
			}
			var builtin, custom bytes.Buffer
			NewLineSink(&builtin, WithColor(false), WithImmediate(), WithPrefix("")).Write(rec)
			NewLineSink(&custom, WithColor(false), WithImmediate(), WithPrefix(""), WithRenderer(render)).Write(rec)
			want := fmt.Sprintf(" | %s | %s app.keep=true", slogTextFragment(t, "source", string(rec.Source)), wantAttr)
			if rec.HostID != "" {
				want += " | " + slogTextFragment(t, "host", rec.HostID)
			}
			want += " | " + slogTextFragment(t, "err", rec.Err.Error())
			if rec.TraceID != "" {
				want += " | " + slogTextFragment(t, "trace", rec.TraceID)
			}
			if !strings.HasSuffix(builtin.String(), want+"\n") {
				t.Fatalf("LineSink = %q, want tail %q", builtin.String(), want+"\n")
			}
			if custom.String() != want+"\n" {
				t.Fatalf("custom primitives = %q, built-in tail = %q", custom.String(), want+"\n")
			}
		})
	}
}
