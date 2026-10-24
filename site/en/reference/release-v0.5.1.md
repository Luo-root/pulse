# Pulse v0.5.1 release notes

**Status: v0.5.1 is released.** This page records the Go 1.26+ baseline, compatibility impact, and the formal SQLite measurement entry. Install it with `go get github.com/Luo-root/pulse@v0.5.1`.

## Compatibility impact: minimum Go version

v0.5.1 raises the minimum Go version from v0.5.0's **Go 1.25.0+** to **Go 1.26.0+**, aligned with Durable. Both the root module and the standalone DAG benchmark module declare `go 1.26.0`.

This is a toolchain compatibility change, not full backward compatibility: environments pinned to Go 1.25 with automatic toolchain selection disabled cannot directly build the new version. Select Go 1.26.0 or later on developer machines and CI, and update application go.mod/go.sum as needed.

This adjustment does not change public APIs, workflow runtime semantics, checkpoint bridge contracts, or Core / Durable responsibilities; it is not an execution-engine feature redesign. The original toolchain requirements in v0.5.0 release and migration notes, historical performance reports, and recorded test toolchains remain unchanged and are not Go 1.26 validation results.

## Installation and acceptance

- Official installation: `go get github.com/Luo-root/pulse@v0.5.1`; the minimum is Go 1.26.0+.
- v0.5.0 remains installable from its original tag with its original Go 1.25.0+ requirement; do not apply that historical toolchain requirement to v0.5.1.
- The final candidate completed Go 1.26 build, vet, race, documentation-site build, and CI checks; the site build retains a chunk-size warning that does not prevent generation.
- The current formal measurement is [formal-release SQLite and recovery measurements](./sqlite-formal-20261023.md). Read its `COMPLETED_WITH_ERRORS` status as recorded; it does not replace the complete historical evidence or turn unmeasured points into successes.
- Pulse Core and Pulse Durable are released independently; this page describes Core v0.5.1 only.
