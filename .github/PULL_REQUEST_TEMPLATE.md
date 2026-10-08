<!--
Read CONTRIBUTING.md before filling this in — it explains what "…写清什么" means in practice.
填之前请先读 CONTRIBUTING.md（中英双语同一文件，中文在后半）。
Write in whichever language you prefer; every heading is given in both. If a section is genuinely
empty, write "无 / none" rather than deleting it — a reviewer needs to know it was considered.
-->

## 关联 Issue / Linked issue

<!-- 例：见 #192。只有这个 PR 真的把票干完时才用自动关闭关键字（closes #N）。 -->

## 改了什么 / What changed

## 新增 / What's new

## 影响范围 / Impact

<!-- 谁受影响；有没有动冻结契约（slots 三态 / Aspect 形状与门闩 / 哨兵错误 /
     Observer 回调次数契约 / 六条编码原语与 LineRenderer）；有没有破坏性。 -->

## 测试 / Testing

<!-- 具体命令 + 观察到的结果。「CI 绿了」是结果，不是测试方案。 -->

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `"$(go env GOROOT)/bin/gofmt" -l $(git ls-files '*.go')` 输出为空 / empty output
- [ ] `go test -race -count=1 ./...`

## Review 关注点 / Review focus

<!-- 希望别人重点看哪里，以及你自己没把握的地方。 -->

## 自审 / Self-review

- [ ] 我像 reviewer 一样读完了自己的 diff / I read my own diff the way a reviewer would
- [ ] 没有提交任何凭据（`.env`、token、私钥）/ No credentials committed
- [ ] 行为有变化时，文档与设计文档已同步 / Docs updated if behavior changed
