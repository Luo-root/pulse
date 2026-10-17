package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func envList(name, fallback string) []string {
	value := os.Getenv(name)
	if value == "" {
		value = fallback
	}
	return strings.Split(value, ",")
}

func benchInt(b *testing.B, value string) int {
	b.Helper()
	n, err := strconv.Atoi(value)
	if err != nil {
		b.Fatal(err)
	}
	return n
}

// BenchmarkDAG 与 CLI 共享装配/运行边界；用 DAG_* 环境变量缩小原生 benchmem 矩阵。
func BenchmarkDAG(b *testing.B) {
	parallelism := benchInt(b, strings.Join(envList("DAG_PARALLELISM", "4"), ","))
	procs := benchInt(b, strings.Join(envList("DAG_PROCS", "4"), ","))
	for _, engine := range envList("DAG_ENGINES", "direct,pulse,azure,taskflow") {
		for _, shape := range envList("DAG_SHAPES", "chain,wide,diamonds") {
			for _, size := range envList("DAG_NODES", "1,10,100,1000,10000") {
				for _, work := range envList("DAG_ROUNDS", "1,256") {
					for _, mode := range envList("DAG_MODES", "e2e,build,reuse") {
						c := config{engine: engine, shape: shape, mode: mode, nodes: benchInt(b, size), rounds: benchInt(b, work),
							parallelism: parallelism, procs: procs, minTime: time.Millisecond, timeout: 5 * time.Minute}
						b.Run(fmt.Sprintf("%s/%s/N%d/R%d/P%d/procs%d/%s", engine, shape, c.nodes, c.rounds, parallelism, procs, mode), func(b *testing.B) {
							if err := c.validate(); err != nil {
								b.Fatal(err)
							}
							old := runtime.GOMAXPROCS(procs)
							defer runtime.GOMAXPROCS(old)
							g, _ := newGraph(shape, c.nodes)
							_, checksum := g.reference(c.rounds)
							ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
							defer cancel()
							o, err := prepare(c, g)
							if err != nil {
								b.Fatal(err)
							}
							if _, err := o.run(ctx); err != nil {
								b.Fatal(err)
							}
							o.last = nil
							b.ReportAllocs()
							b.ResetTimer()
							for range b.N {
								if err := ctx.Err(); err != nil {
									b.Fatal(err)
								}
								got, err := o.run(ctx)
								if err != nil {
									b.Fatal(err)
								}
								if mode != "build" && got != checksum {
									b.Fatalf("checksum %d, want %d", got, checksum)
								}
							}
							b.StopTimer()
							runtime.KeepAlive(o)
						})
					}
				}
			}
		}
	}
}
