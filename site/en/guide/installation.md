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
| `v0.5.0` release preparation | Evaluate object-model, managed-stream, and recovery-bridge changes; do not install this version before its tag exists | [Release notes](../reference/release-v0.5.0.md) · [Migration](../reference/migration.md) |
| `main` | You want the development API already merged into the main branch | [main source](https://github.com/Luo-root/pulse) |
| Local source checked out with these docs | You are trying unmerged fixes or contributing | These guides and reference; use the local replacement below |

**This site describes the development API, not v0.4.1.** `go get …@main` resolves the current main branch to a concrete pseudo-version in `go.mod`. Once that file is committed, teammates use that resolved version; running the application does not automatically update it.

`main` includes managed Stream, live Sub, message attempts, and incremental checkpoints, so you can install it directly to run the corresponding examples. Use the local replacement below when trying an unmerged change. The older public snapshot `1f67045d6c42` lacks the sugar constructors' `description` argument and cannot compile the updated sugar examples.

Unreleased target versions become available only when the maintainer creates their tags / releases. During 0.x, breaking changes require a minor release. Read the [migration notes](../reference/migration.md) before upgrading.

## Create an example module

In the directory where you want the example:

```sh
mkdir pulse-example
cd pulse-example
go mod init example.com/pulse-example
go get github.com/Luo-root/pulse@main
```

Save the [quick-start program](quickstart.md) as `main.go`, then run:

```sh
go run .
```

Commit your application's `go.mod` and `go.sum` so that teammates and CI use the same dependencies.

## Try unmerged source

Obtain a checkout containing the changes you need and provided with these docs. The following assumes its directory is named `pulse`, next to the `pulse-example` module. Set a local replacement inside the example module:

```sh
go mod edit -replace=github.com/Luo-root/pulse=../pulse
```

After saving the example's `main.go`, run:

```sh
go mod tidy
go list -m github.com/Luo-root/pulse
go run .
```

An output containing `=> ../pulse` confirms that this local source is selected. Even if the left-hand version in `go.mod` is older, the replacement determines the implementation. Adjust the relative path for your layout. Do not commit a personal-machine path into a production application. After the changes merge, remove the replacement, select a public commit containing them, and commit the resolved version:

```sh
go mod edit -dropreplace=github.com/Luo-root/pulse
go get github.com/Luo-root/pulse@main
```

## Check the dependency

```sh
go list -m -json github.com/Luo-root/pulse
```

Without a local replacement, inspect the public commit selected by Version. With a replacement, check that Replace.Dir points to the checkout matching the docs. Also inspect `go.work` for intentional overrides. To test public dependencies independently, set `GOWORK=off` for that process; this does not remove a replace from `go.mod`, so check both. Do not change global Go settings for a one-off check.

## Optional packages

All packages belong to the same Go module. Once it is installed, these import paths are available:

```go
import (
	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)
```

Keep only the packages your program uses, because Go rejects unused imports. See the [package reference](../packages/index.md) to choose and use the optional packages.

## Troubleshooting

**The compiler reports mismatched constructor or `Run` arguments.** Check the selected source first. Older tags, earlier development snapshots, and this API differ. All five node sugar constructors now take `description` after `id`, with an empty string allowed. Unmerged changes require the corresponding local checkout.

**The toolchain cannot be downloaded.** Install or select Go 1.25+, or configure toolchain and module download access for your environment. Running Pulse itself requires no online service or credentials.

**You need recovery across process restarts.** Read the [checkpoint bridge](../reference/checkpoint.md) and follow the separate [Durable repository](https://github.com/Luo-root/pulse-durable)'s installation instructions. Pulse core does not store execution history.

Next: [Run your first workflow](quickstart.md).
