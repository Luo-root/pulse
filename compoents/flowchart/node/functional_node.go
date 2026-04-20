package node

import (
	"Pulse/compoents/chatmodel"
	"Pulse/compoents/schema"
	"context"
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

// NewLoopNode 创建【循环节点】（while 模式：条件为真就一直执行）
// id: 节点ID
// controlKey: 循环控制key（节点会等待这个key来启动循环）
// condition: 循环条件函数，返回true=继续循环，false=退出循环
// loopBody: 循环体内执行的逻辑
// outputKey: 循环结束后输出的结果key
func NewLoopNode(
	id string,
	controlKey string,
	condition func(ctx *schema.FlowContext) bool, // 循环条件：true继续，false退出
	loopBody func(ctx *schema.FlowContext), // 循环体逻辑
	outputKey string, // 循环结束输出key
) *SimpleNode {

	return NewNode(
		id,
		[]string{controlKey},
		[]string{outputKey},

		// 核心：循环执行逻辑
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			// 第一次被触发后，开始循环
			for condition(ctx) {
				// 执行循环体
				loopBody(ctx)
			}

			// 循环结束，输出结果
			return map[string]any{
				outputKey: "complete",
			}, nil
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
