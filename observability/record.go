// Package observability 是 pulse v2 的正式观测包：SpringBoot 式装配
// 日志的最小实现。
//
// 分层纪律（方案 A）：本包只 import kernel，绝不 import llm/loop/flow。
// 它只认识两样东西——kernel 发出的装配期事实（typed 事件），以及
// 下游的 Sink 出口。运行期业务事件（token 计数、HITL 结果、节点耗时）
// 由伴生装配层 observability/bridge 订阅 llm/loop 公开事件后折进
// Record 信封写同一 Sink——认识业务组件的适配器单独成包，依赖方向
// 不变、无环（见 observability-v1-design.md §2 / §9）。
//
// 接入姿态（v1 仅一种）：
//
//	host := kernel.New()
//	// 必须最先 Use：完整装载轨迹的前提（kernel 事件不回放）
//	if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil { ... }
//	// 此后其它插件正常 Use，每次状态迁移都会进入 Sink
package observability

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"time"
)

// Source 标识记录来源层。正式包只会产生 kernel 来源；其余来源由
// 装配层桥产生，本包不校验枚举。
type Source string

const (
	SourceKernel Source = "kernel" // fiber_state / loader_action（Bootstrap 产生）
	// SourceAdapter 是运行期观测适配来源：各包 Observe 折叠与宿主经
	// Collector 的直写共用（trace_id 必填）。历史名 SourceBridge——
	// 伴生桥时代（#126 已废）的常量名，折叠主体改为各包自身后原名
	// 名不副实；字面值保持 "bridge" 不变，兼容既有日志解析。
	SourceAdapter Source = "bridge"
)

// Event 名称常量。仅列正式包自己产出的事件；桥的事件名自定义，
// 建议保持 <组件>.<事实> 的点分约定以便日志聚合分组。
const (
	EventFiberState   = "pulse.kernel.fiber_state"
	EventLoaderAction = "pulse.kernel.loader_action"
	EventHostReady    = "observability.host_ready"
)

// Record 是观测信封：通用可空字段 + 装配专用具名段 + Attrs 开放段。
//
// 隐私边界：没有 map[string]any 或任意对象注入口——Attrs 的值域经
// 泛型 Set/Get 锁死在标量（string/int64/float64/bool），Message 切片、
// 附件字节、思维链等 payload 结构在类型上就无法进入；「把 payload
// 塞进一个标量值」属于蓄意行为，防线是 key 自述意图 + Sink 侧
// redact 钩子（宿主实现可拒绝敏感 key / 截断超长 / 限条数）。
//
// 字段填充规则：
//   - kernel 装配记录（Bootstrap 产生）：TraceID/Duration/Attrs 为零值
//   - 桥记录（observability/bridge 产生）：填 TraceID/Duration/Status/
//     Attrs（key 契约见 llm/loop/flow 各包的 Attr* 常量，如
//     llm.model、loop.tool、flow.node）；不要再扩本结构
type Record struct {
	Time    time.Time
	HostID  string
	TraceID string // 装配期为空；运行期桥必填
	Source  Source
	Event   string

	// Duration 仅运行期指标记录使用；装配迁移恒为 0。
	Duration time.Duration
	// Status 是结果状态字符串（finish reason / completed|failed 等）；
	// 迁移事件为空（状态已在 From/To）。
	Status string

	// Err 非 nil 表示该记录关联一次失败。
	Err error

	// ---- 以下为装配期专用字段，桥记录留零值 ----

	FiberName  string // fiber_state: 实例诊断名
	From, To   string // fiber_state: 状态名（FiberState.String()）
	LoaderKind string // loader_action: mount|unmount|recreate|disable
	EntryID    string // loader_action: 条目 ID
	PluginName string // loader_action: plugin 注册名

	// Attrs 是产生方自定义的标量 kv（运行期桥与业务插件使用，
	// 经 Set/Get 写读）。出口实现应按 key 排序输出以获得确定性。
	// 引用语义见 Sink 接口契约：产出方 Write 后不再修改，Sink 只读。
	Attrs Attrs
}

// AttrValue 是 Attrs 的值域约束：仅标量（含底层类型命中约束的命名
// 类型）。它只能出现在 Set/Get 的泛型约束位置——Go 的 union 接口不能
// 作普通变量类型，这恰好堵死「绕过约束直接构造值」的口子。
type AttrValue interface {
	~string | ~int64 | ~float64 | ~bool
}

// Attrs 是产生方自定义的标量 kv。key 约定 <组件>.<字段> 点分
// （如 llm.model、loop.tool、flow.node），各组件独立 key 空间。
//
// 零值可用。写入经泛型 Set（就地），读取经泛型 Get；Range 供出口
// 无序遍历，MarshalJSON 按 key 排序输出。并发语义与 Record 一致：
// 产生方单 goroutine 填充，进入 Sink 后只读。
type Attrs struct {
	m map[string]attrScalar
}

type attrKind uint8

const (
	attrString attrKind = iota
	attrInt
	attrFloat
	attrBool
)

// attrScalar 是标量的定长联合表示：kind 决定哪个字段有效。
type attrScalar struct {
	kind attrKind
	s    string
	i    int64
	f    float64
	b    bool
}

