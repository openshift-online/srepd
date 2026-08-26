# 422 — Hotfixes N1/N2/N4 + AI-path hygiene

## Problem

Three defects surfaced by audit after plans 418 and 413/417 merged, plus a
batch of bounded-resource and correctness cleanups in the same files. All
three headline defects are instances of the lesson recorded in
`417-413b-hygiene.md`:

> Snapshot the identity at the point of creation, not at the point of
> execution. The same principle applies to stream messages — every message
> must carry enough identity to be routed correctly even when superseded.

| ID | Severity | Summary |
|----|----------|---------|
| N1 | HIGH | `get_recent_events` was wired to frozen model state and always returned `[]` |
| N2 | MEDIUM-HIGH | Escalation ask acted on the UI selection, not the investigation's origin |
| N4 | LOW-MED | Typewriter ticks carried no identity; overlapping runs double-advanced |
| N3 | — | CI gating (see "CI decision" below — deliberately deferred) |

## N1 — `get_recent_events` wired to frozen state

### Defect

`initToolRegistryForModel(&m)` runs inside `InitialModel` and registered
`func() []delta.Change { return m.recentChanges }`. `Update` has a **value
receiver** (`func (m model) Update`), so `computeAndStoreDeltas`' appends
mutated per-`Update` copies. The closure's captured struct kept a nil slice
forever, and the tool returned `"[]"` for the whole session.

Two bugs in one: the closure read a stale copy, *and* the handler runs on the
investigation `tea.Cmd` goroutine, so even correct wiring would have been an
unsynchronized cross-goroutine read of a slice the main loop was appending to.

### Fix

A mutex-guarded bounded ring in `pkg/delta/log.go`:

```go
type Log struct{ mu sync.Mutex; changes []Change; max int }
func NewLog(max int) *Log
func (l *Log) Append(cs ...Change)       // evicts oldest past max
func (l *Log) Recent(limit int) []Change // returns a COPY
```

The model holds a `*delta.Log`. A pointer survives the value-receiver copies,
so every model generation appends to — and the tool handler reads from — one
shared buffer. The mutex makes the cross-goroutine read safe.

### Key design decisions

- **One source of truth.** The `recentChanges []delta.Change` field is
  **deleted**, not reduced to display-only. A display-only mirror would be a
  second copy to keep in sync, which is how this class of bug arises in the
  first place. Everything reads `m.changeLog`.
- **`Recent` returns a copy.** The handler marshals the result on another
  goroutine; handing out the internal slice would leak the mutex's protection.
  `Append` likewise copies its input rather than aliasing the caller's array.
- **A nil `*Log` is safe** (writes discarded, reads empty) so a partially-built
  test model cannot panic.
- **Eviction re-slices into a fresh array** rather than reusing the tail of the
  old one, so the evicted prefix is collectable instead of pinned.
- The retention bound stays 200 (`maxRecentChanges`), unchanged in behaviour.

### The wiring test that was missing

`TestGetRecentEventsTool_SeesDeltasFromUpdateLoop` constructs the real model,
drives a poll through `Update`, and invokes the registered handler **exactly as
the registry exposes it**. Against `main` it fails:

```
--- FAIL: TestGetRecentEventsTool_SeesDeltasFromUpdateLoop (0.00s)
    delta_tool_wiring_test.go:80:
        	Error:      	Should not be: "[]"
        	Messages:   	get_recent_events must observe deltas recorded by the Update loop
    delta_tool_wiring_test.go:82:
        	Error:      	"[]" does not contain "INC-DELTA-1"
    delta_tool_wiring_test.go:83:
        	Error:      	"[]" does not contain "INC-DELTA-2"
```

The original D5 tests (plan 418) exercised the tool against a hand-supplied
`getChanges` closure, so they passed while the production wiring was inert.
**The lesson: a test that supplies the dependency it is meant to verify tests
nothing about the wiring.** The new test takes the closure from the registry
the model actually built.

