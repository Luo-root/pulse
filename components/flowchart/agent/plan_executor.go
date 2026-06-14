package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

// PlanExecutor Plan-and-Execute 策略引擎
// Phase 1: LLM 生成执行计划（自然语言步骤）
// Phase 2: 验证计划（依赖检查）
// Phase 3: 逐步执行，每步用 ReAct 循环
// Phase 4: 验证结果，必要时重规划
type PlanExecutor struct {
	model            chatmodel.BaseModel
	registry         *tools.ToolRegistry
	maxSteps         int
	maxReplans       int
	maxRoundsPerStep int
	maxPlanRounds    int
	systemPrompt     string
	onStep           func(step *ExecStep)
	onPlan           func(steps []PlanStep)
}

// PlanStep 计划中的单个步骤
type PlanStep struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// ExecStep 执行步骤记录
type ExecStep struct {
	PlanStep    PlanStep
	AgentResult *AgentResult
	Status      StepStatus
	Error       string
	Duration    time.Duration
}

// StepStatus 步骤状态
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepSuccess StepStatus = "success"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// PlanResult 规划执行结果
type PlanResult struct {
	Answer    string
	Steps     []*ExecStep
	PlanSteps []PlanStep
	Rounds    int // 总 ReAct 轮次
	Replans   int // 重规划次数
	Usage     schema.Usage
}

// PlanOption PlanExecutor 配置选项
type PlanOption func(*PlanExecutor)

func WithPlanMaxSteps(n int) PlanOption {
	return func(pe *PlanExecutor) {
		if n > 0 {
			pe.maxSteps = n
		}
	}
}

func WithPlanMaxReplans(n int) PlanOption {
	return func(pe *PlanExecutor) {
		if n >= 0 {
			pe.maxReplans = n
		}
	}
}

func WithPlanMaxRoundsPerStep(n int) PlanOption {
	return func(pe *PlanExecutor) {
		pe.maxRoundsPerStep = n
	}
}

func WithPlanMaxPlanRounds(n int) PlanOption {
	return func(pe *PlanExecutor) {
		pe.maxPlanRounds = n
	}
}

func WithPlanSystemPrompt(prompt string) PlanOption {
	return func(pe *PlanExecutor) {
		pe.systemPrompt = prompt
	}
}

func WithPlanStepCallback(fn func(step *ExecStep)) PlanOption {
	return func(pe *PlanExecutor) {
		pe.onStep = fn
	}
}

func WithPlanCallback(fn func(steps []PlanStep)) PlanOption {
	return func(pe *PlanExecutor) {
		pe.onPlan = fn
	}
}

// NewPlanExecutor 创建 Plan-and-Execute 引擎
func NewPlanExecutor(model chatmodel.BaseModel, registry *tools.ToolRegistry, opts ...PlanOption) *PlanExecutor {
	pe := &PlanExecutor{
		model:            model,
		registry:         registry,
		maxSteps:         8,
		maxReplans:       2,
		maxRoundsPerStep: 10,
		maxPlanRounds:    5,
	}
	for _, opt := range opts {
		opt(pe)
	}
	return pe
}

// Run 执行 Plan-and-Execute 流程
func (pe *PlanExecutor) Run(ctx context.Context, goal string) (*PlanResult, error) {
	result := &PlanResult{}

	// Phase 1: 规划
	planSteps, err := pe.plan(ctx, goal)
	if err != nil {
		return nil, fmt.Errorf("planning failed: %w", err)
	}
	result.PlanSteps = planSteps

	if pe.onPlan != nil {
		pe.onPlan(planSteps)
	}

	// Phase 2: 验证
	if err := validateSteps(planSteps); err != nil {
		return nil, fmt.Errorf("plan validation failed: %w", err)
	}

	// Phase 3: 执行 + Phase 4: 重规划
	stepResults := make(map[string]*ExecStep)
	for _, ps := range planSteps {
		stepResults[ps.ID] = &ExecStep{PlanStep: ps, Status: StepPending}
	}

	for replanCount := 0; replanCount <= pe.maxReplans; replanCount++ {
		result.Replans = replanCount

		// 按依赖顺序执行
		failedSteps := pe.executeSteps(ctx, goal, planSteps, stepResults, result)
		if len(failedSteps) == 0 {
			break // 全部成功
		}

		if replanCount >= pe.maxReplans {
			break // 达到重规划上限
		}

		// 重规划
		newSteps, err := pe.replan(ctx, goal, planSteps, stepResults, failedSteps)
		if err != nil {
			break // 重规划失败，保留当前结果
		}
		planSteps = newSteps
		result.PlanSteps = planSteps

		if pe.onPlan != nil {
			pe.onPlan(planSteps)
		}

		// 重置失败步骤
		for _, fs := range failedSteps {
			stepResults[fs] = &ExecStep{
				PlanStep: findStep(planSteps, fs),
				Status:   StepPending,
			}
		}
	}

	// 收集结果
	result.Answer = synthesizeAnswer(goal, stepResults)
	for _, ps := range planSteps {
		if es, ok := stepResults[ps.ID]; ok {
			result.Steps = append(result.Steps, es)
			if es.AgentResult != nil {
				result.Rounds += es.AgentResult.Rounds
				result.Usage.PromptTokens += es.AgentResult.Usage.PromptTokens
				result.Usage.CompletionTokens += es.AgentResult.Usage.CompletionTokens
				result.Usage.TotalTokens += es.AgentResult.Usage.TotalTokens
			}
		}
	}

	return result, nil
}

