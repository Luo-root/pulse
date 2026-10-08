package observe

// #255-1 的回归用例：AsyncSink 必须把内层出口的 Flush/Err 透传出去。

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestAsyncSinkPassesThroughInnerFlush：包住自带缓冲的出口（LineSink 的 32 KiB
// 写缓冲）时，AsyncSink 只调 inner.Write，于是「排空」≠「落地」——Close 返回 nil
// 之后最后一批仍在内存里，进程退出即丢（观测的静默缺口）。Flush/Close 必须把
// 内层自己的 Flush 跑掉，内层的写错误也要有出口。
func TestAsyncSinkPassesThroughInnerFlush(t *testing.T) {
	t.Run("Flush", func(t *testing.T) {
		var buf bytes.Buffer
		as := NewAsyncSink(NewLineSink(&buf), WithCapacity(8))
		as.Write(Record{Event: "evt"})

		if err := as.Flush(context.Background()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if buf.Len() == 0 {
			t.Fatal("Flush 返回后内层缓冲仍是空的：inner.Flush 没有被透传")
		}
	})

	t.Run("Close", func(t *testing.T) {
		var buf bytes.Buffer
		as := NewAsyncSink(NewLineSink(&buf), WithCapacity(8))
		as.Write(Record{Event: "evt"})

		if err := as.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if buf.Len() == 0 {
			t.Fatal("Close 返回 nil 但内层缓冲仍有记录：最后一批会随进程退出丢掉")
		}
	})

	t.Run("Err", func(t *testing.T) {
		as := NewAsyncSink(NewLineSink(&failWriter{}), WithCapacity(8))
		as.Write(Record{Event: "evt"})

		err := as.Close(context.Background())
		if err == nil || !strings.Contains(err.Error(), "disk on fire") {
			t.Fatalf("Close err = %v, want the inner write error surfaced", err)
		}
	})
}
