# 426 — Test coverage for OCM service-log / limited-support response mapping (protects PR #17, `ocm-sdk-go`)

Part of the Sept 2026 Dependabot audit of `openshift-online/srepd` PRs
#13–#18 — the umbrella audit plan identified this as one of two coverage
gaps at a dependency boundary (the other is
`docs/plans/425-bedrock-region-resolution-tests.md`, `pkg/ai`). This lands
as its own PR ahead of the dependency bumps, per `AGENTS.md`'s
one-plan-per-PR convention; the umbrella audit plan itself is not a repo
artifact.

## Problem

`clusterFromResponse` (`pkg/ocm/client.go`) is a standalone pure function
with thorough coverage in `TestClusterFromResponse` — the right pattern
for code that would break if `ocm-api-model` renamed or changed a getter.
`GetServiceLogs` and `GetLimitedSupportHistory` did the equivalent field
mapping (`entry.Timestamp()`, `.Severity()`, `.ServiceName()`, … →
`ServiceLog{}`; `reason.ID()`, `.Summary()`, `.DetectionType()`, … →
`LimitedSupportReason{}`) but inline inside the `.Each(...)` closure of the
method that also makes the live `c.conn...SendContext(ctx)` call. There
was no way to unit-test the mapping without a live or mocked OCM
connection, and nothing did — so PR #17 (`ocm-sdk-go` 0.1.505 → 0.1.509)
had to be manually verified rather than caught by CI.

## Fix

**Step 1 — extract**, following `clusterFromResponse`'s pattern exactly.
In `pkg/ocm/client.go`:

- Added `serviceLogFromResponse(entry *slv1.LogEntry) ServiceLog`.
- Added `limitedSupportReasonFromResponse(reason *cmv1.LimitedSupportReason) LimitedSupportReason`.
- `GetServiceLogs` and `GetLimitedSupportHistory` now call these inside
  their `.Each(...)` closures. Every field read was moved verbatim — no
  field added, dropped, or reordered. Confirmed with `git diff`: the only
  changes are the two new functions and the two one-line call-site swaps.
- Nil-handling matches `clusterFromResponse`'s convention: no explicit
  nil guard on the top-level pointer. This is safe because every SDK
  getter used (`entry.Timestamp()`, `reason.ID()`, etc.) already
  nil-checks its receiver and returns the type's zero value — confirmed
  by reading the generated getter bodies in `ocm-api-model` before relying
  on it, and by the `nil entry is safe` / `nil reason is safe` test cases
  below.

**Step 2 — test.** In `pkg/ocm/client_test.go`, built the same way
`TestClusterFromResponse` is (SDK builders, `require.NoError` on
`.Build()`):

- `TestServiceLogFromResponse` — three subtests: every field set and
  checked (`Timestamp`, `Severity`, `ServiceName`, `Summary`,
  `Description`, `ClusterID`, `ClusterUUID`, `InternalOnly` — every getter
  `serviceLogFromResponse` calls), a zero-value/empty-builder case, and a
  nil-`*slv1.LogEntry` case.
- `TestLimitedSupportReasonFromResponse` — same three shapes for
  `LimitedSupportReason` (`ID`, `Summary`, `Details`, `DetectionType`,
  `CreationTimestamp`).

Fields were enumerated from the extracted functions' bodies, not from
memory, so a renamed getter in a future `ocm-sdk-go` bump fails the test
build, not at runtime.

## Testing (TDD)

Extracted the two functions first (pure refactor, confirmed no behavior
change via `git diff`), then wrote the tests against them, then did the
fail-on-purpose check below to confirm the tests aren't tautological.

## Verification

- `go build ./pkg/ocm/...`, `go vet ./pkg/ocm/...`,
  `gofmt -s -l pkg/ocm/` — all clean.
- `go test ./pkg/ocm/... -count=1` and
  `CGO_ENABLED=1 go test -race ./pkg/ocm/... -count=1` — all green,
  including all pre-existing `pkg/ocm` tests plus the 6 new subtests
  (3 + 3) across the 2 new top-level tests.
- `git diff pkg/ocm/client.go` shows exactly the extract-function
  refactor described above: two new functions, two call sites updated to
  use them. No field added, removed, or reordered.
- **Fail-on-purpose evidence:** temporarily swapped the `ServiceName` and
  `Summary` field assignments in `serviceLogFromResponse`
  (`ServiceName: entry.Summary()`, `Summary: entry.ServiceName()`), reran
  `go test -run TestServiceLogFromResponse -v`. Result:
  `TestServiceLogFromResponse/every_field_mapped` failed on both fields
  (`expected: "SREManualAction", actual: "cluster flagged for review"`
  and vice versa); the zero-value and nil-input subtests were unaffected
  (both fields empty either way). Reverted; confirmed `go test
  ./pkg/ocm/...` green again and the diff matches the description above.
- `golangci-lint run` could not execute in this sandbox: the installed
  binary was built with go1.25, older than this repo's `go 1.26.5`
  toolchain directive. This is pre-existing and unrelated to this PR
  (same error against unmodified `pkg/ocm` before any change here, and
  already noted in plans 424 and 425).
- `pkg/ocm` has no dependency on `pkg/ai`/`pkg/tui`, so unlike plan 425
  this package builds and tests directly in the sandbox with no isolation
  workaround needed.
- No config/keymap/root/commands changes — `readme-check` should not
  trigger.
- Golden snapshots unaffected — nothing here touches `pkg/tui`.

## Files modified

- `pkg/ocm/client.go` — extract `serviceLogFromResponse`,
  `limitedSupportReasonFromResponse`; `GetServiceLogs` and
  `GetLimitedSupportHistory` call sites updated
- `pkg/ocm/client_test.go` — `TestServiceLogFromResponse`,
  `TestLimitedSupportReasonFromResponse`
- `docs/plans/426-ocm-response-mapper-tests.md` — this document

## Post-mortem

Filed after merge, if requested.
