module github.com/Luo-root/pulse

go 1.25.0

require (
	github.com/anthropics/anthropic-sdk-go v1.66.0
	github.com/modelcontextprotocol/go-sdk v1.7.0
	github.com/openai/openai-go/v3 v3.52.0
	golang.org/x/net v0.58.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.57.0
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// retract：v1.x 是 v1 旧架构（components/ 树，无 kernel/）时期的 tag，GitHub 上已删除，
// 但 module proxy 永久保留已服务过的版本——且 v1.1.0 > v0.2.4 恒成立，@latest 因此永远
// 解析到不含本仓库包的 v1.1.0（`go get .../pulse/kernel` 报 "does not contain package"；
// `go get .../pulse` 静默装到旧架构代码）。retract 声明只从「最高版本」的 go.mod 读取，
// 所以连同承载本声明的 v1.1.1 一起撤回——否则 @latest 会落在同样没有 kernel/ 的 v1.1.1。
// 撤回后 @latest 回落到 v0.x 线的最高版本。
retract (
	v1.1.1
	v1.1.0
	v1.0.1
	v1.0.0
)