// plan 调用 LLM 生成执行计划
func (pe *PlanExecutor) plan(ctx context.Context, goal string) ([]PlanStep, error) {
	toolList := pe.getToolList()
	prompt := fmt.Sprintf(`你是一个任务规划专家。请将用户目标拆解为可执行的步骤列表。

## 目标
%s

## 可用工具
%s

## 要求
1. 每个步骤是一个独立的、可验证的操作
2. 步骤数量 2-%d 个
3. 用 depends_on 字段声明步骤之间的依赖关系
4. 无依赖的步骤 depends_on 为空数组
5. 仅输出 JSON，无其他文字

## 输出格式
{
  "steps": [
    {"id": "step_1", "description": "步骤描述", "depends_on": []},
    {"id": "step_2", "description": "步骤描述", "depends_on": ["step_1"]}
  ]
}`, goal, toolList, pe.maxSteps)

	loop := NewAgentLoop(pe.model, pe.registry,
		WithMaxRounds(pe.maxPlanRounds),
		WithSystemPrompt("你是一个任务规划专家，只输出 JSON。"),
	)

	agentResult, err := loop.Run(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return parsePlanSteps(agentResult.Answer)
}

// executeSteps 按依赖顺序执行所有步骤
func (pe *PlanExecutor) executeSteps(ctx context.Context, goal string, planSteps []PlanStep, stepResults map[string]*ExecStep, result *PlanResult) []string {
	executed := make(map[string]bool)
	var failedSteps []string

	for {
		// 找到所有可执行的步骤（依赖已满足）
		ready := false
		for _, ps := range planSteps {
			es := stepResults[ps.ID]
			if es.Status != StepPending {
				continue
			}

			// 检查依赖是否全部完成
			depsReady := true
			for _, dep := range ps.DependsOn {
				if depResult, ok := stepResults[dep]; !ok || depResult.Status != StepSuccess {
					depsReady = false
					break
				}
			}
			if !depsReady {
				continue
			}

			ready = true
			es.Status = StepRunning

			// 构建上下文：目标 + 前序步骤结果
			stepContext := pe.buildStepContext(goal, ps, stepResults)

			// 用 ReAct 循环执行单步
			loop := NewAgentLoop(pe.model, pe.registry,
				WithMaxRounds(pe.maxRoundsPerStep),
				WithSystemPrompt(pe.systemPrompt),
			)

			agentResult, err := loop.Run(ctx, stepContext)
			es.Duration = time.Duration(0) // 已在 AgentResult 中记录

			if err != nil {
				es.Status = StepFailed
				es.Error = err.Error()
				failedSteps = append(failedSteps, ps.ID)
			} else {
				es.Status = StepSuccess
				es.AgentResult = agentResult
			}

			executed[ps.ID] = true

			if pe.onStep != nil {
				pe.onStep(es)
			}
		}

		if !ready {
			break // 没有更多可执行的步骤
		}
	}

	return failedSteps
}

// buildStepContext 构建单步执行的上下文描述
func (pe *PlanExecutor) buildStepContext(goal string, step PlanStep, stepResults map[string]*ExecStep) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## 总目标\n%s\n\n", goal))
	sb.WriteString(fmt.Sprintf("## 当前步骤\nID: %s\n描述: %s\n\n", step.ID, step.Description))

	// 加入前序步骤的结果
	var prevResults []string
	for _, dep := range step.DependsOn {
		if es, ok := stepResults[dep]; ok && es.Status == StepSuccess && es.AgentResult != nil {
			prevResults = append(prevResults, fmt.Sprintf("- %s: %s", dep, es.AgentResult.Answer))
		}
	}
	if len(prevResults) > 0 {
		sb.WriteString("## 前序步骤结果\n")
		for _, r := range prevResults {
			sb.WriteString(r + "\n")
		}
	}

	return sb.String()
}

