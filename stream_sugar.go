package pulse

import (
	"fmt"
	"io"
)

// WithMessageHandling 配置 ConsumeStream / TeeStream 的有限消息尝试；默认只处理一次。
// 普通整节点 Retry 仍不能包含流 I/O。ProduceStream 不处理输入消息，会拒绝非空配置。
// 创建选项和每次应用都复制切片；闭包的业务状态不深复制。
func WithMessageHandling(opts ...MessageOption) StreamOption {
	opts = append([]MessageOption(nil), opts...)
	return func(c *streamConfig) {
		c.messageOptions = append(append([]MessageOption(nil), c.messageOptions...), opts...)
	}
}

func streamWriterStopped[T any](writer *StreamWriter[T]) bool {
	writer.state.mu.Lock()
	defer writer.state.mu.Unlock()
	return writer.state.stopped
}

func streamWritersStopped[T any](writers []*StreamWriter[T]) bool {
	for _, writer := range writers {
		if !streamWriterStopped(writer) {
			return false
		}
	}
	return true
}

// streamOpenOption 只传递打开端点的配置，不把节点切面或消息处理预算传给 OpenStream。
// cfg 是装配时取得的副本；每个分支另取副本，不修改构造糖捕获的配置。
func streamOpenOption(cfg streamConfig) StreamOption {
	cfg.aspects = nil
	cfg.messageOptions = nil
	return func(c *streamConfig) { *c = cfg }
}

// ProduceStream 构造以 Key[Stream[T]] 输出的受管生产节点，description 原样交给 NewNode。
// 打开后直接调用 fn；Send / SendAt 逐条接纳，不等整个回调结束才发布。
// WithBuffer 限制总在途量，默认 1；等待容量时由受管 I/O 临时让出业务额度。
// 完整节点链（含切面）结束后才确定流的终态，fn 的错误、取消和 panic 不转成正常 EOF。
//
// WithStreamSource 只声明外部来源的续读能力；fn 必须自行用 Position / SendAt 续读，
// 本糖不保存任意闭包。读取组已明确 Stop 时，fn 返回精确 ErrStreamStopped 正常结束；
// 包装后的错误、或没有实际 Stop 的停止信号仍是节点失败。
func ProduceStream[T any](g *Graph, id, description string, out Key[Stream[T]],
	fn func(*RunCtx, *StreamWriter[T]) error, opts ...StreamOption) error {
	if g == nil {
		return fmt.Errorf("pulse: ProduceStream %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: ProduceStream: empty node id")
	}
	if fn == nil {
		return fmt.Errorf("pulse: ProduceStream %q: nil fn", id)
	}
	cfg := newStreamConfig(opts)
	if len(cfg.messageOptions) != 0 {
		return fmt.Errorf("pulse: ProduceStream %q: WithMessageHandling requires an input message; use it on ConsumeStream / TeeStream", id)
	}
	return g.Add(NewNode(id, description, nil, Provides(out), func(rc *RunCtx) error {
		writer, err := OpenStream(rc, out, streamOpenOption(cfg))
		if err != nil {
			return err
		}
		err = fn(rc, writer)
		if err == ErrStreamStopped && streamWriterStopped(writer) {
			return nil
		}
		return err
	}, cfg.aspects...))
}

