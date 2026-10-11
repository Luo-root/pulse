package yaml

import (
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
)

// 校验图必须与真图**同额度**，否则流式那道判据在装配期是空的。
//
// 这条只能在包内测：`checkGraph` 未导出，而公开面上也**看不见**这件事——`Produce` /
// `Consume` / `Tee` 是 Go 糖，而 YAML 的工厂拿到的是 `*RunCtx`，装配期连 `*Graph` 都
// 摸不到，所以声明式那条路今天表达不了流。它是给「YAML 也能声明流」那天留的闸门，
// 也钉住「校验图 = 真图的选项」这件事。
//
// 额度不足时拒在**装配那一刻**（`Produce` 自己的快速检查就按容量判），不是等 `Validate`。
func TestCheckGraphCarriesCapacity(t *testing.T) {
	build := func(maxRunning int) (*pulse.Graph, error) {
		g, err := checkGraph("chk", LoadOptions{MaxRunning: maxRunning})
		if err != nil {
			return nil, err
		}
		out := pulse.NewKey[<-chan int]("chk.out")
		if err := pulse.Produce(g, "prod", out,
			func(rc *pulse.RunCtx, send func(int) error) error { return nil }); err != nil {
			return nil, err
		}
		if err := pulse.Consume(g, "cons", out,
			func(rc *pulse.RunCtx, v int) error { return nil }); err != nil {
			return nil, err
		}
		return g, nil
	}

	// 这条路的两端必须同时活着（2 个名额）；额度 1 养不起 → 装配期就该拒。
	// 校验图没有额度时的旧行为：这里什么都不报，`Validate` 也过——一路装到最后交出去。
	if _, err := build(1); err == nil || !strings.Contains(err.Error(), "cannot host this stream") {
		t.Fatalf("额度 1 养不起两端同时活着的流，装配期必须拒：%v", err)
	}

	// 对照一：额度够 → 装配与 `Validate` 都通过（否则上面那条可能只是「什么都拒」）。
	roomy, err := build(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := roomy.Validate(); err != nil {
		t.Fatalf("额度 2 装得下这条流，不该拒：%v", err)
	}

	// 对照二：不给额度（<=0 = 不限）时也不该拒。
	free, err := build(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := free.Validate(); err != nil {
		t.Fatalf("不限额度时不该拒：%v", err)
	}
}
