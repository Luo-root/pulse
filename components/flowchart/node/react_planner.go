package node

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
)

// NewReActPlannerNode 创建 ReAct 规划节点
// id: 节点ID
// user_goal: 目标
// model: 使用的模型
// 规划的结果会存到: ID_plan 中
func NewReActPlannerNode(
	id string,
	agent chatmodel.AgentInterface,
) *SimpleNode {
	userGoal := "user_goal"
	planName := fmt.Sprintf("%s_plan", id)
	return NewNode(
		id,
		[]string{userGoal},
		[]string{planName},
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			get, err := ctx.Get(userGoal)
			if err != nil {
				return nil, err
			}
			plan, err := Planning(*ctx.GetContext(), get.(string), agent)
			if err != nil {
				return map[string]any{
					planName: plan,
				}, err
			}

			plan.mu.Lock()
			for i := range plan.Tasks {
				plan.Tasks[i].State = TaskPending
			}
			plan.mu.Unlock()

			return map[string]any{
				planName: plan,
			}, nil
		},
	)
}

// ScheduleLoopNode 调度循环：监听任务完成/失败，触发重规划
// plannerNodeName: 需要监控的 planner 的节点ID
// 最终结果会存到: final_answer 中
func ScheduleLoopNode(
	plannerNodeID string,
	agent chatmodel.AgentInterface,
) *SimpleNode {
	finalAnswer := "final_answer"
	id := fmt.Sprintf("%s_schedule_loop", plannerNodeID)
	planName := fmt.Sprintf("%s_plan", plannerNodeID)

	return NewNode(
		id,
		[]string{planName, finalAnswer},
		nil,
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			baseCtx := ctx.GetContext()
			for {
				// 从上下文获取最新计划
				planVal, err := ctx.Get(planName)
				if err != nil {
					return nil, fmt.Errorf("plan %s not found in ctx", planName)
				}
				plan, ok := planVal.(*Plan)
				if !ok {
					return nil, fmt.Errorf("plan %s type error", planName)
				}

				select {
				case <-(*baseCtx).Done(): // 监听外部取消/超时
					for i := range plan.Tasks {
						plan.mu.Lock()
						plan.Tasks[i].State = TaskCancelled
						plan.mu.Unlock()
					}
					ctx.Set("final_answer", (*baseCtx).Err().Error())
					return nil, (*baseCtx).Err()
				default:
					time.Sleep(100 * time.Millisecond)
				}

				// 检查失败任务
				var failedTask *Task
				for i := range plan.Tasks {
					if plan.Tasks[i].State == TaskFailed {
						failedTask = &plan.Tasks[i]
						break
					}
				}

				if failedTask != nil {
					fmt.Println("又重规划了")
					// 重规划
					newPlan, err := RePlan(*baseCtx, plan, failedTask, agent)
					if err != nil {
						ctx.Set("final_answer", fmt.Sprintf("RePlan Failed：%v", err))
						break // 无法恢复，结束
					}
					plan = newPlan
					ctx.Set(planName, plan)
					continue
				}

				// 检查是否全部完成
				if IsCompleted(plan) {
					// 收集结果，生成最终答案
					result := synthesizeResult(plan)
					ctx.Set(finalAnswer, result)
					break
				}
			}

			return nil, nil
		},
	)
}

