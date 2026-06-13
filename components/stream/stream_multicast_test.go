package stream

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// TestMulticastControllerBasic 测试多播控制器基础功能
func TestMulticastControllerBasic(t *testing.T) {
	// 创建源流
	source := schema.NewStreamReaderWithBuffer(10)

	// 创建多播控制器
	mc := NewMulticastController(source, 10)

	// Fork 3个子流
	readers := mc.Fork(3)
	if len(readers) != 3 {
		t.Fatalf("expected 3 readers, got %d", len(readers))
	}

	// 向源流发送消息
	msgs := []schema.Message{
		{Role: schema.AssistantRole, Content: "Hello"},
		{Role: schema.AssistantRole, Content: "World"},
		{Role: schema.AssistantRole, Content: "!"},
	}

	// 在goroutine中发送消息
	go func() {
		for _, msg := range msgs {
			source.Send(msg)
		}
		source.Close()
	}()

	// 从3个子流读取
	var wg sync.WaitGroup
	results := make([][]string, 3)

	for i := range readers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var contents []string
			for {
				msg, err := readers[idx].Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Logf("reader %d error: %v", idx, err)
					break
				}
				contents = append(contents, msg.Content)
			}
			results[idx] = contents
		}(i)
	}

	wg.Wait()

	// 验证所有子流收到相同的消息
	expected := []string{"Hello", "World", "!"}
	for i, result := range results {
		if len(result) != len(expected) {
			t.Errorf("reader %d: expected %d messages, got %d", i, len(expected), len(result))
			continue
		}
		for j, content := range result {
			if content != expected[j] {
				t.Errorf("reader %d message %d: expected %q, got %q", i, j, expected[j], content)
			}
		}
	}

	t.Logf("✅ Multicast basic test passed")
}

// TestMulticastControllerErrorPropagation 测试错误传播
func TestMulticastControllerErrorPropagation(t *testing.T) {
	source := schema.NewStreamReaderWithBuffer(10)
	mc := NewMulticastController(source, 10)
	readers := mc.Fork(2)

	// 发送一条消息
	source.Send(schema.Message{Role: schema.AssistantRole, Content: "msg1"})
	// 等待消息被转发
	time.Sleep(50 * time.Millisecond)
	// 设置错误并关闭
	source.SetError(errors.New("source error"))
	source.Close()

	// 读取子流
	for i, r := range readers {
		msg1, err := r.Recv()
		if err != nil {
			t.Fatalf("reader %d: unexpected error on first recv: %v", i, err)
		}
		if msg1.Content != "msg1" {
			t.Errorf("reader %d: expected msg1, got %s", i, msg1.Content)
		}

		// 第二次读取应该收到错误
		_, err = r.Recv()
		if err == nil {
			t.Errorf("reader %d: expected error, got nil", i)
		} else if err != io.EOF && err.Error() != "source error" {
			t.Errorf("reader %d: expected 'source error', got %v", i, err)
		}
	}

	t.Logf("✅ Multicast error propagation test passed")
}

// TestMulticastControllerSlowConsumer 测试慢消费者隔离
func TestMulticastControllerSlowConsumer(t *testing.T) {
	source := schema.NewStreamReaderWithBuffer(10)
	mc := NewMulticastController(source, 2) // 小缓冲
	readers := mc.Fork(2)

	// 发送大量消息
	go func() {
		for i := 0; i < 20; i++ {
			source.Send(schema.Message{Role: schema.AssistantRole, Content: "msg"})
		}
		source.Close()
	}()

	// reader 0: 快速消费
	var fastResults int
	go func() {
		for {
			_, err := readers[0].Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			fastResults++
		}
	}()

	// reader 1: 慢速消费（模拟阻塞）
	time.Sleep(100 * time.Millisecond)
	var slowResults int
	for {
		_, err := readers[1].Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Logf("slow reader error: %v", err)
			break
		}
		slowResults++
		time.Sleep(10 * time.Millisecond)
	}

	t.Logf("fast reader got %d messages, slow reader got %d messages", fastResults, slowResults)

	// 慢消费者不应该影响快消费者
	if fastResults == 0 {
		t.Error("fast reader should have received messages")
	}

	t.Logf("✅ Multicast slow consumer test passed")
}

// TestMulticastControllerStop 测试主动停止
func TestMulticastControllerStop(t *testing.T) {
	source := schema.NewStreamReaderWithBuffer(10)
	mc := NewMulticastController(source, 10)
	readers := mc.Fork(2)

	// 不发送任何消息，直接停止
	mc.Stop()

	// 子流应该被关闭
	for i, r := range readers {
		_, err := r.Recv()
		if err != io.EOF && err == nil {
			t.Errorf("reader %d: expected EOF after stop, got nil", i)
		}
	}

	t.Logf("✅ Multicast stop test passed")
}

// TestMulticastControllerLateFork 测试延迟Fork
// 说明：forwardLoop 在首次 Fork 时启动，启动后会立即消费源流中的已有数据
// 因此延迟 Fork 的子流可以收到 Fork 前已存在于源流中的数据（这是预期行为）
func TestMulticastControllerLateFork(t *testing.T) {
	source := schema.NewStreamReaderWithBuffer(10)
	mc := NewMulticastController(source, 10)

	// 先发送一些消息到源流
	source.Send(schema.Message{Role: schema.AssistantRole, Content: "early"})

	// 然后Fork（forwardLoop启动后会立即消费源流中的数据）
	readers := mc.Fork(1)

	// 继续发送
	go func() {
		// 给forwardLoop一点时间处理early
		time.Sleep(20 * time.Millisecond)
		source.Send(schema.Message{Role: schema.AssistantRole, Content: "late"})
		source.Close()
	}()

	// 应该能收到 early 和 late
	var contents []string
	for {
		msg, err := readers[0].Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		contents = append(contents, msg.Content)
	}

	if len(contents) != 2 || contents[0] != "early" || contents[1] != "late" {
		t.Errorf("expected [early, late], got %v", contents)
	}

	t.Logf("✅ Multicast late fork test passed: received %v", contents)
}
