package yaml_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// 显式 stream 标签仍是标签：Analyze 的 any 占位图不能证明真实流类型的静态约束。
func TestAnalyzeStreamTypeLabelsDoNotProveStreamSafety(t *testing.T) {
	const doc = `
nodes:
  - id: writer
    uses: write
    provides: [{name: stream, type: "pulse.Stream[int]"}]
`
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[pulse.Stream[int]]("stream"))
	calls := 0
	reg.MustRegister("write", func(*pulse.RunCtx) error {
		calls++
		return nil
	})
	opts := pulseyaml.LoadOptions{GraphID: "stream-label-boundary"}
	top, err := pulseyaml.Analyze([]byte(doc), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Problems) != 0 || top.Nodes[0].Provides[0].Type != "pulse.Stream[int]" {
		t.Fatalf("Analyze 应保留标签，但不报告依赖真实类型的缺失读取者：%+v", top)
	}
	g, _, err := pulseyaml.Load([]byte(doc), reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	loaded := g.Topology()
	if len(loaded.Problems) == 0 || !strings.Contains(fmt.Sprint(loaded.Problems), `provides "stream" but nothing consumes it`) {
		t.Fatalf("有真实类型的 Topology 应报告缺失读取者：%v", loaded.Problems)
	}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), `provides "stream" but nothing consumes it`) {
		t.Fatalf("Validate 应拒绝无读取者的流：%v", err)
	}
	if calls != 0 {
		t.Fatalf("静态检查不应执行工厂：calls=%d", calls)
	}
	t.Logf("Analyze.Problems=%v; Load.Topology.Problems=%v", top.Problems, loaded.Problems)
}