// native 把标量还原为基础类型值（string/int64/float64/bool），
// 供 slog 等出口按原生类型输出。
func (x attrScalar) native() any {
	switch x.kind {
	case attrString:
		return x.s
	case attrInt:
		return x.i
	case attrFloat:
		return x.f
	case attrBool:
		return x.b
	}
	return nil
}

// Set 把标量键值就地写入 a（零值 Attrs 可用，同名覆盖）。T 的类型集
// 见 AttrValue——[]byte、struct、slice、任意对象在类型上就无法进入，
// 内容载荷（prompt、消息、思维链）不能以 kv 形式进入观测记录，
// 这是本包隐私边界的类型部分。
func Set[T AttrValue](a *Attrs, key string, val T) {
	if a.m == nil {
		a.m = make(map[string]attrScalar)
	}
	var x attrScalar
	switch v := any(val).(type) {
	case string:
		x = attrScalar{kind: attrString, s: v}
	case int64:
		x = attrScalar{kind: attrInt, i: v}
	case float64:
		x = attrScalar{kind: attrFloat, f: v}
	case bool:
		x = attrScalar{kind: attrBool, b: v}
	default:
		// 底层类型命中约束的命名类型（如 type Model string）。
		switch rv := reflect.ValueOf(val); rv.Kind() {
		case reflect.String:
			x = attrScalar{kind: attrString, s: rv.String()}
		case reflect.Int64:
			x = attrScalar{kind: attrInt, i: rv.Int()}
		case reflect.Float64:
			x = attrScalar{kind: attrFloat, f: rv.Float()}
		case reflect.Bool:
			x = attrScalar{kind: attrBool, b: rv.Bool()}
		default:
			return // 约束保证不可达
		}
	}
	a.m[key] = x
}

// Get 读取标量值：缺失或与 T 底层类型不符返回零值与 false。
func Get[T AttrValue](a Attrs, key string) (T, bool) {
	var zero T
	x, ok := a.m[key]
	if !ok {
		return zero, false
	}
	if out, ok := x.native().(T); ok {
		return out, true
	}
	// T 是命中约束的命名类型（如 type Model string）时走反射转换；
	// T 的底层 kind 必须与存储 kind 一致，否则类型不符。
	var want reflect.Kind
	switch x.kind {
	case attrString:
		want = reflect.String
	case attrInt:
		want = reflect.Int64
	case attrFloat:
		want = reflect.Float64
	case attrBool:
		want = reflect.Bool
	default:
		return zero, false
	}
	if tp := reflect.TypeFor[T](); tp.Kind() != want {
		return zero, false
	}
	out := reflect.New(reflect.TypeFor[T]()).Elem()
	switch x.kind {
	case attrString:
		out.SetString(x.s)
	case attrInt:
		out.SetInt(x.i)
	case attrFloat:
		out.SetFloat(x.f)
	case attrBool:
		out.SetBool(x.b)
	}
	got, ok := out.Interface().(T)
	return got, ok
}

// Len 返回条数。
func (a Attrs) Len() int { return len(a.m) }

// Range 无序遍历键值对；val 已还原为基础类型（string/int64/float64/bool）。
// 出口实现如需确定性输出，请按 key 排序（参考 SlogSink / MarshalJSON）。
func (a Attrs) Range(fn func(key string, val any)) {
	for k, x := range a.m {
		fn(k, x.native())
	}
}

// sortedKeys 返回按键排序的全部 key（同包出口用）。
func (a Attrs) sortedKeys() []string {
	keys := make([]string, 0, len(a.m))
	for k := range a.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MarshalJSON 按 key 排序输出为对象（确定性）；空 Attrs 输出 {}。
func (a Attrs) MarshalJSON() ([]byte, error) {
	keys := a.sortedKeys()
	buf := bytes.NewBuffer(make([]byte, 0, 16*len(keys)+2))
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(a.m[k].native())
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Sink 是记录出口。实现必须并发安全且不得长时间阻塞调用方
// （Emit 处于 kernel 派发路径上）。
//
// 契约：无 context.Context——kernel Emit 路径不带 ctx；需要截止时间
// 的导出器自行持有内部队列，不把阻塞回传到派发路径。
//
// 引用语义契约（Attrs 是 Record 第一个引用类型字段，MultiSink 会把
// 同一个 Attrs 递给多个 Sink）：
//   - 产出方每次构造独立 Attrs，Write 返回后不再修改该 Record；
//   - Sink 实现不得修改收到的 Record 及其 Attrs（只读消费）；
//   - 异步导出器（队列化后再落盘/上报）必须自行拷贝所需字段后再持有。
type Sink interface {
	Write(r Record)
}

// stampTime 在 Time 为零时补 wall clock，避免调用方漏填导致死字段。
func stampTime(r Record) Record {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	return r
}

// MultiSink 扇出到多个 Sink；nil 成员跳过。
type MultiSink []Sink

// Write 实现 Sink。Time 由叶子 Sink（SlogSink / MemorySink）补齐。
func (s MultiSink) Write(r Record) {
	for _, sink := range s {
		if sink != nil {
			sink.Write(r)
		}
	}
}
