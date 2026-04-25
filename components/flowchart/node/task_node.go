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

func buildOutputExample(outputs []string) string {
	example := "{\n"
	for i, output := range outputs {
		example += fmt.Sprintf("\t\"%s\": \"<%s的结果>\"", output, output)
		if i < len(outputs)-1 {
			example += ","
		}
		example += "\n"
	}
	example += "}"
	return example
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
			inputsInfoStr := strings.Join(inputsInfo, "\n")

			prompt := fmt.Sprintf(`
# 任务要求
严格执行以下任务，**必须使用已提供的工具完成所有实际操作**，禁止直接编造任何结果。

## 当前任务
任务ID: %s
任务描述: %s

## 前置输入
%s

## 输出要求
1. 所有实际操作（文件读写、目录检查等）必须通过调用工具完成
2. 工具执行完成后，基于工具返回的结果继续处理
3. 任务全部完成后，仅输出最终结果JSON，严格匹配以下结构：
%s
4. 不要输出任何解释、说明、注释，仅输出JSON

开始执行！
`, task.ID, task.Description, inputsInfoStr, buildOutputExample(task.Outputs))
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