### While here — change timestamps

- `delta.Change` gains `At time.Time`.
- `Diff` takes `now time.Time` as a parameter and stamps every change. Passing
  the clock in keeps `Diff` pure and deterministic; it does not read
  `time.Now()` itself. All callers updated.
- `Narrate`'s `now` parameter was **dead** — accepted and never used. It now
  produces relative times (`"3m ago"`) via a new `relativeTime` helper.
  Zero-valued `At` renders without an age; future timestamps (clock skew)
  render as `"just now"` rather than a negative age.

## N2 — escalation ask acts on selection, not origin

### Defect

`AskEscalationSuggestion` snapshotted `*m.selectedIncident` at ask-creation
time even though `ask.IncidentID` already held the true origin and the
approvals pane *displayed* that origin. Two failure modes:

1. **Wrong target.** Origin ≠ selection dispatched the selection.
2. **Zero-value dispatch.** With nothing selected, the `incidentID == ""` guard
   *passes* (the origin ID is non-empty), so a zero-value
   `pagerduty.Incident{}` reached `unAcknowledgeIncidentsMsg` — re-escalating
   nothing while reporting success.

### Fix

The action resolves the incident **by `ask.IncidentID` at accept time**, via a
new `reEscalateIncidentByIDCmd`:

1. `registry.Lookup(id)` — a live read through the mutex-guarded
   `incidentRegistry` (see below). It matches the queue first, then falls back
   to the current selection **only when the selection's own ID equals the ask's
   ID** — a lookup table keyed by the ask's identity, never a substitute for
   it. That fallback covers an incident open in a detail view but absent from
   the list.
2. `pd.GetIncident` — an origin that has aged out of the queue.
3. Otherwise a clean `setStatusMsg` ("incident … no longer in queue") — never a
   panic, never a zero dispatch.

#### Why a registry, and not `m.incidentList` directly

The first implementation read `m.incidentList` and `m.selectedIncident`
straight out of the closure. Review caught that this **reintroduced N1's own
bug on the escalation path**: `buildAskFromVerdict` has a pointer receiver but
its only call site is inside `func (m model) Update` — a *value* receiver — so
`&m` is Update's dead stack copy. The stored closure then read a frozen list
much later, on a `tea.Cmd` goroutine. Two proven failures:

- **Stale reads.** With the origin removed from the live list by a later
  `Update`, the closure still resolved it out of the frozen snapshot. Since
  `tui.go` reads `incident.EscalationPolicy.ID` off the dispatched value, a
  stale snapshot escalates to the *old* policy — or, with an empty policy,
  drops the incident via a `log.Warn` with no user-visible failure.
- **A data race** against the in-place write at `tui.go:533`
  (`m.incidentList[i].Title = …`), which mutates the array the dead copy's
  slice header still aliases.

The fix mirrors N1's: an `*incidentRegistry` the model owns **by pointer**, so
it survives the value-receiver copies. `Publish` copies the slice in and
`Lookup` returns an owned copy out, so no caller can alias the array the Update
loop mutates. Crucially this *preserves* accept-time resolution — the whole
point of N2 — rather than regressing to a creation-time snapshot.
`reEscalateIncidentByIDCmd`, `postAINoteToIncidentCmd`, and
`copyToClipboardCmd` all became plain functions taking their dependencies
explicitly, so no ask closure can bind a method to a dead model. That makes the
invariant structural rather than a comment.