func Planning(ctx context.Context, goal string, agent chatmodel.AgentInterface) (*Plan, error) {
	prompt := fmt.Sprintf(`
# 角色
你是专业的任务规划专家，擅长将复杂目标拆解为可执行、可验证的任务序列。

# 目标
请将用户目标拆解为清晰的任务序列，用户目标: %s

# 任务节点格式要求
任务节点的格式为
- id: 节点ID格式必须为 task_1、task_2、task_3... 依次递增，禁止跳号/自定义格式
- description: 拆解后的任务的描述信息明确任务目标。inputs 中每个参数的含义，outputs 中每个参数的含义
- inputs: 该节点所依赖的输入的名称，为数组形式可以依赖多个输入
- outputs: 该节点的输出的名称，为数组形式可以有多个输出

# 核心要求
## 1. 任务拆解原则
- 粒度适中：每个任务「小到可独立完成，大到有明确产出」，避免过粗（如仅“完成项目”）或过细（如“打开记事本”）；
- 依赖清晰：明确每个任务的前置依赖，无依赖则 inputs 为空数组 []，需要避免循环依赖问题；
- 可独立验证：每个任务完成后有明确输出，可直接判断是否成功（如“输出XX格式的字符串”而非“完成分析”）。

## 2. 其他硬性约束
- 任务数量: 3-8个（根据目标复杂度灵活调整，简单目标取下限，复杂目标取上限）；
- 无冗余任务: 每个任务必须直接服务于最终目标，禁止无意义的“占位任务”；

# 输出格式要求
## 强制规则
1. 仅输出JSON文本，无任何前置/后置文字（如“好的，我规划的任务如下：”）；
2. JSON语法必须合规（无多余逗号、引号闭合、数组/对象结构完整）；
3. 严格匹配以下结构，字段名/层级不可修改：
{
	"tasks": [
		{
			"id": "task_1",
			"description": "获取当前工作目录的绝对路径，作为后续文件操作的基准路径。outputs: {"current_path": "当前的工作路径"}",
			"inputs": [],
			"outputs": ["current_path"],
		},
		{
			"id": "task_2",
			"description": "基于 task_1 的工作目录路径，列出该目录下的所有文件和文件夹，确认 go.mod 文件是否存在。inputs: {"current_path": "当前的工作路径"}, outputs: {"file_list": "文件列表"}",
			"inputs": ["current_path"],
			"outputs": ["file_list"],
		}
	]
}

## 执行指令
请严格按上述规则输出拆解后的任务序列JSON，无需额外说明。`, goal)

	resp, err := agent.Send(ctx, prompt)
	if err != nil {
		return nil, err
	}

	// 解析 Agent 返回的计划
	plan := NewPlan()
	resp.Content = TrimJSONWrapper(resp.Content)
	fmt.Println(resp.Content)
	if err := json.Unmarshal([]byte(resp.Content), &plan); err != nil {
		// Agent 可能没按格式，尝试从 Content 提取
		*plan, err = extractPlan(resp.Content, goal)
		if err != nil {
			return plan, err
		}
	}

	plan.Goal = goal

	return plan, nil
}

func RePlan(ctx context.Context, plan *Plan, failedTask *Task, agent chatmodel.AgentInterface) (*Plan, error) {
	planState := planStateJSON(plan)

	prompt := fmt.Sprintf(`
# 角色
你是专业的任务重规划专家，擅长根据失败任务的信息调整计划，确保整体目标最终达成。

# 背景信息
- 原目标：%s
- 失败任务ID：%s
- 失败任务描述：%s
- 失败错误信息：%s
- 当前完整计划状态（JSON格式）：
%s

# 重规划核心要求
## 1. 重规划原则
- 保持原目标不变：所有调整都应服务于完成最初的用户目标
- 保留已成功任务：状态为 success 的任务必须完整保留，不要修改/删除/变更ID
- 聚焦失败任务：重点分析失败原因，针对性调整
- 最小化变更：仅修改失败任务或新增必要前置任务，禁止大规模重构

## 2. 重规划策略（可选择一种或多种）
请先分析失败原因，再执行对应策略：
- 策略A：任务描述不清晰 → 优化 description，明确任务目标
- 策略B：任务粒度太粗 → 将失败任务拆分为 2-3 个细粒度子任务
- 策略C：缺少前置验证 → 在失败任务前新增验证任务，完善执行条件
- 策略D：输入依赖错误 → 调整任务 inputs 数组，修正依赖关系

## 3. 任务节点格式强制规范（与原规划完全一致）
- id: 节点ID格式必须为 task_1、task_2、task_3... 依次递增，禁止跳号/自定义格式
- description: 拆解后的任务的描述信息明确任务目标。inputs 中每个参数的含义，outputs 中每个参数的含义
- inputs: 该节点所依赖的输入的名称，为数组形式可以依赖多个输入
- outputs: 该节点的输出的名称，为数组形式可以有多个输出
- 禁止循环依赖、禁止冗余任务

## 4. 输出格式要求（强制遵守）
1. 仅输出JSON文本，无任何多余文字、注释、说明
2. JSON语法合规（无多余逗号、引号闭合）
3. 严格匹配以下结构，字段不可修改：
{
	"tasks": [
		{
			"id": "task_1",
			"description": "获取当前工作目录的绝对路径。outputs: {"current_path": "当前的工作路径"}",
			"inputs": [],
			"outputs": ["current_path"]
		},
		{
			"id": "task_2",
			"description": "优化后的任务描述。inputs: {"current_path": "当前的工作路径"}, outputs: {"file_list": "文件列表"}",
			"inputs": ["current_path"],
			"outputs": ["file_list"]
		}
	]
}

请严格按规则输出重规划后的完整计划JSON。`,
		plan.Goal,
		failedTask.ID,
		failedTask.Description,
		failedTask.Error,
		planState,
	)

	// 2. 调用模型生成重规划结果
	resp, err := agent.Send(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("replan failed for task %s: %w", failedTask.ID, err)
	}

	// 3. 解析JSON结果
	newPlan := NewPlan()
	resp.Content = TrimJSONWrapper(resp.Content)
	if err := json.Unmarshal([]byte(resp.Content), &newPlan); err != nil {
		// 解析失败：保留原计划，重置失败任务状态
		newPlan = &Plan{
			Goal:  plan.Goal,
			mu:    &sync.Mutex{},
			Tasks: make([]Task, len(plan.Tasks)),
		}
		copy(newPlan.Tasks, plan.Tasks)

		newPlan.mu.Lock()
		for i := range newPlan.Tasks {
			if newPlan.Tasks[i].ID == failedTask.ID {
				newPlan.Tasks[i].State = TaskPending
				newPlan.Tasks[i].Error = ""
			}
		}
		newPlan.mu.Unlock()
	}

	// 4. 初始化新计划
	newPlan.Goal = plan.Goal
	// 重置所有任务状态（保留成功状态，失败任务改为待执行）
	newPlan.mu.Lock()
	defer newPlan.mu.Unlock()
	for i := range newPlan.Tasks {
		// 仅重置失败任务，已成功任务保持不变
		if newPlan.Tasks[i].ID == failedTask.ID {
			newPlan.Tasks[i].State = TaskPending
			newPlan.Tasks[i].Error = ""
		} else if newPlan.Tasks[i].State == "" {
			newPlan.Tasks[i].State = TaskPending
		}
	}

	return newPlan, nil
}

