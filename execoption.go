package pulse

// 本文件是**一次运行的运行期配置**：`WithExec*` 一族覆盖这张定义声明的默认值。
//
// 分工：`Option`（`With*`）是**定义**的声明，每次运行都生效；`ExecOption`（`WithExec*`）
// 只作用于**这一次**运行。优先级从高到低：本次运行 > 定义声明 > 父执行环境。

// ExecOption 配置**某一次运行**，在 `NewExecution` / `Run` 时给。
type ExecOption func(*execConfig)

// execConfig 是一次运行的覆盖项；每项都带一个「给了没有」的标记，
// 否则零值（nil 出口、0 名额）分不出「没给」与「给了零值」。
type execConfig struct {
	observer    Observer
	observerSet bool
	maxRunning  int
	quotaSet    bool
	aspects     []Aspect
	checkpoint  *checkpointConfig
}

// WithExecObserver 让**这一次运行**用这个观察者，压过定义上 `WithObserver` 声明的那一个。
//
// 它与定义声明的关系是「覆盖」而不是「叠加」：要组合两个出口就自己用 `MultiObserver`
// 拼好再传进来。子图那一层没声明出口时会继承父执行环境的（这次覆盖后的）出口——
// 继承走的是**有效值**，不是定义上的声明。
func WithExecObserver(o Observer) ExecOption {
	return func(c *execConfig) {
		c.observer = o
		c.observerSet = true
	}
}

// WithExecMaxRunning 让**这一次运行**用这个名额上限，压过定义上 `WithMaxRunning` 声明的那个。
//
// `n<=0` = 这一次不限（与定义声明同一条规则：`<=0` 也是「声明过」，不是「没声明」）。
// 这次声明的名额同样会往下继承给子执行环境（同一棵执行树共用一个计数）。
func WithExecMaxRunning(n int) ExecOption {
	return func(c *execConfig) {
		c.maxRunning = n
		c.quotaSet = true
	}
}

// WithExecAspects 给**这一次运行**追加图级切面：它们排在定义声明的那批**之后**
// （更靠内），并照样继承给子执行环境。
//
// **是追加不是替换**：替换会让「这一次多加一个超时」变成「把这张图的图级切面全撤掉」，
// 那不是调用方想要的。要撤掉继承来的那批，用定义上的 `WithoutAspects()`。
func WithExecAspects(as ...Aspect) ExecOption {
	return func(c *execConfig) { c.aspects = append(c.aspects, as...) }
}
