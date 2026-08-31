package candidate

import "sync/atomic"

// metrics 是 Pipeline 的累计动作计数（atomic——反射轮与审批面并发安全；
// §10.3 并发上限由调用方控制，包内 -race 安全即可）。
type metrics struct {
	extracted         atomic.Uint64
	stored            atomic.Uint64
	duplicates        atomic.Uint64
	invalid           atomic.Uint64
	approved          atomic.Uint64
	rejected          atomic.Uint64
	rejectedUntrusted atomic.Uint64
}

// Metrics 是累计动作计数快照（D4 指标面，票 #92）：提炼率/批准率/撤销
// 率/污染拒绝率的数据源——本包只提供原始计数，率值计算归宿主或展示层。
type Metrics struct {
	// Extracted 是累计 extractor 返回候选数。
	Extracted uint64
	// Stored 是累计入库 Pending 候选数（提炼率 = Stored/Extracted）。
	Stored uint64
	// Duplicates 是累计去重丢弃数（归一包含判定，含 ID 撞车防御计数）。
	Duplicates uint64
	// Invalid 是累计形状丢弃数（空 Content / Structured 非法 JSON）。
	Invalid uint64
	// Approved 是累计批准数（Supersede 晋升；批准率 = Approved/
	// (Approved+Rejected)）。
	Approved uint64
	// Rejected 是累计否决数（Revoke——v1 撤销即否决；撤销率 =
	// Rejected/(Approved+Rejected)）。
	Rejected uint64
	// RejectedUntrusted 是累计否决中 TaintUntrustedExternal 候选数
	//（ASI06 污染闸实证；仅 untrusted-external 档计入——user-supplied
	// 被拒不算外部污染，票 #92 补强口径）。
	RejectedUntrusted uint64
}

// Metrics 返回累计动作计数快照（atomic 读，-race 安全）。计数只在动作
// 完整成功时累计：Extract 整轮成功累计整份 Report（错误中断的批次不计
// ——宿主重试成功后完整计一轮）；Approve/Reject 成功各 +1。
func (p *Pipeline) Metrics() Metrics {
	return Metrics{
		Extracted:         p.m.extracted.Load(),
		Stored:            p.m.stored.Load(),
		Duplicates:        p.m.duplicates.Load(),
		Invalid:           p.m.invalid.Load(),
		Approved:          p.m.approved.Load(),
		Rejected:          p.m.rejected.Load(),
		RejectedUntrusted: p.m.rejectedUntrusted.Load(),
	}
}