func IsCompleted(plan *Plan) bool {
	for _, task := range plan.Tasks {
		if task.State != TaskSuccess {
			return false
		}
	}
	return true
}

func extractPlan(content, goal string) (Plan, error) {
	plan := Plan{Goal: goal}
	re := regexp.MustCompile(`\{[\s\S]*"tasks"[\s\S]*\}`)
	if match := re.FindString(content); match != "" {
		err := json.Unmarshal([]byte(match), &plan)
		if err != nil {
			return plan, fmt.Errorf("extractPlan json unmarshal failed: %v", err)
		}
	}
	return plan, nil
}

// planStateJSON 生成计划状态JSON
func planStateJSON(plan *Plan) string {
	if plan == nil {
		return "{}"
	}

	var views []Task
	for _, task := range plan.Tasks {
		views = append(views, Task{
			ID:          task.ID,
			Description: task.Description,
			Inputs:      task.Inputs,
			Outputs:     task.Outputs,
			State:       task.State,
			Result:      task.Result,
			Error:       task.Error,
		})
	}

	b, _ := json.Marshal(map[string]any{
		"goal":  plan.Goal,
		"tasks": views,
	})
	return string(b)
}

func synthesizeResult(plan *Plan) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("目标「%s」已完成，执行结果：", plan.Goal))

	for _, task := range plan.Tasks {
		status := "✓"
		if task.State == TaskFailed {
			status = "✗"
		}
		parts = append(parts, fmt.Sprintf("%s %s: %v", status, task.Description, task.Result))
	}

	return strings.Join(parts, "\n")
}

// 工具方法

func TrimJSONWrapper(raw string) string {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return ""
	}

	clean = trimMarkdownCodeStart(clean)
	clean = trimMarkdownCodeEnd(clean)
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return ""
	}

	clean = extractJSONBody(clean)
	return clean
}

func trimMarkdownCodeStart(s string) string {
	re := regexp.MustCompile(`^(?i)\s*` + "```" + `\s*(json)?\s*`)
	return re.ReplaceAllString(s, "")
}

func trimMarkdownCodeEnd(s string) string {
	re := regexp.MustCompile(`(?i)\s*` + "```" + `\s*$`)
	return re.ReplaceAllString(s, "")
}

func extractJSONBody(s string) string {
	reObject := regexp.MustCompile(`(?s)\{.*\}`)
	if match := reObject.FindString(s); match != "" {
		return match
	}

	reArray := regexp.MustCompile(`(?s)\[.*\]`)
	if match := reArray.FindString(s); match != "" {
		return match
	}

	return s
}