**Lesson (extends 417's):** *snapshot identity at creation, resolve state at
execution — and never let a closure outlive the model value it was built from.*
A pointer-receiver helper called from a value-receiver `Update` silently
captures a dead copy; prefer plain functions with explicit dependencies for
anything stored in a closure.

`buildAskFromVerdict` also changed: the **origin ID now wins even when the
incident is not currently in the list**. Previously `ask.IncidentID` was set
only from a *found* incident, silently falling back to the selection; the
ask's identity is the ID the investigation fired on, and resolution is the
action's job.

### Structural guard

`TestAskActionsDoNotReadSelectedIncident` scans the source region between the
`switch kind {` dispatch and `return ask`, asserting the substring
`m.selectedIncident` does not appear. Verified to catch a reintroduction:
inserting `_ = m.selectedIncident` into the escalation closure fails the test.

### Failing output on main

```
--- FAIL: TestBuildAskFromVerdict_Escalation_ActsOnOriginNotSelection
        	Error:      	Not equal: expected: "INC-ORIGIN" actual: "INC-OTHER"
        	Messages:   	escalation must act on the originating incident, not the UI selection
--- FAIL: TestBuildAskFromVerdict_Escalation_NothingSelectedNoZeroDispatch
        	Error:      	Should NOT be empty, but was
        	Messages:   	a zero-value pagerduty.Incident must never be dispatched
--- FAIL: TestBuildAskFromVerdict_Escalation_UnresolvableIDErrorsCleanly
        	Error:      	"no incident selected for re-escalation" does not contain "no longer in queue"
--- FAIL: TestBuildAskFromVerdict_Escalation_FetchesOriginNotInList
        	Messages:   	expected unAcknowledgeIncidentsMsg, got tui.setStatusMsg
```

One pre-existing test (`TestBuildAskFromVerdict_Escalation_TargetsOriginalIncident`)
needed its fixture corrected: it browsed away from an incident that was never
in `incidentList`, which is not a state the real UI produces. Both incidents
are now in the list. The test's assertion is unchanged.

## N4 — typewriter tick identity

### Defect

`typewriterTickMsg struct{}` carried no identity. Two overlapping typewriters
(a `:watcher` response landing during an investigation verdict) both drove the
single shared `m.typewriter`, so a superseded run's in-flight ticks advanced
its successor — visibly double-speed output.

### Fix

Per the 413b message-identity lesson: `model.typewriterGen` is a monotonic
counter stamped into each run's `typewriterState.gen` and into every tick it
schedules. `advanceTypewriter(gen uint64)` drops a tick whose `gen` does not
match the live run — it neither advances nor reschedules.

A bare `typewriterTickMsg{}` (gen 0) can no longer drive a real run, since
generations start at 1.

## CI decision (N3) — deliberately waived for this PR

`.github/workflows/go-ci.yml` is **not touched**. CI stays disabled by
maintainer decision; conversion to Prow is the next PR.

The original plan's acceptance criterion "CI runs on the PR itself" is
**consciously waived here**. Rationale: re-enabling GitHub Actions for one PR
would be immediately reverted by the Prow migration, and a workflow enabled and
disabled across two adjacent PRs is worse for history than one that stays
disabled until its replacement lands. The follow-up Prow PR restores gating.

All gates were run locally instead, and their output is recorded in the PR body.

`make deadcode` was added as an **informational, non-blocking** target
(`go run golang.org/x/tools/cmd/deadcode@latest ./...`, `|| true`). It is
deliberately **not** wired into `test-all` or any workflow: it reports
test-only helpers (mocks, `NewSessionWithChannels`) as unreachable from `main`,
so it is a human-read signal, not a gate.

## Quick wins

### 1. `SetTestChannels` → constructor (Option A, maintainer-decided)

`agent.SetTestChannels(s, events, done)` — an exported mutator that reached
into a session and swapped its channels — is **deleted**. It is replaced by
`NewSessionWithChannels(events, done) *Session`, which also sets
`lifecycleCtx` so the result is never a half-built object, and substitutes
real channels for nil ones.

**Why a constructor and not `export_test.go`:** the sole consumer lives in
`pkg/tui`, a **different package**, so an `export_test.go` helper cannot reach
it. **Why not a build tag:** `-tags test` would have to be threaded through
five Makefile targets plus the lint config, for no gain.

A constructor removes the *actual* hazard the mutator enabled: unsynchronized
mutation of a live session's channels while its `readLoop` is running. A
constructor cannot be pointed at a running session.

The sole consumer (`pkg/tui/claude_test.go`) previously hand-rolled
`&agent.Session{}` and then mutated it; it now calls the constructor.

### 2. Bounded stderr buffer + named marker

`execStreamExecutor` captured child stderr into an unbounded `bytes.Buffer`
for the life of the session. Replaced with `tailBuffer`, a mutex-guarded ring
retaining the last 64 KiB (`defaultStderrTailBytes`). Only the tail matters —
the buffer exists solely so `spawn` can look for the retry marker.

`Write` always reports `len(p)` so it composes with `io.MultiWriter`, which
treats a short write as an error.

The `"already in use"` literal (two sites) is hoisted to `sessionInUseMarker`
with a comment to re-verify on Claude CLI bumps — it is not a stable API.

`StreamCommandExecutor.Start` now returns `*tailBuffer` instead of
`*bytes.Buffer`; the test executors were updated mechanically.

### 3. Anthropic `Query` honours the package timeout

`anthropicProvider` gained a `requestTimeout` field and `Query` now calls
`ensureTimeout`, as ollama and openai already did. Without it a hung API call
blocked the `tea.Cmd` goroutine indefinitely.

### 4. `StreamQuery` drain contract

All three providers sent to `ch` unguarded. If the consumer went away, the
send blocked forever and leaked the provider goroutine. Each send site now
selects on `ctx.Done()`.

**Review lesson — the first revert-check had no teeth.** The original test
cancelled the context *before* calling `StreamQuery`. All three providers
issue their HTTP request first, so `httpClient.Do` failed on the cancelled
context in ~20µs and returned early: the scanner loop, and therefore the
guarded send site, was never reached. Deleting all three `select` guards
left the test passing in 0.00s. A revert-check that never executes the line
it protects is worse than no test — it reports safety that does not exist.

The replacement (`TestStreamQuery_CancellationUnblocksSend`) stands up an
`httptest.Server` per provider streaming multiple flushed chunks in that
provider's own wire format, runs `StreamQuery` on a goroutine against an
*unbuffered* channel, reads exactly one token — that receive is the
synchronisation point proving the send site was reached — and only then
cancels. With the guards removed it hangs and fails on a 10s detector. The
weaker original property (a pre-cancelled context aborts at the request
rather than hanging) is retained under the honest name
`TestStreamQuery_PreCancelledContextFailsFast`, documented as *not* a
revert-check.

Generalised: when a test is the sole evidence for a fix, verify it fails
with the fix reverted. 410a §3c requires this for the headline fixes; it
applies to quick wins too.

### 5. Rune-safe truncation

`pkg/agent/agent.go` (`summarizeToolInput`) and `pkg/tui/views.go`
(`renderEnrichmentError`) byte-sliced strings, splitting multi-byte runes.
Confirmed on main:

```
Messages: summary must stay valid UTF-8, got "日本語テキスト…日本語テキ\xe3\x82..."
```

Both are now rune-aware, with the caps named (`maxCommandSummaryRunes`,
`maxRawSummaryRunes`, `maxEnrichmentErrorRunes`).

### 6. Bounded `evicted` + index compaction

**`SessionManager.evicted`** recorded every incident ever evicted and never
removed anything. Two changes: the flag is **deleted once consumed** (a
resumed session already carries `resumed=true`, so the flag has done its job),
and `maxEvictedEntries` (256) backstops a process that keeps evicting
incidents it never revisits.

**`index.jsonl` compaction.** The file is append-only, so an incident
revisited many times accumulates a line per session. Above
`indexCompactionThreshold` (1000) lines, `load()` rewrites it keeping the last
entry per incident.

This is the highest-risk item — it rewrites a file on disk — so:

- **Atomic replace only.** A temp file in the same directory is written,
  synced, chmod'd 0600, and `rename`d over the original. The live file is
  **never truncated in place**, so a crash mid-rewrite leaves the previous
  index intact. Every failure path removes the temp file and leaves the
  original alone.
- **A scanner error aborts compaction.** A partially-read file would compact
  away entries that were never seen — silent data loss. `load()` returns
  early instead.
- **Original `Created` stamps are preserved**, not restamped to `time.Now()`.
- **Deterministic order.** First-seen incident order is retained rather than
  following Go's randomized map iteration.
- Corrupt and unparseable lines are tolerated and dropped; the rewritten file
  contains only well-formed entries.

Covered by tests for: atomicity (no temp files left, file complete), corrupt
and partial lines, empty file, missing file, exactly at threshold (not
rewritten), above threshold, no data loss for retained entries, and
append-after-compaction.

### 7. Named constants (opportunistic)

Only in files this plan already edits: `eventChanBuffer` (128),
`stdoutScannerBuffer` (256 KiB), `streamChanBuffer` (64),
`watcherBufferCapacity` (50). No repo-wide sweep.

## Files modified

| File | Change |
|------|--------|
| `pkg/delta/log.go` | **new** — mutex-guarded bounded `Log` |
| `pkg/delta/log_test.go` | **new** — bounding, copy semantics, concurrency |
| `pkg/delta/delta.go` | `Change.At`; `Diff(prev, curr, now)`; live `Narrate` + `relativeTime` |
| `pkg/delta/timestamp_test.go` | **new** — stamping and relative-time formatting |
| `pkg/delta/delta_test.go` | `Diff` callers updated |
| `pkg/tui/model.go` | `changeLog *delta.Log`; `typewriterGen`; `incidentRegistry` + N2 origin resolution via `reEscalateIncidentByIDCmd` |
| `pkg/tui/watcher.go` | `changeLog.Append`; typewriter generations; `watcherBufferCapacity` |
| `pkg/tui/tui.go` | `advanceTypewriter(msg.gen)` |
| `pkg/tui/views.go` | rune-safe `renderEnrichmentError` |
| `pkg/tui/stream.go` | `streamChanBuffer` |
| `pkg/tui/delta_tool_wiring_test.go` | **new** — N1 wiring, live-log, race |
| `pkg/tui/ask_escalation_origin_test.go` | **new** — N2 origin/zero-dispatch/unresolvable + structural guard |
| `pkg/tui/ask_live_state_test.go` | **new** — proves ask actions resolve against live state, not a dead model copy (incl. `-race` vs the in-place title write) |
| `pkg/tui/typewriter_identity_test.go` | **new** — N4 stale-tick rejection |
| `pkg/tui/enrichment_truncate_test.go` | **new** — rune-safe truncation |
| `pkg/tui/ask_wiring_test.go` | fixture corrected (both incidents in list) |
| `pkg/tui/watcher_integration_test.go` | typewriter ticks carry generations |
| `pkg/tui/model_test.go`, `pkg/tui/watcher_test.go` | `changeLog`, `testNow` |
| `pkg/tui/claude_test.go` | uses `NewSessionWithChannels` |
| `pkg/agent/session.go` | `NewSessionWithChannels`; `tailBuffer`; `sessionInUseMarker`; bounded `evicted`; constants |
| `pkg/agent/tailbuffer.go` | **new** — bounded tail writer |
| `pkg/agent/tailbuffer_test.go` | **new** |
| `pkg/agent/session_channels_test.go` | **new** |
| `pkg/agent/index.go` | atomic load-time compaction |
| `pkg/agent/index_compaction_test.go` | **new** — compaction + `evicted` bounding |
| `pkg/agent/agent.go` | rune-safe `summarizeToolInput` |
| `pkg/agent/truncate_test.go` | **new** |
| `pkg/agent/session_test.go` | executors return `*tailBuffer` |
| `pkg/ai/anthropic.go` | `requestTimeout` + `ensureTimeout`; guarded send |
| `pkg/ai/ollama.go`, `pkg/ai/openai_compat.go` | guarded sends |
| `pkg/ai/timeout_drain_test.go` | **new** |
| `Makefile` | `deadcode` target (informational) |

## Verification

Run locally (CI is disabled — see "CI decision"):

```
gofmt -s -l cmd pkg          # clean
go vet ./...                 # clean
go test ./... -count=1        # all packages ok
CGO_ENABLED=1 go test -race ./... -count=1
golangci-lint run --timeout 5m  # 0 issues
make test-fixtures
make quickstart-verify
make test-coverage-threshold
make test-vuln              # see note below
go build -o /dev/null .
```

### `make test-vuln` — pre-existing toolchain failure

`govulncheck` reports **7 Go standard-library vulnerabilities, all
`@go1.26.5`, all "Fixed in `@go1.26.6`"**:

| ID | Package |
|----|---------|
| GO-2026-6218 | `net/url` |
| GO-2026-6091 | `html/template` |
| GO-2026-6090 | `crypto/tls` |
| GO-2026-6089 | `net/http` |
| GO-2026-6088 | `encoding/xml` |
| GO-2026-5972 | `encoding/asn1` |
| GO-2026-5026 | `net/http` |

`go.mod` pins `go 1.26.5`, so this failure is present on `main` and is
**not caused by this PR** — no module dependency was added or changed here.
It is cleared only by a toolchain bump to 1.26.6, which belongs in its own
PR (compare plan 410, which handled the previous round of stdlib CVEs the
same way). The reported call sites are all pre-existing functions.

### Revert-check properties

1. **N1:** revert `changeLog` to a slice field → `TestGetRecentEventsTool_SeesDeltasFromUpdateLoop` fails with `"[]"`.
2. **N2:** read `m.selectedIncident` in an ask action → `TestAskActionsDoNotReadSelectedIncident` fails (verified).
3. **N4:** drop the gen check in `advanceTypewriter` → `TestTypewriter_StaleTickIsDropped` fails.
4. **Compaction:** truncate-in-place instead of rename → `TestSessionIndex_CompactionIsAtomic` fails.

## Constraints

- No new Go modules added. `golang.org/x/tools/cmd/deadcode` is invoked via
  `go run …@latest` from a Makefile target and is **not** a `go.mod` dependency.
- No `//nolint` directives added to production code (two in tests, both
  explaining deliberate no-op constructs).
- No `replace` directives or vendoring.
- `.github/workflows/go-ci.yml` untouched.

## Lessons (for 418 and 413/417)

1. **A test that supplies the dependency it is meant to verify tests nothing
   about the wiring.** Plan 418's D5 tests passed a `getChanges` closure
   straight into the tool and asserted on the result, so they were green for a
   feature that returned `[]` in production for its entire life. Where a
   component is *registered* with a dependency, at least one test must obtain
   that dependency **from the registry the production code built**.

2. **A value-receiver `Update` makes every model field a snapshot.** Any state
   that must outlive one `Update` — or be visible to a `tea.Cmd` goroutine —
   has to live behind a pointer, and if a goroutine can read it, behind a
   mutex. This is the same identity-race family as 417's B1/B5, one level
   down: not "which incident is this message about" but "which copy of the
   model is this closure holding".

3. **A guard that cannot fail is not a guard.** N2's `incidentID == ""` check
   passed in exactly the case that produced the zero-value dispatch. When a
   guard and the value it protects come from different sources, the guard
   proves nothing about the value.

4. **An unused parameter is a defect, not a style issue.** `Narrate`'s `now`
   was accepted and ignored across two plans, so every caller believed it was
   getting relative times. `make deadcode` was added partly to surface this
   class of thing.

## Post-mortem

_To be appended after merge._
