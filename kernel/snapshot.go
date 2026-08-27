package kernel

// FiberSnapshot 是插件实例某一刻的只读诊断视图：横幅与外部观测
// 只拿到值拷贝，拿不到能 Close 或触碰内部锁的活指针。
type FiberSnapshot struct {
	// Name 是稳定诊断名：Loader 装载 = Entry.ID；裸 Use = 类型名#序号。
	Name string
	// State 是当前生命周期状态。
	State FiberState
	// Err 是装载失败原因（仅 Failed 非 nil）。
	Err error
	// WaitingFor 是未满足的依赖服务名拷贝（按 Inject 声明序）；
	// 仅 Inactive/Failed 且依赖缺失时非空，Active 为 nil。
	WaitingFor []string
}

// FiberSnapshots 从 root 起整棵作用域树收集全部插件实例快照
// （含裸 Use 实例与各 Loader 条目），锁内逐实例取值、输出为拷贝——
// 调用方无法通过结果 Close 实例或读内部可变状态。
//
// 这正是 observability.Bootstrap 横幅的数据源：Bootstrap 在自己的
// 私有子 ctx 上 Apply，业务插件是兄弟子树；若只 dump 本层 fibers，
// 横幅对业务插件必然失明。必须走 root 全树遍历。
func (c *Context) FiberSnapshots() []FiberSnapshot {
	if c == nil {
		return nil
	}
	root := c.root()

	root.mu.Lock()
	fibers := make([]*Fiber, len(root.fibers))
	copy(fibers, root.fibers)
	children := make([]*Context, len(root.children))
	copy(children, root.children)
	root.mu.Unlock()

	out := make([]FiberSnapshot, 0, len(fibers))
	for _, f := range fibers {
		out = append(out, f.snapshot())
	}
	for _, kid := range children {
		out = append(out, kid.fiberSnapshotsFrom()...)
	}
	return out
}

// fiberSnapshotsFrom 收集以 c 为根的子树快照（内部递归辅助，不导出：
// 外部只需要全树视图；暴露子树枚举会扩大公共面且无使用场景。）
func (c *Context) fiberSnapshotsFrom() []FiberSnapshot {
	c.mu.Lock()
	fibers := make([]*Fiber, len(c.fibers))
	copy(fibers, c.fibers)
	children := make([]*Context, len(c.children))
	copy(children, c.children)
	c.mu.Unlock()

	out := make([]FiberSnapshot, 0, len(fibers))
	for _, f := range fibers {
		out = append(out, f.snapshot())
	}
	for _, kid := range children {
		out = append(out, kid.fiberSnapshotsFrom()...)
	}
	return out
}

// snapshot 取单实例只读视图（锁内取值，WaitingFor 输出拷贝）。
func (f *Fiber) snapshot() FiberSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := FiberSnapshot{
		Name:  f.name,
		State: f.state,
		Err:   f.applyErr,
	}
	if waiting := f.unsatisfiedLocked(); len(waiting) > 0 {
		s.WaitingFor = append([]string(nil), waiting...)
	}
	if s.Name == "" {
		s.Name = "fiber#unknown"
	}
	return s
}
