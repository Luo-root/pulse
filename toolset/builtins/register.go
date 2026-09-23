package builtins

import (
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/toolset"
)

// env 是一组工具共享的运行时。
type env struct {
	opt     Options
	tracker *readTracker
	jobs    *jobTable
}

// Register 将 P0 基础工具登记到 reg；返回统一 dispose（撤销本批登记）。
func Register(scope *kernel.Context, reg *toolset.Registry, opt Options) (dispose func(), err error) {
	if scope == nil {
		return nil, fmt.Errorf("builtins: nil scope")
	}
	if reg == nil {
		return nil, fmt.Errorf("builtins: nil registry")
	}
	opt, err = opt.withDefaults()
	if err != nil {
		return nil, err
	}
	opt.HTTPClient = guardedClient(opt.HTTPClient, opt.BlockPrivate)
	if opt.Searcher == nil {
		opt.Searcher = newDDGSearcher(opt.HTTPClient, opt.SearchEndpoint)
	}
	e := &env{opt: opt, tracker: newReadTracker(), jobs: newJobTable(opt.MaxJobs, opt.MaxExecBytes)}

	// 先按「全部已实现工具」构造登记项，再校验 Enabled、最后过滤：校验与
	// 登记共用同一份事实源，不会出现「工具名清单两份、改一处漏一处」。
	type item struct {
		name string
		reg  toolset.Registration
	}
	var all []item
	add := func(name string, r toolset.Registration) {
		r.Source = opt.SourcePrefix + "." + name
		all = append(all, item{name: name, reg: r})
	}

	add("read", e.regRead())
	add("ls", e.regLS())
	add("glob", e.regGlob())
	add("grep", e.regGrep())
	add("exec", e.regExec())
	add("job_output", e.regJobOutput())
	add("job_kill", e.regJobKill())
	add("edit", e.regEdit())
	add("write", e.regWrite())
	add("apply_patch", e.regApplyPatch())
	add("web_fetch", e.regWebFetch())
	add("web_search", e.regWebSearch())
	add("question", e.regQuestion())

	// Enabled 未知名一律 fail-loud：原先未知名只是让过滤集合命中不到任何
	// 登记（静默注册 0 个工具、err=nil），错误要拖到运行期才以模型可见的
	// `unknown tool` 文本露出，或者永不暴露（#214）。
	want := map[string]bool{}
	if len(opt.Enabled) > 0 {
		known := make(map[string]bool, len(all))
		names := make([]string, 0, len(all))
		for _, it := range all {
			known[it.name] = true
			names = append(names, it.name)
		}
		var unknown []string
		for _, n := range opt.Enabled {
			if !known[n] {
				unknown = append(unknown, n)
			}
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("builtins: Options.Enabled has unknown tool name(s): %s (valid: %s)",
				strings.Join(unknown, ", "), strings.Join(names, ", "))
		}
		for _, n := range opt.Enabled {
			want[n] = true
		}
	}

	items := make([]item, 0, len(all))
	for _, it := range all {
		if len(want) == 0 || want[it.name] {
			items = append(items, it)
		}
	}

	disposers := make([]func(), 0, len(items))
	rollback := func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}
	for _, it := range items {
		d, err := reg.Register(scope, it.reg)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("builtins: register %s: %w", it.name, err)
		}
		disposers = append(disposers, d)
	}
	// job 清理走独立 Effect：scope.Dispose（宿主忘记显式 dispose）时也能杀活 job。
	// 幂等；显式 dispose 闭包里同样杀，两条路都覆盖。
	if _, err := scope.Effect(func() (func(), error) {
		return e.jobs.killAll, nil
	}); err != nil {
		rollback()
		return nil, fmt.Errorf("builtins: register job cleanup: %w", err)
	}
	return func() {
		e.jobs.killAll()
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}, nil
}
