# 424 — Parse the JSON `firing` detail in the shape PagerDuty actually delivers it (SOP link, cluster ID)

Rework of PR #23 (`78a1eee`). Replaces the previous version of this document
on the same branch/plan number.

## Problem

Two independent incidents from the `app-sre-alertmanager` PD service render
in srepd with missing data:

- `ClusterProvisioningDelay` (PR #23's trigger): no SOP link, even though the
  alert carries a `runbook` annotation and an `SOP:` URL in `message`.
- `HCPNodepoolUpgradeDelay`-shaped incidents: no OCM data, and cluster login
  refuses with "no cluster ID found" — extraction finds nothing at all
  because there is no top-level `cluster_id` detail either.

Both are the same bug. Some app-interface PrometheusRules put the raw
Alertmanager webhook payload into the `firing` custom detail. **On the wire
that value is a JSON array, not a string:**

```json
"details": {
  "firing": [
    {
      "annotations": { "message": "...", "runbook": "https://.../ClusterProvisioningFailure.md" },
      "labels":      { "alertname": "...", "severity": "high", "cluster_id": "..." }
    }
  ],
  "num_firing": "1"
}
```

## Root cause

go-pagerduty decodes `IncidentAlert.Body` as `map[string]interface{}`, so a
JSON-array `firing` detail arrives pre-decoded as `[]interface{}` — never as
a Go string. `getDetail("firing", alert)` (`pkg/alert/types.go`) does a
`.(string)` assertion and silently returns `""` for that shape, so
`ParseFiring` is never even called. `SOPLink`, `Severity`, `Condition`,
`Reason`, `ClusterName`, `DashboardLink`, and the `cluster_id` firing-label
fallback in `parseAppSRE` all stay empty. The same `.(string)` assertion
against `details["firing"]` in `pkg/tui/watcher.go`'s watcher-context
builders has the identical failure mode (and, on the *text* format, a
separate pre-existing bug: it labels the entire `Labels:/Annotations:` dump
as `SOP:` instead of extracting the actual URL).

**PR #23 did not fix this.** It added JSON decoding inside
`ParseFiring(firing string)`, but that function's only caller,
`getDetail(...)`, had already returned `""` by the time the array existed —
the new code was unreachable for the reported incident. PR #23's own tests
passed only because they built `details["firing"]` as a Go string (the shape
PD does not send); confirmed by re-running the fail-on-main test in this
document against PR #23's commit (`78a1eee`) — it fails identically to
`main`. See "Verification" for the captured output.

## Fix

All code changes are in `pkg/alert`, plus two call sites in
`pkg/tui/watcher.go`. `pkg/tui/commands.go` is not touched: `getSOPLink`,
`getUniqueClusters`, and `mapClusterServices` already fall back to
`alert.NormalizeAlert(...)` when the top-level detail is empty, so they are
fixed transitively once `pkg/alert` parses the JSON detail correctly.

### D1 — `pkg/alert/firing.go`: one flatten function for both shapes

`ParseFiring(string) map[string]string` is kept (some environments may still
stringify the payload). Added `parseFiringValue(raw interface{})
map[string]string`, which accepts the detail exactly as decoded from the
alert body:

- `string` → delegates to `ParseFiring`
- `[]interface{}` / `map[string]interface{}` → flattened directly
- anything else (`nil`, a JSON scalar, wrong element types) → empty map, no
  panic

Both the JSON-text path (inside `ParseFiring`) and the pre-decoded path
(`parseFiringValue`) route through the same `flattenAlertObjects` +
`copyStringValues` helpers, so they cannot disagree. Rules, enforced by
`TestParseFiring_JSONTextAndDecodedAgree`:

- Each alert object's `labels` are copied, then its `annotations` — matching
  the text parsers' key ordering.
- **Later alerts overwrite earlier ones** for a repeated key (PR #23 took
  only the first alert; that was wrong — it matches neither text format,
  where a repeated key's last line wins).
- Non-string label/annotation values are skipped per-value, not fatal (PR
  #23 decoded into `map[string]string`, so one non-string value failed the
  whole `json.Unmarshal` and dropped everything, including the SOP link).
  Decoding into `map[string]interface{}` and type-checking with comma-ok
  fixes this.

### D2 — `pkg/alert/types.go`: read `firing` without the string assertion

Added `getFiringDetail(alert) map[string]string`, which walks
`Body → details → firing` with comma-ok assertions and returns
`parseFiringValue(raw)`. Replaced the `firingText := getDetail("firing",
alert); if firingText != "" { fields := ParseFiring(firingText) ... }`
pattern in `parseOSDHive`, `parseAppSRE`, and `parseRHOBSHCP` with
`fields := getFiringDetail(alert)` (the empty-map return means the `if
firingText != ""` guard is no longer needed — the same lookups just see no
matches). `parseRHOBSInfra` does not read `firing` at all in the current
code and was left untouched. `getDetail` itself is unchanged and still used
for every other string detail.

### D3 — `pkg/tui/watcher.go`: stop mislabeling the firing dump as `SOP:`

Both `buildObservationContext` and `buildWatcherContext` had a
`details["firing"].(string)` block that (a) found nothing for the JSON-array
shape, and (b) for the text shape, printed the *entire* `Labels:/Annotations:`
dump under an `SOP:` heading rather than the actual URL. Replaced with
`alert.NormalizeAlert(inc.Service.Summary, inc.Title, a).SOPLink`, emitted
only when non-empty. The alert-loop variable was renamed `alert` → `a` in
both loops so the new `pkg/alert` import doesn't shadow.

### Not done (explicit non-goals)

- No `alertDetails`-style read-once accessor refactor, no regex-free text
  parsing rewrite, no debug-log line for the firing detail's dynamic type.
  These are refactors/nice-to-haves, not required to fix the reported bug,
  and the current diff is already the minimum needed for D1–D3.
- `namespace` vs `exported_namespace` ambiguity (carried over from PR #23's
  scope notes) is unchanged and still out of scope.
- `getUniqueClusters` / `mapClusterServices` / `getSOPLink` each read the raw
  `cluster_id` detail and then separately call `NormalizeAlert`, which reads
  it again. Harmless duplication; collapsing it touches `commands.go` and
  belongs in its own PR.

## Testing (TDD)

Wrote the tests below first, confirmed each reproduces the bug against
`main` (and, for the `pkg/alert` ones, against PR #23's commit `78a1eee`
too — see Verification), then implemented D1–D3 and confirmed green.

`pkg/alert/fixtures_test.go` (new): `appSREJSONFiringText` (synthetic
ClusterProvisioningDelay payload, with a `cluster_id` label added so the
label-fallback path is covered), `appSREJSONFiringTwoAlerts` (last-wins
fixture), and `decodeJSON(t, s)` — unmarshal into `interface{}` so fixtures
reproduce go-pagerduty's exact dynamic types instead of hand-built
`[]interface{}` literals.

`pkg/alert/firing_test.go`: kept PR #23's four `TestParseFiring_JSON*`
tests (string path). Added `TestParseFiringValue_DecodedArray/_DecodedObject
/_String/_NilAndUnknownTypes` (table-driven),
`TestParseFiring_JSONTextAndDecodedAgree`, `TestParseFiring_JSONLastAlertWins`,
`TestParseFiring_JSONNonStringValuesSkipped` (fails against PR #23's
`map[string]string` decode), `TestParseFiring_JSONMalformedElements`.

`pkg/alert/normalize_test.go`: rewrote `TestNormalizeAlert_AppSRE_JSONFiringFormat`
to build `firing` via `decodeJSON` with no top-level `cluster_id` — this is
the fail-on-main (and fail-on-PR#23) test. Added
`TestNormalizeAlert_AppSRE_JSONFiringString` (string-shape parity),
`_TopLevelClusterIDWins`, `_SOPFromMessage`,
`TestNormalizeAlert_JSONFiring_OtherTypes` (OSD Hive + RHOBS HCP with a
decoded firing), `TestNormalizeAlert_MalformedBodies_DoNotPanic` (`nil`,
`42`, `[]interface{}{"x"}`, `map[string]interface{}{"labels": "notamap"}`).
All pre-existing tests in the package pass unchanged.

`pkg/tui/appsre_json_firing_test.go` (new): `getUniqueClusters` /
`getSOPLink` / `mapClusterServices` against a decoded-array alert with no
top-level `cluster_id`; a malformed label-sourced `cluster_id` is dropped
with the same `skipping malformed cluster_id` warning
(`ocm.ValidClusterID` still applies regardless of source);
`buildWatcherContext` / `buildObservationContext` against both a
decoded-array alert (exactly one `SOP:` line, no `annotations` substring)
and a text-format alert (SOP URL present, no `Labels:` dump).

No test computes its expected value by calling the function under test —
all expectations are literals from the fixtures (plans 414/423 lesson).

## Verification

- `go test ./pkg/alert/... -race`, `go vet ./pkg/alert/...`,
  `gofmt -s -l pkg/alert` — all clean, all green.
- Fail-on-main evidence: `TestNormalizeAlert_AppSRE_JSONFiringFormat` run
  against `upstream/main` (`db6cb3c`) fails on `ClusterID`, `Severity`,
  `Condition`, `Reason`, `ClusterName`, `SOPLink`, `DashboardLink` — every
  one empty. The same test run against PR #23's commit (`78a1eee`) fails
  identically, confirming PR #23 did not reach the bug. Both runs captured
  and included in the PR body.
- `pkg/tui` could **not** be built or tested in this sandbox: `pkg/ai`
  unconditionally imports `anthropic-sdk-go/vertex`, which imports
  `google.golang.org/api`; that module's zip is not in the local module
  cache and the sandbox's egress policy blocks both
  `storage.googleapis.com` (the proxy.golang.org redirect target) and
  `google.golang.org` directly (`GOPROXY=direct` also fails). This is a
  pre-existing sandbox limitation, not introduced by this change — the same
  failure occurs on a clean checkout of `main` with no changes at all. The
  `pkg/tui/appsre_json_firing_test.go` changes were verified by `gofmt -e`
  (parses cleanly) and manual trace against the exact signatures used
  elsewhere in the same files, but could not be executed here. Flagging
  this rather than silently skipping it — CI (which presumably has
  unrestricted network) should confirm `pkg/tui` green.
- `golangci-lint` also could not run (installed binary predates the repo's
  go1.26.5 toolchain requirement) — same limitation noted in PR #23.
- No README/config/keymap changes; `commands.go`/`keymap.go`/`root.go` are
  untouched, so `readme-check` should not trigger.
- Golden snapshots unchanged — nothing here touches `View()` rendering.

## Files modified

- `pkg/alert/firing.go` — `parseFiringValue`, `decodeAlertObjects`,
  `flattenAlertObjects`, `copyStringValues`; `ParseFiring`'s JSON branch now
  shares them
- `pkg/alert/types.go` — `getFiringDetail`; `parseOSDHive`, `parseAppSRE`,
  `parseRHOBSHCP` use it
- `pkg/tui/watcher.go` — D3, two call sites, plus the `pkg/alert` import
- `pkg/alert/fixtures_test.go` (new), `pkg/alert/firing_test.go`,
  `pkg/alert/normalize_test.go`, `pkg/tui/appsre_json_firing_test.go` (new)
- `docs/plans/424-json-firing-sop-parsing.md` — this document

## Post-mortem

Filed after merge, if requested.