// ConsumeStream 构造受管消费节点，逐条 Receive 后交给 HandleMessage，成功才 Ack。
// WithMessageHandling 显式配置当前消息的预算；失败只重做这一条，不提前读下一条。
// NoValue / Skip 是正常处理；空流成功，未打开的输入沿用普通节点的跳过语义。
// WithBuffer 不创建上游缓冲，正数配置在装配时拒绝。
//
// 回调直接返回 ErrStreamStopped 可在成功确认当前消息后 Stop 本读取组；未处理消息只
// 清理、不确认。包装后的停止错误仍是失败。回调内部不能直接 Stop（它在消息尝试内）。
// 普通错误和取消原样返回；读取到 io.EOF 才正常结束。
func ConsumeStream[T any](g *Graph, id, description string, in Key[Stream[T]],
	fn func(*RunCtx, T) error, opts ...StreamOption) error {
	if g == nil {
		return fmt.Errorf("pulse: ConsumeStream %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: ConsumeStream: empty node id")
	}
	if fn == nil {
		return fmt.Errorf("pulse: ConsumeStream %q: nil fn", id)
	}
	cfg := newStreamConfig(opts)
	if cfg.buf > 0 {
		return fmt.Errorf("pulse: ConsumeStream %q: WithBuffer(%d) is only supported by ProduceStream / TeeStream; ConsumeStream does not create a buffer", id, cfg.buf)
	}
	cfg.messageOptions = append([]MessageOption(nil), cfg.messageOptions...)
	return g.Add(NewNode(id, description, Requires(in), nil, func(rc *RunCtx) error {
		stream, err := Get(rc, in)
		if err != nil {
			return err
		}
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			stop := false
			if err := HandleMessage(rc, delivery, func(rc *RunCtx, value T) error {
				err := fn(rc, value)
				stop = err == ErrStreamStopped
				if stop {
					return nil
				}
				return err
			}, cfg.messageOptions...); err != nil {
				return err
			}
			if stop {
				return stream.Stop(rc)
			}
		}
	}, cfg.aspects...))
}

// TeeStream 把每条受管输入复制到显式列出的分支，每个分支收到完整同序的数据。
// outs 在装配时复制；WithBuffer 控制各分支的总在途量，默认每支 1。
// 一条输入使用一次 HandleMessage：各分支 Send 先暂存，整批接纳成功后才 Ack 输入。
// 慢支提供有界背压，不等待各后代业务完成，也不承诺跨分支同一瞬间可见。
// 已明确 Stop 的分支不再接收消息；所有分支都 Stop 后，本节点 Stop 自己的输入读取组。
// 分支或来源的真实失败仍保留，不能用正常停止掩盖。
//
// 本节点是确定性消息变换，分支默认来源为 "tee/" + id + "/" + keyName，标为可续读：
// 恢复依赖持久输入和消息原子提交，不保存闭包或调用栈。显式 WithStreamSource 保留原值。
// WithStreamAspects 进入普通 NewNode 链，WithMessageHandling 只控制每条输入的预算。
func TeeStream[T any](g *Graph, id, description string, in Key[Stream[T]], outs []Key[Stream[T]],
	opts ...StreamOption) error {
	if g == nil {
		return fmt.Errorf("pulse: TeeStream %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: TeeStream: empty node id")
	}
	if len(outs) == 0 {
		return fmt.Errorf("pulse: TeeStream %q: no outputs", id)
	}
	outs = append([]Key[Stream[T]](nil), outs...)
	cfg := newStreamConfig(opts)
	cfg.messageOptions = append([]MessageOption(nil), cfg.messageOptions...)
	return g.Add(NewNode(id, description, Requires(in), Provides(outs...), func(rc *RunCtx) error {
		stream, err := Get(rc, in)
		if err != nil {
			return err
		}
		writers := make([]*StreamWriter[T], len(outs))
		for i, out := range outs {
			branch := cfg
			if branch.checkpointSource == (CheckpointStreamSource{}) {
				branch.checkpointSource = CheckpointStreamSource{ID: "tee/" + id + "/" + out.asRef().name, Resumable: true}
			}
			writers[i], err = OpenStream(rc, out, streamOpenOption(branch))
			if err != nil {
				return err
			}
		}
		for {
			if streamWritersStopped(writers) {
				return stream.Stop(rc)
			}
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err := HandleMessage(rc, delivery, func(rc *RunCtx, value T) error {
				rc.message.allowStoppedOutputs = true
				for _, writer := range writers {
					if err := writer.Send(value); err != nil {
						return err
					}
				}
				return nil
			}, cfg.messageOptions...); err != nil {
				return err
			}
		}
	}, cfg.aspects...))
}
