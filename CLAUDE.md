# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Core Principles

- **Do NOT maintain backward compatibility** unless explicitly requested. Break things boldly.
- Follow existing patterns; prefer explicit over clever; delete dead code immediately.
- **Keep the instruction sections of this file under 20-30 lines.** Move detail into `docs/` and reference it.

## Commands

`GOEXPERIMENT=jsonv2` is mandatory (the codebase imports `encoding/json/v2`); the Makefile exports it, direct `go` invocations must set it.

```bash
make build                 # -> dist/kirocc
make test                  # go test -race ./...
make lint                  # golangci-lint run   (make fmt / make fix also available)
make run ARGS="-port 4000" # run from source; make debug adds -debug
make test-e2e              # build tag `e2e`, hits the real Kiro API via local kiro-cli credentials
GOEXPERIMENT=jsonv2 go test -race -run TestName ./internal/reqconv/   # single test
make service-install       # macOS launchd user agent (scripts/service.sh)
```

CI (`.github/workflows/ci.yml`) additionally enforces `go mod tidy` and `go fix ./...` produce no diff. lefthook runs `golangci-lint run --fix`, `golangci-lint fmt`, `go fix`, and `oxfmt` on markdown at pre-commit.

## Architecture

kirocc is a localhost proxy: it accepts **Anthropic Messages API** requests and relays them to the **Kiro (Amazon Q) backend**, which speaks a different JSON request shape and returns AWS Event Stream binary frames. Almost all complexity lives in the two conversion directions.

Request path: `cmd/kirocc` → `internal/server` (routing + trace/CORS/api-key middleware) → `internal/app/messages` (the service: auth, model resolve, retry, tool-search orchestration) → `internal/reqconv` → `internal/kiroclient` → Kiro. Response path: `internal/kiroproto` (frame parsing) → `internal/respconv` (SSE or JSON) → client.

Package responsibilities that are not obvious from the name:

- `internal/app/messages` — the real handler layer. `handler.go` orchestrates one request; `execute.go` holds the retry-once loop; `gate_writer.go` buffers SSE output until visible content arrives so a thinking-only response can be retried transparently; `toolsearch.go` is the multi-round orchestrator for proxy-executed tools (ToolSearch and web_search — one round may fan out parallel searches); `websearch.go` enriches search results with fetched page text (`internal/webfetch`) and round-trips them to later turns via the `encrypted_content` carrier; `capture.go` records full upstream request/response bodies when `-debug` is on. `internal/server` is a thin router over this.
- `internal/models` — model ID mapping and the `[1m]` suffix semantics. The suffix means different things on request (thinking opt-in, stripped before upstream) vs response (1M context-window advertisement that Claude Code regex-matches). `Resolve` is two-tier: exact match first so always-1M aliases like `claude-opus-4-8[1m]` do *not* enable thinking, then strip-and-retry. Overridable via `KIROCC_MODEL_MAPPINGS`.
- `internal/reqconv` — message normalization, JSON Schema sanitization for Kiro's stricter tool schema, `<env>` block parsing, tool-result reordering, `cache_control` → `cachePoint`, and the synthetic system-prompt/ack history pair that kiro-cli always sends.
- `internal/respconv` — converts cumulative upstream text into incremental deltas, parses `<thinking>` tags / `reasoningContentEvent`, enforces `stop_sequences` and `max_tokens` adapter-side, and detects truncation.
- `internal/toolsearch` — proxy-side implementation of Anthropic's Tool Search Tool (regex + BM25), because the Kiro backend has no equivalent.
- `internal/auth` — reads credentials from kiro-cli's SQLite DB read-only and refreshes tokens (singleflight). API region and OIDC region are deliberately independent.
- `internal/kiroclient` — pins a kiro-cli User-Agent/version triple (`client.go`); the `TestUserAgent_Documents2100` drift guard fails if those strings change, so bump them intentionally.

`internal/kiroproto` holds the wire types; field order and `omitempty`/`omitzero` choices mirror observed kiro-cli captures and should not be changed casually. The `/kiro-capture` skill (`.claude/skills/`) captures real kiro-cli traffic when the wire format needs re-verifying.

Behavioral detail (effort/thinking resolution, model mapping table, tool-search query forms, flags and env vars) is documented in `README.md`; release process in `docs/RELEASING.md` and the `/release-kirocc` skill.
