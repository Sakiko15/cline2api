# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Cline2API (module `cline-go-proxy`) is a reverse proxy in front of the Cline API: it exposes OpenAI (`/v1/chat/completions`, `/v1/responses`) and Anthropic (`/v1/messages`) endpoints, rotates a pool of Cline accounts, and routes free models through the opencode "zen" API. Ships as a CLI server (default port 3457), a Docker image, and a Wails desktop app. Bilingual admin UI at `/admin/` (no auth by default; optional password).

## Commands

```bash
go build -o cline-proxy .            # CLI build
./cline-proxy                        # run :3457 — flags: -port -host -login -add-account -list -capture -start
go test ./...                        # all tests (single package)
go test -run TestRouteModelZenFree . # single test (add -v for verbose)
./desktop/build.sh                   # desktop (Wails) build — also ./desktop/dist.sh for release zip (Windows-only)
docker compose up -d                 # Docker run
```

- CLI and desktop are the **same package** split by build tags: `main.go` (`//go:build !desktop`) vs `desktop_main.go` (`//go:build desktop`). Plain `go build .` never compiles desktop code; desktop needs `-tags "desktop production"` (done by `desktop/build.sh`) and cannot cross-compile.
- CI: `build.yml` (desktop binaries) runs only on `v*` tags / workflow_dispatch — no PR/push CI, no lint/test job (only a binary `-selfcheck` smoke step), no linter or formatter config. `docker.yml` (GHCR multi-arch image `ghcr.io/sakiko15/cline2api`, linux/amd64+arm64) runs on `v*` tags + **main pushes (tagged `edge` + `sha-*`)** + workflow_dispatch; `latest` is only updated by tag pushes. `appVersion` is injected via `-ldflags "-X main.appVersion=..."`.
- Names disagree: module `cline-go-proxy`, repo `cline2api`, binary `cline-proxy`, desktop output `cline-proxy-desktop`.

## Architecture

One flat `package main` at the repo root — no subpackages, no web framework (stdlib `net/http` `ServeMux` + `encoding/json`). Direct deps: `utls` (TLS fingerprint spoofing), `wails/v2` (desktop shell), `x/net` (HTTP/2, zen client only).

### Request flow

`startProxy` (`proxy.go`) wires routes + middleware (`corsHandler` → `apiKeyHandler` → handler — model endpoints only; `/health` and `/admin` sit outside this chain). Chat handler flow:

1. Body parsed into `map[string]any`; `override.md` (resolved via `resolveDataPath`, mtime+size cached, 256KiB cap) replaces the first system message — or prepends one if none — on `/v1/chat/completions` and `/v1/messages` (Anthropic `system` field); never applied to `/v1/responses`.
2. `routeModel` (`zen.go`) decides: paid opencode model → 400 reject; zen free model → `maybeCompact` (context compaction, `compact.go`) → `callZenAPI`; otherwise → `callClineAPI`.
3. Cline path: `pickAccountForModel` (`pool.go`) selects an account (strategy + model-cooldown aware), `buildUpstreamBody` transforms the request, spoofed headers applied, POST to `api.cline.bot`; retries once on 401 after token refresh.
4. Response normalized (`unwrapDataEnvelope`/`responses.go`, `normalizeOpenAIResponse`), usage parsed and recorded (per-model stats only for free models), request log finalized (`request_logs.go`) — requests failing early validation (method/body/account checks) return before any logging.

Three independent SSE paths: OpenAI pass-through (`handleStreamResponse`), OpenAI→Anthropic event machine (`handleAnthropicStream`, with `toolAccumulator` for tool_use blocks), OpenAI→Responses (`chatStreamToResponses`). Protocol converters live in `proxy.go` (`anthropicToOpenAI`, `openAIToAnthropic`) and `responses.go` (`responsesToChat`, `chatToResponses`). Anthropic image blocks in user turns are converted to OpenAI `image_url` parts by `anthropicToOpenAI` (base64→data URL, url passed through; unsupported source shapes skipped with a one-time log; assistant-turn images are still dropped) — cline upstream acceptance of image parts is unverified. `/health` and `/v1/health` return only `{"status":"ok"}`.

