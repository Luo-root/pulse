package builtins

import (
	"fmt"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/toolset"
)

// env 是一组工具共享的运行时。
type env struct {
	opt     Options
	tracker *readTracker
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
	e := &env{opt: opt, tracker: newReadTracker()}

	want := map[string]bool{}
	for _, n := range opt.Enabled {
		want[n] = true
	}
	allow := func(name string) bool {
		return len(want) == 0 || want[name]
	}

	type item struct {
		name string
		reg  toolset.Registration
	}
	var items []item
	add := func(name string, r toolset.Registration) {
		if !allow(name) {
			return
		}
		r.Source = opt.SourcePrefix + "." + name
		items = append(items, item{name: name, reg: r})
	}

	add("read", e.regRead())
	add("ls", e.regLS())
	add("glob", e.regGlob())
	add("grep", e.regGrep())
	add("exec", e.regExec())
	add("edit", e.regEdit())
	add("write", e.regWrite())
	add("apply_patch", e.regApplyPatch())
	add("web_fetch", e.regWebFetch())
	add("web_search", e.regWebSearch())
	add("question", e.regQuestion())

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
	return func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}, nil
}
