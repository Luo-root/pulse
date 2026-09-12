package kernel

import (
	"fmt"
	"testing"
)

// 本文件是 #168 的基准口径（只用公开 API，可在修复前后同一基准下对照）：
// 请求级 scope 的典型周期 = 派生 → 每请求 Provide（CollectorKey 形态）
// → 撤销 → 销毁。

var benchKey = NewServiceKey[int]("test.bench.key")

// benchBuildTree 建一棵 root + n 个插件的树。declare=true 时插件声明
// benchKey（外加一个永不提供的依赖，使其停在 Inactive，通知只做判定
// 不做状态迁移）；false 时插件无依赖（benchKey 无人声明）。
func benchBuildTree(b *testing.B, n int, declare bool) *Context {
	b.Helper()
	root := New()
	neverKey := NewServiceKey[int]("test.bench.never")
	for i := 0; i < n; i++ {
		var deps []Dependency
		if declare {
			deps = []Dependency{Require(benchKey), Require(neverKey)}
		}
		if _, err := Use(root, &countingPlugin{deps: deps}); err != nil {
			b.Fatal(err)
		}
	}
	return root
}

// BenchmarkScopeCycle 基线：请求级 scope 的派生 + 销毁。
func BenchmarkScopeCycle(b *testing.B) {
	root := New()
	defer root.Dispose()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		scope, err := root.Derive()
		if err != nil {
			b.Fatal(err)
		}
		scope.Dispose()
	}
}

// BenchmarkProvideCycleNoDependents 每请求 Provide 一个**无人声明**的服务
// （CollectorKey 形态）：成本应与插件树规模解耦（#168 修复前 ≈ +47ns/插件）。
func BenchmarkProvideCycleNoDependents(b *testing.B) {
	for _, fibers := range []int{0, 10, 50, 100} {
		root := benchBuildTree(b, fibers, false) // 建树在计时区外
		b.Run(fmt.Sprintf("fibers=%d", fibers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				scope, err := root.Derive()
				if err != nil {
					b.Fatal(err)
				}
				undo, err := Provide(scope, benchKey, i)
				if err != nil {
					b.Fatal(err)
				}
				undo()
				scope.Dispose()
			}
		})
		root.Dispose()
	}
}

// BenchmarkProvideCycleWithDependents 命中 N 个依赖者的扇形口径：通知
// 每个依赖者标脏是固有成本（随 N 增长），给出量级与分配面。
// 每轮两次通知（provide + undo）× N 依赖者会扇出收敛协程，建议以
// 固定迭代数跑（-benchtime=2000x）避免标定开销。
func BenchmarkProvideCycleWithDependents(b *testing.B) {
	for _, dependents := range []int{1, 10} {
		root := benchBuildTree(b, dependents, true)
		b.Run(fmt.Sprintf("dependents=%d", dependents), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				scope, err := root.Derive()
				if err != nil {
					b.Fatal(err)
				}
				undo, err := Provide(scope, benchKey, i)
				if err != nil {
					b.Fatal(err)
				}
				undo()
				scope.Dispose()
			}
		})
		root.Dispose()
	}
}
