# 正式版本 SQLite 可靠确认与恢复重测

测量日期：2026-10-23。实际依赖：Core v0.5.0、Durable v0.1.0、sqlitestore v0.1.0，Go 1.26.0。

`summary.csv` 和 `identity.json` 由仓库外保留的原始采样生成。本目录仅保留精简统计与公开模块身份，不含数据库、二进制、机器路径或原始运行日志。

有效171条：102 ok、4 deadline、4 hard_timeout、15 not_equivalent、46 not_repeated。另45条 Temporal 错误使用 checkpoint 模式的记录属于采样配置诊断，保留在外部证据包、排除性能统计，不能归为引擎性能失败。整体状态 COMPLETED_WITH_ERRORS；36/36恢复完成，不能将其泛化为全部规模通过。

原始JSONL SHA256：
- checkpoint（135行，含45条配置诊断）：d59f5116fe41cc87308d5be6724df55d18c679f4898f875a211d9325e436269d
- temporal-native（45行）：610bb237276d167a968176d8fb33750538269a1cb8571786f511423a0d37e7b3
- recovery（36行）：d0485cee9514d4d3f888b5a60eb9e0c5af5907202f0e88d774a264c53ba54d80

统计口径与结果见[统一生产性能对比](../../../site/reference/production-comparison.md)。
