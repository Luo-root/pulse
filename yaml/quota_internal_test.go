package yaml

import (
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
)

// TestLoadOptionsQuotaPrecedence 钉住 `LoadOptions` 两个额度入口的**实际**优先级
// （评审在 #305 上点的那条：注释说的与代码做的对不上）。
//
// 规则就是代码里那两条 `if opts.MaxRunning > 0`：
//
//   - `MaxRunning > 0`：它拼在 `Graph` 那批的最后 ⇒ **压过**里面可能写的
//     `WithMaxRunning`（哪怕比它更小）；
//   - `MaxRunning <= 0`（含默认）：**不动** `Graph` 里那份——`int` 的零值分不出
//     「没写」与「写了 0」，所以「整棵树不限」这件事只能靠「别在 `Graph` 里放」
//     表达，不能靠「把 MaxRunning 写成 0」。
//
// 判据借 `checkGraph`（与真图**同一次推导**）+ `Produce` 的装配期快速检查：一条流的
// 两端要同时活着，容量 1 当场被拒、容量 ≥2 放行——于是「有效容量是几」看得见，不用
// 去数并发峰值。
func TestLoadOptionsQuotaPrecedence(t *testing.T) {
	limit := func(n int) []pulse.Option { return []pulse.Option{pulse.WithMaxRunning(n)} }

	cases := []struct {
		name         string
		opts         LoadOptions
		wantRejected bool
	}{
		{"都不写 = 不限", LoadOptions{}, false},
		{"MaxRunning=1", LoadOptions{MaxRunning: 1}, true},
		{"MaxRunning=2", LoadOptions{MaxRunning: 2}, false},
		{"只写 Graph 里 WithMaxRunning(1)", LoadOptions{Graph: limit(1)}, true},
		{"Graph(1) + MaxRunning=0 ⇒ 仍是 1（零值分不出「没写」）", LoadOptions{Graph: limit(1)}, true},
		{"Graph(1) + MaxRunning=2 ⇒ 2 生效", LoadOptions{Graph: limit(1), MaxRunning: 2}, false},
		{"Graph(3) + MaxRunning=1 ⇒ 1 生效（更小也压）", LoadOptions{Graph: limit(3), MaxRunning: 1}, true},
	}

	src := pulse.NewKey[<-chan int]("quota.probe")
	need2 := func(rc *pulse.RunCtx, send func(int) error) error { return nil }

	for _, c := range cases {
		g, err := checkGraph("quota-probe", c.opts)
		if err != nil {
			t.Fatalf("%s: checkGraph: %v", c.name, err)
		}
		err = pulse.Produce(g, "p", src, need2)
		if rejected := err != nil; rejected != c.wantRejected {
			t.Fatalf("%s: 装配期 %v，want rejected=%v（err=%v）", c.name, rejected, c.wantRejected, err)
		}
		if err != nil && !strings.Contains(err.Error(), "cannot host this stream") {
			t.Fatalf("%s: 拒的理由不对：%v", c.name, err)
		}
	}
}