// replan 重规划失败步骤
func (pe *PlanExecutor) replan(ctx context.Context, goal string, currentSteps []PlanStep, stepResults map[string]*ExecStep, failedStepIDs []string) ([]PlanStep, error) {
	var failedInfo []string
	for _, id := range failedStepIDs {
		if es, ok := findExecStep(stepResults, id); ok {
			failedInfo = append(failedInfo, fmt.Sprintf("- %s: %s (错误: %s)", es.PlanStep.ID, es.PlanStep.Description, es.Error))
		}
	}

	prompt := fmt.Sprintf(`你是一个任务重规划专家。以下计划中有步骤执行失败，请调整计划。

## 原目标
%s

## 当前计划
%s

## 失败步骤
%s

## 要求
1. 保留已成功的步骤不变
2. 调整失败步骤的描述或依赖
3. 可以新增替代步骤
4. 仅输出调整后的完整计划 JSON`, goal, formatCurrentSteps(currentSteps), strings.Join(failedInfo, "\n"))

	loop := NewAgentLoop(pe.model, pe.registry,
		WithMaxRounds(pe.maxPlanRounds),
		WithSystemPrompt("你是一个任务重规划专家，只输出 JSON。"),
	)

	agentResult, err := loop.Run(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return parsePlanSteps(agentResult.Answer)
}

// getToolList 获取可用工具列表
func (pe *PlanExecutor) getToolList() string {
	if pe.registry == nil {
		return "无可用工具"
	}
	toolDefs := pe.registry.GetEnabledTools()
	if len(toolDefs) == 0 {
		return "无可用工具"
	}
	var parts []string
	for _, t := range toolDefs {
		parts = append(parts, fmt.Sprintf("- %s: %s", t.Name, t.Description))
	}
	return strings.Join(parts, "\n")
}

// ============================================================================
// 辅助函数
// ============================================================================

// parsePlanSteps 解析 LLM 返回的计划 JSON
func parsePlanSteps(raw string) ([]PlanStep, error) {
	clean := trimJSONWrapper(raw)

	var plan struct {
		Steps []PlanStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(clean), &plan); err != nil {
		return nil, fmt.Errorf("parse plan JSON: %w", err)
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("plan has no steps")
	}

	// 设置默认 ID
	for i := range plan.Steps {
		if plan.Steps[i].ID == "" {
			plan.Steps[i].ID = fmt.Sprintf("step_%d", i+1)
		}
		if plan.Steps[i].DependsOn == nil {
			plan.Steps[i].DependsOn = []string{}
		}
	}

	return plan.Steps, nil
}

// validateSteps 验证步骤的依赖关系
func validateSteps(steps []PlanStep) error {
	ids := make(map[string]bool)
	for _, s := range steps {
		ids[s.ID] = true
	}

	// 检查依赖是否存在
	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("step %s depends on unknown step %s", s.ID, dep)
			}
		}
	}

	// 检查循环依赖（简单拓扑排序）
	inDegree := make(map[string]int)
	dependents := make(map[string][]string)
	for _, s := range steps {
		inDegree[s.ID] = len(s.DependsOn)
		for _, dep := range s.DependsOn {
			dependents[dep] = append(dependents[dep], s.ID)
		}
	}

	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	processed := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		processed++
		for _, dep := range dependents[id] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if processed != len(steps) {
		return fmt.Errorf("cycle detected in step dependencies")
	}

	return nil
}

// findStep 在步骤列表中查找指定 ID 的步骤
func findStep(steps []PlanStep, id string) PlanStep {
	for _, s := range steps {
		if s.ID == id {
			return s
		}
	}
	return PlanStep{ID: id}
}

// findExecStep 从执行结果中查找指定 ID 的步骤
func findExecStep(stepResults map[string]*ExecStep, id string) (*ExecStep, bool) {
	es, ok := stepResults[id]
	return es, ok
}

// formatCurrentSteps 格式化当前步骤列表
func formatCurrentSteps(steps []PlanStep) string {
	var parts []string
	for _, s := range steps {
		deps := "无"
		if len(s.DependsOn) > 0 {
			deps = strings.Join(s.DependsOn, ", ")
		}
		parts = append(parts, fmt.Sprintf("- %s: %s (依赖: %s)", s.ID, s.Description, deps))
	}
	return strings.Join(parts, "\n")
}

// synthesizeAnswer 从步骤结果中合成最终答案
func synthesizeAnswer(goal string, stepResults map[string]*ExecStep) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("目标: %s", goal))
	parts = append(parts, "")

	for _, es := range stepResults {
		status := "✓"
		if es.Status == StepFailed {
			status = "✗"
		}
		if es.AgentResult != nil {
			parts = append(parts, fmt.Sprintf("%s %s: %s", status, es.PlanStep.Description, es.AgentResult.Answer))
		} else if es.Error != "" {
			parts = append(parts, fmt.Sprintf("%s %s: 失败 - %s", status, es.PlanStep.Description, es.Error))
		}
	}

	return strings.Join(parts, "\n")
}

// trimJSONWrapper 去除 JSON 外层包装（markdown code block 等）
func trimJSONWrapper(raw string) string {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return ""
	}

	// 去掉 markdown code block
	if strings.HasPrefix(clean, "```") {
		lines := strings.Split(clean, "\n")
		var inner []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock || !strings.HasPrefix(strings.TrimSpace(line), "```") {
				inner = append(inner, line)
			}
		}
		clean = strings.TrimSpace(strings.Join(inner, "\n"))
	}

	// 找到第一个 { 和最后一个 }
	start := strings.Index(clean, "{")
	end := strings.LastIndex(clean, "}")
	if start >= 0 && end > start {
		clean = clean[start : end+1]
	}

	return clean
}
