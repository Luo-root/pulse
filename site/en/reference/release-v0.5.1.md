# v0.5.1 preparation (unreleased)

**Status: v0.5.1 is not released.** These preparation notes serve [Issue #502](https://github.com/Luo-root/pulse/issues/502); they do not establish an available tag, GitHub Release, or downloadable module. Default installation continues to pin published v0.5.0 until v0.5.1 is actually available.

## Compatibility impact: minimum Go version

Current source and planned v0.5.1 raise the minimum Go version from v0.5.0's **Go 1.25.0+** to **Go 1.26.0+**, aligned with Durable. Both the root module and the standalone DAG benchmark module declare `go 1.26.0`.

This is a toolchain compatibility change, not full backward compatibility: environments pinned to Go 1.25 with automatic toolchain selection disabled cannot directly build the new version. Select Go 1.26.0 or later on developer machines and CI, and update application go.mod/go.sum as needed.

This adjustment does not change public APIs, workflow runtime semantics, checkpoint bridge contracts, or Core / Durable responsibilities; it is not an execution-engine feature redesign. The original toolchain requirements in v0.5.0 release and migration notes, historical performance reports, and recorded test toolchains remain unchanged and are not Go 1.26 validation results.

## Installation and acceptance

- Default installation remains `go get github.com/Luo-root/pulse@v0.5.0`, whose original minimum is Go 1.25.0+.
- To try this unreleased change, select matching local source through the [installation guide](../guide/installation.md); do not assume main already contains it.
- Before publication, run build, vet, race, the documentation signature gate, and the site build on the final candidate with Go 1.26. Actual execution records establish results; these notes do not claim they have already passed.
- New performance results require actual remeasurement with the corresponding toolchain; this upgrade does not rewrite historical data.
- The maintainer decides merging, tagging, and Release creation; preparation notes do not publish a release.