### Account pool & auth

`pool.go` + `types.go`: global `pool *AccountPool` guarded by `poolMu`. Hot-path mutations (account selection, cooldowns, usage stats) set a dirty flag via `markPoolDirtyLocked`/`markPoolDirty` and flush at most once per second (`debounce.go` `poolDebouncer` — writes happen inside `poolMu` because `writeFileAtomic` uses a fixed tmp path); admin/config operations still `savePool()` synchronously. Accounts carry plaintext refresh tokens, in-memory access tokens refreshed via a Cline refresh-grant (`refreshClineToken`; the WorkOS device flow in `auth.go` is only for initial login/add-account) with a 60s expiry skew, status, per-model cooldowns, usage stats. Selection strategies: `round_robin | fill | random` (persisted in `.cline-config.json`, not the pool; unknown values fall back to round_robin) — round-robin uses a global `atomic.Uint64` counter (`rrCounter`), independent of any filtered list; `CurrentIdx` is display-only. Cline 429 → model-level cooldown parsed from upstream error text (`parseCooldownUntil`; account-level instead when the model name is empty); transport error → 5-min account cooldown; `startCooldownRecovery` probes and reactivates every 30s, plus low-frequency probes (~5 min) of `expired` accounts — an account is only marked `expired` on an HTTP 4xx from the refresh endpoint (`tokenRefreshRejectedError`); transient refresh failures keep it active, so auto-recovery can pick it up. Model syncs and custom-model deletion prune orphan per-model cooldowns (`pruneOrphanModelCooldownsLocked`).

### Free models / zen

`model="free"` walks `freeModelChain` across accounts until one succeeds, else HTTP 429 (only account-unavailable/429 errors continue the walk; other upstream errors abort it). Zen (opencode) models go through `callZenAPI` (`zen.go`) over a separate uTLS Chrome-120 + HTTP/2 client (`zen_proxy.go` — chat calls only; the model-list sync uses a plain client) with an egress proxy pool (http/https/socks5/socks5h) and per-request identity rotation (`freshZenIdentity` — new session/request id + UA). Consecutive zen failures trip a failover state machine that temporarily routes free models back to the Cline pool. Model `Source`: `remote` (synced at startup — replaces only other `remote` entries, never deletes user custom models), `zen`, `seed`, or empty = user custom; `getAllModels` dedupes by ID with first-seen priority (custom → remote → zen, custom → builtin fallback), and after each sync `validateDefaultModelAfterSync` clears a now-missing `DefaultModel`; in `routeModel`, any pool entry with the same ID beats zen.

### Admin panel

`admin.go` mounts 30 JSON endpoints under `/admin/api/*` (`apiResponse{success,data,error,message}`); auth via in-memory session cookie if `AdminPasswordHash` is set, otherwise open (loopback only — non-loopback is 403 fail-closed until a password exists). `AdminPasswordHash` is PBKDF2-SHA256 (`pbkdf2-sha256$<iter>$<saltHex>$<hashHex>`, 600k iterations, self-contained single field); legacy single-round SHA-256 hashes migrate transparently on the next successful login. Changing the password requires the current password (`oldPassword`) and rejects empty passwords; the clear-password path is Go-internal only (used by tests/reset). Login is brute-force guarded per source IP: 5 consecutive failures → 5-min lockout (checked before password verification, 429 + Retry-After). `logout`/`accounts/export`/`open-external` are POST-only (405 otherwise; open-external takes `{"url"}` and allows only http/https). All admin responses carry security headers via `setAdminSecurityHeaders` (nosniff, X-Frame-Options DENY, Referrer-Policy, no-store, CSP) — iframing the panel is blocked by design. All admin POSTs are CSRF-guarded by `adminSameOrigin` (Sec-Fetch-Site → Origin/Referer fallback vs `r.Host`/`X-Forwarded-Host`; absent headers = non-browser, allowed) — behind a reverse proxy it must forward `Host` (or `X-Forwarded-Host`) and `X-Forwarded-Proto`, or same-origin checks will reject valid requests; login/logout cookies carry `Secure` only over HTTPS. Generated API keys are `"cline_" + randomHex(32)` compared with `subtle.ConstantTimeCompare`; `randomHex` returns an error on RNG failure and callers fail closed (500) rather than mint empty tokens/keys. Upstream header overrides via the config endpoint are validated (RFC 7230 token names; Authorization/Content-Type/Host/Content-Length rejected). Log lines sanitize client-controllable fields (`sanitizeLog` in `http.go`) and mask emails (`truncateEmail`). The entire frontend is one Go string const `adminHTML` in `admin_html.go` (~126KB vanilla HTML/CSS/JS, **no build step**) — `frontend/` is a vestigial empty scaffold; edit the UI in `admin_html.go`. Locale (zh/en) resolved in `i18n.go` from cookie → `Accept-Language`.

