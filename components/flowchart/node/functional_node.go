package node

import (
	"context"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
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
	// 复用 SimpleNode
	return NewNode(
		id,
		[]string{inputKey},
		[]string{trueKey, falseKey},
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			val := inputs[inputKey]
			if condition(val) {
				// 条件成立：只给 trueKey 赋值
				return map[string]any{trueKey: val}, nil
			}
			// 条件不成立：只给 falseKey 赋值
			return map[string]any{falseKey: val}, nil
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
	condition func(ctx *schema.FlowContext) bool, // 循环条件：true继续，false退出
	loopBody func(ctx *schema.FlowContext), // 循环体逻辑
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
		func(flowCtx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			// 确保在函数退出时调用 cancel
			defer cancel()

			iteration := 0

			for {
				// 检查context是否取消
				select {
				case <-ctx.Done():
					result := schema.NewLoopCancelledResult(iteration, ctx.Err())
					return map[string]any{
						outputKey: result,
					}, schema.ErrLoopCancelled
				default:
				}

				// 检查最大循环次数
				if config.MaxIterations > 0 && iteration >= config.MaxIterations {
					result := schema.NewLoopMaxIterationsResult(config.MaxIterations, iteration)
					return map[string]any{
						outputKey: result,
					}, nil
				}

				// 检查循环条件
				if !condition(flowCtx) {
					result := schema.NewLoopConditionFailedResult(iteration)
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
		waitKeys,            // 输入：所有要并行等待的key
		[]string{outputKey}, // 输出：完成信号
		// 核心逻辑：WaitAll 已经自动并行等待所有值
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			// 所有输入都已经在 WaitAll 里等待完成了
			// 走到这里 = 全部并行结束
			return map[string]any{
				outputKey: "complete",
			}, nil
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
	OutputKey string,
	model chatmodel.BaseModel,
	copies uint,
) *SimpleNode {

	return NewNode(
		id,
		[]string{promptKey},
		[]string{OutputKey},

		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {

			prompt := inputs[promptKey].(string)
			msgs := []*schema.Message{{Role: "user", Content: prompt}}

			streamReader, err := model.Stream(context.Background(), msgs)
			if err != nil {
				return nil, err
			}

			multicastReaders := streamReader.Multicast(copies)

			// 输出【多播后的第一个流】
			// 你永远输出多播流，下游永远安全，不会抢数据
			return map[string]any{
				OutputKey: multicastReaders,
			}, nil
		},
	)
}
