package observe

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TextHandler 的外观不能区分数字和字符串，必须经 JSON decode 验证机器类型。
func TestSlogSinkDurationJSONNumber(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration time.Duration
		want     float64
	}{
		{"585100ns", 585100 * time.Nanosecond, 0.5851},
		{"1ns", time.Nanosecond, 0.000001},
		{"zero", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink := SlogSink{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
			rec := Record{
				HostID: "h", TraceID: "tr", Source: SourceObserver, Event: "duration",
				Status: "failed", Duration: tc.duration, Err: errors.New("probe error"),
			}
			Set(&rec.Attrs, "probe.numeric", float64(1.25))
			Set(&rec.Attrs, "probe.label", "control")
			sink.Write(rec)

			var decoded map[string]any
			if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if control, ok := decoded["probe.numeric"].(float64); !ok || control != 1.25 {
				t.Fatalf("JSON 数字对照错误: %v", decoded["probe.numeric"])
			}
			value, present := decoded["duration_ms"]
			if tc.duration == 0 {
				if present {
					t.Fatalf("零耗时应省键: %s", buf.String())
				}
			} else if number, ok := value.(float64); !ok || number != tc.want {
				t.Fatalf("duration_ms=(%T,%v), want JSON number %v: %s", value, value, tc.want, buf.String())
			}

			// 包装字段与开放属性保留既有顺序；省略零耗时只移除该键。
			keys := []string{"status"}
			if tc.duration != 0 {
				keys = append(keys, "duration_ms")
			}
			keys = append(keys, "event", "source", "probe.numeric", "probe.label", "host_id", "error", "trace_id")
			last := -1
			for _, key := range keys {
				at := strings.Index(buf.String(), `"`+key+`":`)
				if at <= last {
					t.Fatalf("字段 %s 丢失或顺序错误: %s", key, buf.String())
				}
				last = at
			}
			t.Logf("duration_ns=%d duration_ms_type=%T value=%v present=%t", tc.duration, value, value, present)
		})
	}
}
