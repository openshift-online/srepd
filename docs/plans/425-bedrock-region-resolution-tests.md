# 425 — Test coverage for Bedrock region resolution (protects PR #14, `aws-sdk-go-v2/config`)

Part of the Sept 2026 Dependabot audit of `openshift-online/srepd` PRs
#13–#18 — the umbrella audit plan identified this as one of two coverage
gaps at a dependency boundary (the other is `docs/plans/426-ocm-response-mapper-tests.md`,
`pkg/ocm`). This lands as its own PR ahead of the dependency bumps, per
`AGENTS.md`'s one-plan-per-PR convention; the umbrella audit plan itself
is not a repo artifact.

## Problem

`pkg/ai/bedrock.go`'s `resolveBedrockRegion(cfg Config) string` resolves
the AWS region through a precedence chain — `cfg.Region` →
`AWS_REGION` → `AWS_DEFAULT_REGION` →
`config.LoadDefaultConfig(ctx).Region` → `""` — and is the only place in
the codebase that depends on `aws-sdk-go-v2/config`. It already existed as
a standalone, unexported function (no extraction needed — confirmed by
reading `pkg/ai/bedrock.go` before writing anything), but nothing tested
it: the existing `pkg/ai/bedrock_test.go` only covered `newBedrockProvider`
auth-panic recovery and the default model constant. A future bump of
`aws-sdk-go-v2/config` (e.g. a change to `LoadDefaultConfig`'s error
semantics or `aws.Config.Region`'s zero value) could silently break the
last link in that chain with nothing to catch it.

## Fix

No production code changed. Added five tests to the existing
`pkg/ai/bedrock_test.go`, mirroring `pkg/ai/vertex_test.go`'s style:

- `TestResolveBedrockRegion_ConfigTakesPrecedence` — `cfg.Region` set with
  conflicting `AWS_REGION`/`AWS_DEFAULT_REGION` also set; config wins.
- `TestResolveBedrockRegion_FromEnvAWSRegion` — `AWS_REGION` beats
  `AWS_DEFAULT_REGION` when both are set.
- `TestResolveBedrockRegion_FromEnvAWSDefaultRegion` — falls to
  `AWS_DEFAULT_REGION` when `AWS_REGION` is unset.
- `TestResolveBedrockRegion_FromSDKConfigChain` — the one test that
  actually exercises `aws-sdk-go-v2/config`: writes a temp
  `~/.aws/config`-format file (`[default]\nregion = ...`), points
  `AWS_CONFIG_FILE` at it via `t.Setenv`, sets `AWS_PROFILE=default`, and
  asserts `LoadDefaultConfig` picks up the region.
- `TestResolveBedrockRegion_NoneFound` — nothing set anywhere; returns
  `""`, no panic.

All five use a shared `noAWSConfigFiles(t)` helper that points
`AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` at a `t.TempDir()` path
and clears `AWS_PROFILE`/`AWS_REGION`/`AWS_DEFAULT_REGION` via `t.Setenv`
(auto-restored), so no test reads a developer's or CI runner's real AWS
config, and none can leak state into another test.

## Testing (TDD)

Wrote the five tests against the existing `resolveBedrockRegion`
implementation (unchanged) and confirmed all pass, then did the
fail-on-purpose check described below to confirm the tests aren't
tautological.

## Verification

- Sandbox note: `pkg/ai` cannot be built directly here — `vertex.go`
  imports `anthropic-sdk-go/vertex`, which imports `google.golang.org/api`,
  and the sandbox proxy blocks that module's zip
  (`storage.googleapis.com` → `Forbidden`). This is pre-existing and
  unrelated to this change (same failure on a clean `main` checkout with
  no edits). Verified instead in an isolated throwaway module at
  `/tmp/ai-isolated`: every `pkg/ai/*.go` file except `vertex.go` and
  `vertex_test.go`, plus a local `vertex_stub.go` providing a stub
  `newVertexProvider` so `factory.go` compiles, `go.mod` pinned to this
  branch's exact `go.mod` versions (`anthropic-sdk-go v1.61.0`,
  `aws-sdk-go-v2/config v1.27.27`), resolved from the local module cache
  via `GOPROXY=file://$(go env GOMODCACHE)/cache/download` (no network).
  This mirrors how PR #16 (`anthropic-sdk-go` bump) was audited.
  - `go build ./...`, `go vet ./...`, `gofmt -s -l .` — clean.
  - `go test ./...` and `CGO_ENABLED=1 go test -race ./...` — all green,
    including all pre-existing `pkg/ai` tests (95 tests, matching the
    PR #16 audit's count) plus the 5 new ones.
  - `go test -run TestResolveBedrockRegion -count=5 -v ./...` — all 25 runs
    (5 tests × 5 reps) pass, no env-leak flakiness.
  - Ran the full suite once with a populated `~/.aws/config`
    (`[default]\nregion = us-fake-nonsense-1`) present on the host to
    confirm `TestResolveBedrockRegion_NoneFound` and the other
    `noAWSConfigFiles`-isolated tests still pass and never see it. They
    did. **Side finding (not fixed here, out of scope):** the pre-existing
    `TestNewProvider_BedrockNoRegion` in `factory_test.go` does *not*
    isolate against `~/.aws/config` — with that file present it picked up
    the fake region via the SDK config chain and the test's
    `require.Error` failed. This is a latent gap in an already-existing
    test, orthogonal to this PR's scope (this PR only adds coverage for
    `resolveBedrockRegion` itself); flagging it here rather than folding
    an unrelated fix into this PR. Removed the fake `~/.aws/config` before
    finishing.
  - `golangci-lint run` could not execute in this sandbox: the installed
    binary was built with go1.25, older than this repo's `go 1.26.5`
    toolchain directive (`can't load config: the Go language version
    (go1.25) used to build golangci-lint is lower than the targeted Go
    version`). Confirmed this is pre-existing and unrelated to this PR by
    running the identical command against unmodified `pkg/ocm` — same
    error. Noted in plan 424 previously; CI's `make lint` should still run
    normally there.
- **Fail-on-purpose evidence:** temporarily swapped the env-var precedence
  in `resolveBedrockRegion` (`{"AWS_REGION", "AWS_DEFAULT_REGION"}` →
  `{"AWS_DEFAULT_REGION", "AWS_REGION"}`), reran
  `go test -run TestResolveBedrockRegion -v`. Result:
  `TestResolveBedrockRegion_FromEnvAWSRegion` failed
  (`expected: "us-west-2", actual: "eu-west-1"`); the other four passed
  unaffected. Reverted; confirmed `git diff pkg/ai/bedrock.go` is empty —
  only `pkg/ai/bedrock_test.go` changed in this PR.
- No config/keymap/root/commands changes — `readme-check` should not
  trigger.
- Golden snapshots unaffected — nothing here touches `pkg/tui`.

## Files modified

- `pkg/ai/bedrock_test.go` — five new tests plus the `noAWSConfigFiles`
  helper; two pre-existing tests untouched
- `docs/plans/425-bedrock-region-resolution-tests.md` — this document

## Post-mortem

Filed after merge, if requested.
