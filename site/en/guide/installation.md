# Installation and versions

Use this page to choose a version matching the documentation, create a Go module, and inspect the dependency you actually use. Then continue to the [quick start](quickstart.md).

## Requirements

Requires **Go 1.25.0 or later**. Check your toolchain:

```sh
go version
```

The core package's dependency closure contains only the standard library. `pulse/yaml` additionally uses `gopkg.in/yaml.v3`; `pulse/observe` adds no third-party runtime dependency.

## Choose a version

| Version | When to choose it | Documentation |
| --- | --- | --- |
| Released tag `v0.4.1` | You require a released version and will use that version's API | [v0.4.1 source and README](https://github.com/Luo-root/pulse/tree/v0.4.1) |
| Public commit `1f67045d6c42` | You want to follow these guides for reusable Graphs, Executions, static subgraphs, and the checkpoint bridge | This site's guides and reference |
| `main` | You are contributing and accept subsequent changes | [main source](https://github.com/Luo-root/pulse) |

**This site describes the development API, not v0.4.1.** Tutorials pin a public commit already merged into main, rather than allowing an unversioned install to select the older tag. Go resolves the commit to a pseudo-version in `go.mod`; that is a normal module version format.

Unreleased target versions become available only when the maintainer creates their tags / releases. During 0.x, breaking changes require a minor release. Read the [migration notes](../reference/migration.md) before upgrading.

## Create an example module

In the directory where you want the example:

```sh
mkdir pulse-example
cd pulse-example
go mod init example.com/pulse-example
go get github.com/Luo-root/pulse@1f67045d6c42
```

Save the [quick-start program](quickstart.md) as `main.go`, then run:

```sh
go run .
```

Commit your application's `go.mod` and `go.sum` so that teammates and CI use the same dependencies.

## Check the dependency

```sh
go list -m github.com/Luo-root/pulse
```

The output should contain the pseudo-version for `1f67045d6c42`. If it shows v0.4.1, run the pinned installation command above again.

An output containing `=>` indicates a local replacement in the module or `go.work`. Inspect the application's module and workspace configuration to check whether it intentionally selects another source tree. To test public dependencies independently, set `GOWORK=off` for that process rather than changing global Go settings; environment-variable syntax depends on your shell.

## Optional packages

All packages belong to the same Go module. Once it is installed, these import paths are available:

```go
import (
	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)
```

This is a list of available imports. Keep only the packages your program uses, because Go rejects unused imports. See the [package reference](../packages/index.md) to choose and use the optional packages.

## Troubleshooting

**The compiler reports mismatched `New` or `Run` arguments.** Check the version with `go list -m`. Older tags and this development API have different object models; mixing their documentation produces signature errors.

**The toolchain cannot be downloaded.** Install or select Go 1.25+, or configure toolchain and module download access for your environment. Running Pulse itself requires no online service or credentials.

**You need recovery across process restarts.** Read the [checkpoint bridge](../reference/checkpoint.md) and follow the separate [Durable repository](https://github.com/Luo-root/pulse-durable)'s installation instructions. Pulse core does not store execution history.

Next: [Run your first workflow](quickstart.md).
