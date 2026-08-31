// reflection.go：07 课的反思与指标面段。
//
// reflection.Reflect 是可配置 background reflection 的最小编排：输入
// 预算截断 → candidate 提炼 → 计数 → 审计结果。默认关（无后台循环、
// 无计时器）——触发时机归宿主，本课在「会话末」调一次示范。
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/candidate"
	"github.com/Luo-root/pulse/memory/reflection"
	"github.com/Luo-root/pulse/memory/store"
)

// reflectionDemo：会话末触发一次反思 → 候选入库 → 打印三处指标快照。
func reflectionDemo() error {
	ctx := context.Background()
	memStore := store.NewMemoryStore()
	ns := (store.MemoryScope{TenantID: "acme", UserID: "u1"}).Namespace()

	cand, err := candidate.New(candidate.Options{
		Store:     memStore,
		Extractor: &fixedExtractor{},
		Namespace: ns,
		OriginFn:  func() store.SourceRef { return store.SourceRef{Type: store.SourceSession, SessionID: "prod-session", Seq: 42} },
	})
	if err != nil {
		return err
	}
	reflector, err := reflection.New(reflection.Options{
		Pipeline:      cand,
		MaxInputChars: 2000, // 预算门：超限从头部丢整条消息（尾部保留——提取看近期内容）
	})
	if err != nil {
		return err
	}

	// 模拟一段刚结束的会话 surface（宿主从 session.Surface() 取出喂入）。
	surface := []*llm.Message{
		llm.UserText(strings.Repeat("我们讨论了部署细节。", 40)),
		llm.UserText("记住：生产环境的审计日志必须写 PostgreSQL，且保留 180 天。"),
	}
	res, err := reflector.Reflect(ctx, surface)
	if err != nil {
		return err
	}

	fmt.Println("=== 反思与指标面（D4）===")
	fmt.Printf("ReflectionResult: Items=%d Report=%+v InputChars=%d TruncatedChars=%d\n",
		len(res.Items), res.Report, res.InputChars, res.TruncatedChars)
	pending, err := cand.Pending(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Pending 候选 %d 条（审批人盖章后才 Active——反思不自动晋升）\n", len(pending))

	cm := cand.Metrics()
	rm := reflector.Metrics()
	fmt.Println("指标面（三处快照 = D4 六项指标全貌；率值计算归宿主）：")
	fmt.Printf("  candidate.Metrics  = %+v\n", cm)
	fmt.Printf("  reflection.Metrics = %+v（token 成本 v1 = Runs/字符数；真实 usage 归宿主桥）\n", rm)
	fmt.Println("  index.Counted      = Searches/Hits（召回命中；本课未接向量索引，见 06 课）")
	fmt.Println("审计接法：ReflectionResult 与各 Metrics 快照由宿主桥进 observability/监控栈")
	fmt.Println("（memory/* 不 import observability——旁路由装配层做，request.usage 同先例）。")
	return nil
}

// fixedExtractor 固定提炼结果（真实项目由宿主 LLM seam 承担提取协议）。
type fixedExtractor struct{}

func (fixedExtractor) Extract(_ context.Context, _ []*llm.Message) ([]store.MemoryItem, error) {
	return []store.MemoryItem{{
		Kind:    store.KindDecision,
		Content: "production audit logs go to PostgreSQL, retained 180 days",
	}}, nil
}
