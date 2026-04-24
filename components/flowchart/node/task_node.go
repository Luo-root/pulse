package node

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
)

func BatchNewTaskNode(plannerNodeID string, plan *Plan, agent chatmodel.AgentInterface) []*SimpleNode {
	if plan == nil {
		return nil
	}
	var taskNodes []*SimpleNode
	for _, task := range plan.Tasks {
		taskNodes = append(taskNodes, NewTaskNode(plannerNodeID, task, agent))
	}
	return taskNodes
}
func NewTaskNode(plannerNodeID string, task Task, agent chatmodel.AgentInterface) *SimpleNode {
	planName := fmt.Sprintf("%s_plan", plannerNodeID)
	allInputs := append(task.Inputs, planName)
	return NewNode(
		task.ID,
		allInputs,
		task.Outputs,
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			plan, err := ctx.Get(planName)
			if err != nil {
				return nil, err
			}
			planVal := plan.(*Plan)
			taskStateModifyRunning(planVal, task.ID)
			ctx.Set(planName, plan)
			var inputsInfo []string
			for _, input := range allInputs {
				get, err := ctx.Get(input)
				if err != nil {
					return nil, err
				}
				inputsInfo = append(inputsInfo, fmt.Sprintf("%s: %v", input, get))
			}
			strings.Join(inputsInfo, "\n")
			prompt := fmt.Sprintf(`
任务详细目标和输入输出参数含义: 
%s

该任务的前置输入信息:
%s

需要输出的结果参数:
%v

输出格式要求（强制遵守）
1. 仅输出JSON文本，无任何多余文字、注释、说明
2. JSON语法合规（无多余逗号、引号闭合）
3. 严格匹配以下结构，字段不可修改：
{
	"current_path": "<需要的结果>"
	"file_list": "<需要的结果>"
}

请严格按规则输出完整JSON。
开始完成你的任务！
`, task.Description, inputsInfo, task.Outputs)
			resp, err := agent.Send(*ctx.GetContext(), prompt)
			if err != nil {
				taskStateModifyFailed(planVal, task.ID)
				taskModifyError(planVal, task.ID, err.Error())
				return nil, err
			}

			result, err := parseDynamicJSON(resp.Content)
			if err != nil {
				taskStateModifyFailed(planVal, task.ID)
				taskModifyError(planVal, task.ID, err.Error())
				return nil, err
			}

			taskModifyResult(planVal, task.ID, result)
			taskStateModifySuccess(planVal, task.ID)
			ctx.Set(planName, plan)
			return result, nil
		},
	)
}

func taskStateModifyRunning(plan *Plan, taskID string) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == taskID {
			plan.Tasks[i].State = TaskRunning
			break
		}
	}
}

func taskStateModifyFailed(plan *Plan, taskID string) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == taskID {
			plan.Tasks[i].State = TaskFailed
			break
		}
	}
}

func taskModifyError(plan *Plan, taskID string, errMsg string) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == taskID {
			plan.Tasks[i].Error = errMsg
			break
		}
	}
}

func taskStateModifySuccess(plan *Plan, taskID string) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == taskID {
			plan.Tasks[i].State = TaskSuccess
			break
		}
	}
}

func taskModifyResult(plan *Plan, taskID string, result map[string]any) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == taskID {
			plan.Tasks[i].Result = result
			break
		}
	}
}

func parseDynamicJSON(raw string) (map[string]any, error) {
	clean := TrimJSONWrapper(raw)
	var result map[string]any
	err := json.Unmarshal([]byte(clean), &result)
	return result, err
}
