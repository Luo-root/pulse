module github.com/Luo-root/pulse

go 1.25.0

require gopkg.in/yaml.v3 v3.0.1

// retract：v1.x 是 v1 旧架构时期的 tag，GitHub 上已删除，但 module proxy 永久保留
// 已服务过的版本——且 v1.1.0 > v0.2.4 恒成立，@latest 因此永远解析到不含本仓库
// 当前包的 v1.1.0（`go get .../pulse` 静默装到旧架构代码）。retract 声明只从
// 「最高版本」的 go.mod 读取，所以连同承载本声明的 v1.1.1 一起撤回——否则
// @latest 会落在同样没有当前包的 v1.1.1。撤回后 @latest 回落到 v0.x 线的最高版本。
retract (
	v1.1.1
	v1.1.0
	v1.0.1
	v1.0.0
)
