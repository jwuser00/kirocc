# Merging upstream

This is a fork of [d-kuro/kirocc](https://github.com/d-kuro/kirocc). Upstream
keeps releasing, and every merge hits the same conflict classes for the same
structural reason: both sides add rows to the same tables, fields to the same
structs, and options to the same constructors. This file records what recurs and
what was decided, so the next merge does not re-derive it.

Remotes: `origin` is this fork, `upstream` is d-kuro/kirocc (push disabled).

## Procedure

Never merge into `main` directly — the merge is large enough that you want a
branch you can throw away.

```bash
git fetch upstream

# What each side has that the other does not.
git log --oneline upstream/main..main    # ours
git log --oneline main..upstream/main    # theirs

# Dry run: which files will conflict, without touching the tree.
git merge-tree --write-tree main upstream/main | grep -i conflict

git switch -c merge/upstream-<version>
git merge upstream/main --no-commit
```

Resolve, then verify all of these before committing. `GOEXPERIMENT=jsonv2` is
mandatory; the Makefile exports it, direct `go` invocations must set it.

```bash
GOEXPERIMENT=jsonv2 go build ./...
GOEXPERIMENT=jsonv2 go vet ./...
GOEXPERIMENT=jsonv2 go vet -tags e2e ./...          # e2e files compile
GOEXPERIMENT=jsonv2 gofmt -l .                      # must print nothing
GOEXPERIMENT=jsonv2 go mod tidy                     # must produce no diff
GOEXPERIMENT=jsonv2 go fix ./...                    # must produce no diff
GOEXPERIMENT=jsonv2 go test -race ./...
```

`git merge --no-commit` leaves `go.sum` auto-merged; run `go mod tidy` after the
source conflicts are resolved, not before, or it will fail on unparseable files.

Then run the service on the branch and exercise it against the real API before
merging to `main` — the test suite does not cover the request/response
conversion end to end.

```bash
make service-install     # builds from the current checkout, replaces the agent
make service-status
```

`/v1/messages` requires an `X-Claude-Code-Session-Id` header; curl probes without
it get a 400 that has nothing to do with your merge.

## Recurring conflict classes

These conflict every time and are almost always a union — both sides added
something, neither replaced the other. Take both.

| Location | What collides |
| --- | --- |
| `internal/config/config.go` — `Config` struct | both sides add fields |
| `internal/app/messages/service.go` — `Service` + options | both sides add options |
| `internal/server/server.go` — `Server` + options | same, one layer up |
| `cmd/kirocc/main.go` — flag registration | both sides add flags |
| `internal/models/models.go` — `modelMapOrdered` | both sides add model rows |
| `internal/models/effort.go` — `effortCapabilities` | both sides add models |
| `README.md` — flag table, env var table | both sides add rows |
| `go.mod` | dependency bumps; take the higher version |

Union is not always enough. Watch for these:

- **A flag name that both sides used for different things.** See
  `-kiro-api-region` below. A textual union produces a silently broken
  double-assignment (`applyString` called twice on different fields), which
  compiles and passes tests.
- **A helper one side deleted in a refactor.** Upstream v0.8.0 wrapped
  `GateWriter` in `streamSession`, which removed the seam our
  `writeStreamingOrJSONError` sat on. The fork-side helper had to go, not be
  reconciled.
- **A function signature one side changed.** `buildCurrentMessage` moved its
  content scan to the call site upstream while we added a `historyImages`
  argument. Take the upstream shape and append our argument.
- **Test files that do not conflict but stop compiling.** `git` reports no
  conflict in a file only one side touched, yet it can still reference a
  signature the other side changed. `internal/respconv/synthetic_echo_test.go`
  (ours) broke on `OnVisibleOutput func()` → `func() error` (theirs), and
  `internal/models/catalog_test.go` (ours) broke on `ListModels() []string` →
  `[]ModelInfo` (theirs). Always run the full build, not just the conflicted
  packages.
- **A test whose assertion the merge invalidates.** `catalog_test.go` asserted
  that `gpt-5.6-terra` is absent from `ListModels()` to prove discovery filters
  non-claude models. Upstream then added that ID to the built-in table, making
  the assertion vacuous. It now uses an ID absent from both tables.

## Decisions that must not be silently reverted

Re-deriving these from the diff alone leads to the wrong answer.

### Region: `KiroAPIRegion` was folded into `Region`

Both sides shipped a flag named `-kiro-api-region` / env `KIRO_API_REGION` with
different meanings — ours (2026-07-27, `e84cf71`) as an alias for `-region`,
upstream's (v0.6.0) as the region for API-key auth. They are both the upstream
API region, so `KiroAPIRegion` was dropped and `cmd/kirocc/main.go` passes
`cfg.Region` to both `auth.WithAPIKey` and `auth.WithRegionOverride`. A
consequence worth knowing: `-region` now also applies in API-key mode, and the
upstream-only combination "API key with a separate runtime region" can no longer
be expressed.

**What must survive any future change here:** the split between the API region
(`Credentials.Region`) and the OIDC region (`Credentials.SSORegion`). Ranking the
stored region above the profile ARN was the fork's first bug (`afb5d1e`): for IDC
credentials the stored field holds the SSO region, so the endpoint became
`runtime.ap-northeast-2.kiro.dev`, which has no DNS record, and every request
failed to connect. `regionOverride` is applied in `AuthManager.readFromDB` and
replaces `Region` only.

Note that `WithRegionOverride`'s doc comment ("SSORegion is left untouched") is
only meaningful for IDC. Social credentials carry one region and
`refreshCredentials` builds the social refresh endpoint from `creds.Region`, so
`-region` moves token refresh too. Pre-existing on both sides, not a merge
artifact, and inert unless someone sets `-region` on a social credential.

### Schema guard: `EnsureObjectRoot` replaced `ensureObjectSchema`

Both sides independently fixed Kiro rejecting a whole request over one tool's
schema (`TOOL_SCHEMA_INVALID`). Upstream's `EnsureObjectRoot` covers more — it
wraps a non-object `type` in an object envelope — so ours was deleted. But
upstream returned early on `type: "object"` without ensuring `properties`
exists, which is the case our version was written for, so that guarantee was
merged in. Both `type: "object"` and no-type inputs now get `properties`.

### Redacted reasoning blobs are not replayable for proxy-executed rounds

GPT 5.6 (upstream v0.8.0) streams reasoning as opaque `redacted_thinking` blobs
that arrive *after* text and tool_use. Upstream replays them into history for
tool-use continuations. Its tool-search path handled exactly one search per
round; our `2a8f730` turned that into a fan-out loop over `plan.toolSearches`,
so the 1:1 blob-to-turn correspondence no longer holds.

The resolution is not about correspondence at all:
`extractReplayableRedactedThinking` (`internal/reqconv/history.go`) attributes a
blob to the tool call it sits with and **skips blobs attributed to a
`server_tool_use`** — the backend has already consumed that round's reasoning,
so replaying it would be rejected. `appendSearchMessages` emits
`server_tool_use`, so its blobs are skipped on the next request regardless.
Carrying them on every search would only inflate the history payload, hence
first-turn-only.

The same reasoning is why `appendWebSearchMessages` takes no `redacted`
argument. That is deliberate: do not "fix" it by adding one.

## Fork-only surface

Files absent from upstream. They never conflict, but upstream refactors reach
them through the seams they attach to, so they are where post-merge compile
errors surface.

- `internal/webfetch/` — page fetch + extract + cache for web search
- `internal/kiromcp/` — InvokeMCP client, Kiro-hosted `web_search`
- `internal/app/messages/websearch.go` — search execution and enrichment
- `internal/reqconv/websearch_blocks.go`, `internal/reqconv/server_tools.go`
- `internal/respconv/streaming_websearch.go`
- `internal/models/catalog.go` — startup model discovery
- `internal/kirocatalog/client.go` — `ListAvailableModels`
- `internal/anthropic/synthetic.go` — synthetic placeholder detection
- `scripts/service.sh`, `scripts/com.kirocc.server.plist.template` — launchd

## Things upstream adds that this fork does not use

`docs/release-notes/` accumulates upstream release notes. This fork does not cut
releases, so they are carried along untouched rather than deleted — deleting them
would conflict on every subsequent merge.
