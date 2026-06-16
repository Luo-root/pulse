package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/flowchart/flow"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/stream"
)

// NewConditionNode 创建【条件判断节点】
// id: 节点ID
// inputKey: 要判断的输入key
// condition: 条件函数（返回true/false）
// trueKey: 条件成立时输出的key
// falseKey: 条件不成立时输出的key
func NewConditionNode(
	id string,
	inputKey string,
	condition func(value any) bool,
	trueKey string,
	falseKey string,
) *SimpleNode {
	return NewNode(
		id,
		[]string{inputKey},
		[]string{trueKey, falseKey},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			val := inputs[inputKey]
			match := condition(val)
			// 两个 key 都设置：match 表示条件是否成立
			return map[string]any{
				trueKey:  match,
				falseKey: !match,
			}, nil
		},
	)
}

// LoopConfig 循环节点配置
type LoopConfig struct {
	MaxIterations int             // 最大循环次数（0表示无限制）
	Timeout       time.Duration   // 超时时间（0表示无超时）
	Context       context.Context // 外部context，用于取消（nil则使用background）
}

// NewLoopNode 创建【循环节点】（while 模式：条件为真就一直执行）
// id: 节点ID
// controlKey: 循环控制key（节点会等待这个key来启动循环）
// condition: 循环条件函数，返回true=继续循环，false=退出循环
// loopBody: 循环体内执行的逻辑
// outputKey: 循环结束后输出的结果key
// config: 循环配置（最大次数、超时、context）
func NewLoopNode(
	id string,
	controlKey string,
	condition func(ctx *flow.FlowContext) bool, // 循环条件：true继续，false退出
	loopBody func(ctx *flow.FlowContext), // 循环体逻辑
	outputKey string, // 循环结束输出key
	config *LoopConfig,
) *SimpleNode {

	// 默认配置
	if config == nil {
		config = &LoopConfig{}
	}

	// 设置默认context
	ctx := config.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// 如果配置了超时，创建带超时的context
	var cancel context.CancelFunc
	if config.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, config.Timeout)
	} else {
		// 即使没有超时，也创建一个可取消的context
		ctx, cancel = context.WithCancel(ctx)
	}

	return NewNode(
		id,
		[]string{controlKey},
		[]string{outputKey},

		// 核心：循环执行逻辑
		func(flowCtx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			// 确保在函数退出时调用 cancel
			defer cancel()

			iteration := 0

			for {
				// 检查context是否取消
				select {
				case <-ctx.Done():
					// 区分超时和其他取消原因
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						result := NewLoopTimeoutResult(iteration)
						return map[string]any{
							outputKey: result,
						}, ErrLoopTimeout
					}

					// 其他取消原因（手动取消、父context取消等）
					result := NewLoopCancelledResult(iteration, ctx.Err())
					return map[string]any{
						outputKey: result,
					}, ErrLoopCancelled
				default:
				}

				// 检查最大循环次数
				if config.MaxIterations > 0 && iteration >= config.MaxIterations {
					result := NewLoopMaxIterationsResult(config.MaxIterations, iteration)
					return map[string]any{
						outputKey: result,
					}, nil
				}

				// 检查循环条件
				if !condition(flowCtx) {
					// 条件不满足，退出循环（正常完成）
					result := NewLoopCompletedResult(iteration)
					return map[string]any{
						outputKey: result,
					}, nil
				}

				// 执行循环体
				loopBody(flowCtx)
				iteration++
			}
		},
	)
}

// NewParallelNode 创建【并行汇聚节点】
// 作用：等待所有输入全部就绪 → 然后输出完成信号
// id: 节点ID
// waitKeys: 要等待的所有输入key（数组）
// outputKey: 全部完成后输出的key
func NewParallelNode(
	id string,
	waitKeys []string,
	outputKey string,
) *SimpleNode {
	return NewNode(
		id,
		waitKeys,
		[]string{outputKey},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			// 透传所有输入，附加一个完成标记
			merged := make(map[string]any, len(inputs)+1)
			for k, v := range inputs {
				merged[k] = v
			}
			merged["__parallel_complete"] = true
			return map[string]any{outputKey: merged}, nil
		},
	)
}

// NewLLMStreamNode 创建【流式LLM节点】
// id: 节点ID
// promptKey: 从上下文获取提示词的key
// OutputKey: 返回一个 []*StreamReader
// model: *chatmodel.BaseModel 实例
// copies: StreamReader的数量
func NewLLMStreamNode(
	id string,
	promptKey string,
	outputKey string,
	model chatmodel.BaseModel,
	copies uint,
) *SimpleNode {
	return NewNode(
		id,
		[]string{promptKey},
		[]string{outputKey},
		func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
			prompt, ok := inputs[promptKey].(string)
			if !ok {
				return nil, fmt.Errorf("node %s: input %q is not a string, got %T", id, promptKey, inputs[promptKey])
			}

			msgs := []*schema.Message{{Role: "user", Content: prompt}}
			streamReader, err := model.Stream(ctx.GetContext(), msgs)
			if err != nil {
				return nil, err
			}

			mc := stream.NewMulticastController(streamReader, 16)
			readers := mc.Fork(int(copies))

			return map[string]any{outputKey: readers}, nil
		},
	)
}
