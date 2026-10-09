// 编译期对齐的**反向用例**：元素类型不匹配必须编译失败。
//
// 它不在 `go build ./...` / `go vet ./...` 的 wildcard 里（`testdata` 目录被
// go 工具链默认忽略），由 sugar_test.go 的 TestSugarTypeMismatchFailsToCompile
// 显式 `go build ./testdata/compilefail` 一次，并断言失败原因是类型不匹配。
//
// 坏在哪里：ins 的元素类型是 string，fn 收的是 Batch[int] ——
// Join[T, O] 的 T 只能有一个取值，两者对不上，编译器直接拒绝。
package main

import "github.com/Luo-root/pulse"

func main() {
	var g *pulse.Graph
	a := pulse.NewKey[string]("compilefail.a")
	out := pulse.NewKey[string]("compilefail.out")

	_ = pulse.Join(g, "j", []pulse.Key[string]{a}, out, func(_ *pulse.RunCtx, b pulse.Batch[int]) (string, error) {
		return "", nil
	})
}
