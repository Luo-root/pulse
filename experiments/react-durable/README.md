# ReAct / Durable experiment (Draft; do not merge)

This standalone Go test module archives the ReAct assessment implementation for
Pulse Draft PR #513. It is not a supported Agent API, not a Pulse core dependency,
and not a proposal to merge this experiment into production. The formal Agent
suite belongs in a separate repository. No root module, README or CI change is
required to run it.

## Provenance and dependency pins

The four assessment files were copied from uncommitted experiments in a Durable
workspace whose tracked HEAD was
[`b2e69374f8653c3cb2eb073d5d293ad05d043544`](https://github.com/Luo-root/pulse-durable/commit/b2e69374f8653c3cb2eb073d5d293ad05d043544).
That commit identifies the source workspace baseline, **not** a published commit
containing these four tests. Their archived source is now in this PR:

- [react_assessment_test.go](react_assessment_test.go): opaque loop replay and message-state resume; asserts tool attempts `[1,2,1,2]` versus `[1,2,2]`.
- [live_react_assessment_test.go](live_react_assessment_test.go): bounded real Messages-protocol client, local HTTP fixtures, rejection/budget/redirect checks and opt-in live model loop.
- [systemtest/react_recovery_assessment_test.go](systemtest/react_recovery_assessment_test.go): fake-model assembly plus opt-in PostgreSQL / independent worker kill windows.
- [systemtest/live_react_recovery_test.go](systemtest/live_react_recovery_test.go): dynamic persisted model/tool state, offline HTTP recovery and opt-in real-model worker takeover.

[helpers_test.go](helpers_test.go) adapts the public runtime helpers from
`runtime_test.go`; [systemtest/helpers_test.go](systemtest/helpers_test.go) extracts
only the required pieces of `distributed/systemtest/harness.go` and
`process_test.go`. It replaces the source helper's fixed fixture password with a
per-fixture random credential. Other tests and benchmark/MQ infrastructure are
not copied. Root assessment imports are adapted to the published public API;
the assertions and model/tool execution implementations are retained.

Dependencies use Pulse v0.5.0 and Durable main commit
[`d4910771b407`](https://github.com/Luo-root/pulse-durable/commit/d4910771b407):
root `v0.1.1-0.20261025184222-d4910771b407`, distributed
`v0.0.0-20261025184222-d4910771b407`. Both versions were verified with
`go mod download`. There are no local replace directives or workspace-dependent
rootbridge versions. `go.sum` records the dependency checksums.

## Default offline tests (PowerShell)

Run from this directory with Go 1.26 or newer. The first download requires network
access to public Go modules; subsequent test execution uses only memory and local
`httptest` servers, not a model API, PostgreSQL or containers.

```powershell
$env:GOWORK = 'off'
$env:PULSE_LIVE_MINIMAX_ALLOW = ''
$env:PULSE_DISTRIBUTED_TEST_ALLOW = ''
$env:PULSE_REACT_CHILD = ''
$env:PULSE_LIVE_RECOVERY_CHILD = ''
go mod download
go test -race -count=1 -buildvcs=false ./...
go vet -buildvcs=false ./...
```

The opt-in flags default to unset. Explicitly clearing them also prevents an
inherited environment from accidentally enabling live tests. Child test entries
skip unless their controller supplies their private control environment.
`TestLiveReActRecoveryOffline` cancels after an assistant and the first tool have
committed, builds a fresh graph/runtime, resumes pending tools and verifies full
opaque content, partial results, request budget and final early stream stop.
It is a deterministic local fixture, **not a real-model API run**.

## Explicit opt-in tests (not run by default)

Credentials come only from process environment, never config files, chat history
or committed fixtures. Do not print environment values or embed them in commands.

- Real model: supply `PULSE_LIVE_MINIMAX_ALLOW` (value `yes`),
  `PULSE_LIVE_MINIMAX_KEY`, `PULSE_LIVE_MINIMAX_BASE` and
  `PULSE_LIVE_MINIMAX_MODEL`. Base/model have the official MiniMax/M3 defaults in
  code; the destination must use HTTPS and redirects are refused.
- PostgreSQL: supply `PULSE_DISTRIBUTED_TEST_ALLOW` (value `yes`) and
  `PULSE_DISTRIBUTED_TEST_DSN` for the isolated local test service. The source
  allowlist is retained: loopback only, port 55432, user `durable_test`, database
  `durable_distributed_test` or `durable_dist_security_20261025`. The administrator
  needs role/migration privileges. Provisioning creates random namespaced roles
  and retains test records/roles as evidence; use a disposable database and manage
  cleanup separately. No container is automatically started.

After externally supplying the appropriate environment variables:

```powershell
$env:GOWORK = 'off'
go test -race -count=1 -buildvcs=false -run '^TestLiveReActMiniMax$' .
go test -race -count=1 -buildvcs=false -run '^TestAssessmentReActProcessRecovery$' ./systemtest
go test -race -count=1 -buildvcs=false -timeout=360s -run '^TestLiveReActRecoveryPostgres$' ./systemtest
```

The last command requires both sets of explicit permissions. It sends only
synthetic random lookup/add data to the model. Child workers receive their
isolated DSN/key through environment and never discover credentials themselves.
Do not set child-control flags by hand.

## Evidence boundaries

The archived design's live PostgreSQL/model observations are historical results;
copying this experiment does not rerun or certify them. This PR's default checks
validate the offline paths only. The fake-model process test retains three kill
windows (`after_model`, `after_tool`, `unsaved_tool`); the real-model process test
retains the first two and verifies fencing epoch, committed history hashes and
non-replay of committed events. An uncommitted model request or external tool
effect may still repeat. No exactly-once external I/O is promised. Cross-machine
live recovery, real write-tool idempotency, long-running behavior and multi-turn
session lifecycle remain unverified. Record-growth measurements in the design
are historical; their separate assessment is not part of this archive.