### Persistence

`resolveDataPath` (`pool.go`) search order: exe dir → cwd → `~/.cline2api/` — existing files first; **new files go to `resolveDataDir()`** (`pool.go`, cached `sync.Once`): the first *writable* of exe dir → cwd → `~/.cline2api/` (created 0700 if needed), so a read-only exe directory no longer loses writes. All state is JSON files, mode 0600, written atomically (tmp+rename via `writeFileAtomic` — fixed tmp path, so all writes to one file must serialize under its mutex), gitignored (plaintext tokens): `.cline-accounts.json` (pool, API keys, models, admin hash), `.cline-config.json` (pool selection strategy + upstream header spoofing), `.cline-zen.json` (zen config + proxies), `.cline-request-logs.json` (ring buffer 5000 entries / 30 days), `.cline-credentials.json` (legacy single account). Pool and request-log hot paths flush via a dirty flag + 1s debounced merge (`debounce.go`; failed writes keep the dirty flag for retry), so a hard kill loses at most ~1s of usage/cooldown/cursor state — admin/config operations still write synchronously. Unparseable state files are quarantined as `<name>.bad` (`quarantineFile`) and defaults are used — never silently overwritten. Admin sessions, OAuth device flows (30-min TTL), zen failover counters, and compaction state are memory-only.

### Background jobs

`startModelSync` (startup, Cline model list), `startZenModelsRefresher` (10 min, only when zen is enabled), `startCooldownRecovery` (30s), `startCompactCleanup` (30 min: purge >24h compaction state + sweep expired OAuth/admin sessions).

## Conventions

- Comments are largely **Chinese**; log lines are English with a two-space indent prefix; commit messages are mixed Chinese/English following `feat(scope):` / `fix(scope):` / `docs:` / `test:` (also `perf:`/`refactor:`).
- Errors: typed error structs (`clineAPIError`, `clineAccountUnavailableError`, `freeModelUnavailableError`, `zenAPIError`, `tokenRefreshRejectedError`) — all but the latter two implement `Unwrap`-friendly wrapping; client-visible status comes from `upstreamErrorHTTPStatus` (zenAPIError/clineAPIError status ≥400 passes through, `freeModelUnavailableError`→429, else 500) via `writeUpstreamError`, which also backfills `Retry-After` on 429 (parsed from the raw error text). Client error *messages* are minimized via `upstreamClientMessage` — generic status-code/ fixed wording only; upstream bodies and account emails stay in the admin request logs. Port-in-use at startup aborts with `ensurePortFree` (names the owning process, no force-kill).
- Tests live next to sources in package `main`; upstreams are faked by swapping `httpClient.Transport` with `freeModelRoundTripper` (protocol tests boot the real proxy on a local port instead). `TestMain` (`zen_test.go`) redirects `poolPath`/`requestLogsPath` to a temp dir, so tests never touch real data files.
- Extending: new upstream provider = new file with config struct + `getXConfig`/`setXConfig`, seed model table, sync func, a case in `routeModel`, a `callXAPI` + stream handlers. New endpoint = handler in `proxy.go` + `mux.HandleFunc` in `startProxy`.

## Working rules

- Verify before asserting: ground every claim in this repo's code/docs (Grep/Read) first; fall back to web search only when the repo can't answer. Never present guesses as facts — label inferences as inferences.