# URL Shortener API: Roadmap

Companion to `tech-stack.md`. Each phase ends with a runnable, tested binary.
Finish a phase before starting the next; later phases assume the earlier ones.

Local toolchain on 2026-11-02: Go 1.27.3, sqlc, goose, task, golangci-lint, docker present. `air` not installed (optional).

## Product scope

Version 1 does one thing well: turn a long URL into a short code and redirect visitors.

| Capability | v1 | Later |
|---|---|---|
| Create short link from URL | yes | |
| Redirect by code | yes | |
| Custom alias | yes | |
| Expiry (`expires_at`) | yes | |
| Delete / deactivate link | yes | |
| Click counting | yes (total) | per-day, referrer, country |
| API key auth | yes | OIDC / user accounts |
| Rate limiting | yes | per-key quotas |
| OpenAPI docs | yes | |
| Redis cache for hot redirects | | yes |
| QR code | | yes |
| Bulk create | | yes |

## API surface

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/v1/links` | API key | Create link. Body: `url`, optional `alias`, `expires_at`. Returns `code`, `short_url`. |
| GET | `/api/v1/links/{code}` | API key | Link details plus `click_count`. |
| DELETE | `/api/v1/links/{code}` | API key | Soft delete (sets `deleted_at`). Redirect returns 404 afterwards. |
| GET | `/api/v1/links` | API key | List caller's links. Cursor pagination. |
| GET | `/{code}` | none | 302 redirect to target. 404 unknown, 410 expired or deleted. |
| GET | `/healthz` | none | Liveness. |
| GET | `/readyz` | none | Readiness (pings Postgres). |
| GET | `/docs`, `/openapi.json` | none | Served by Huma. |

Redirect uses 302, not 301, so browsers do not cache it and every visit is counted.

## Data model

```sql
links (
  id          bigint generated always as identity primary key,
  code        text not null unique,        -- 7 chars base62, or custom alias
  target_url  text not null,
  api_key_id  bigint not null references api_keys(id),
  click_count bigint not null default 0,
  expires_at  timestamptz,
  created_at  timestamptz not null default now(),
  deleted_at  timestamptz
)

api_keys (
  id          bigint generated always as identity primary key,
  key_hash    bytea not null unique,       -- sha256 of the raw key
  name        text not null,
  created_at  timestamptz not null default now(),
  revoked_at  timestamptz
)

clicks (                                   -- phase 5
  id          bigint generated always as identity primary key,
  link_id     bigint not null references links(id),
  occurred_at timestamptz not null default now(),
  referrer    text,
  user_agent  text
)
```

Code generation: 7 random base62 chars from `crypto/rand` (about 3.5e12 combinations), insert, retry up to 3 times on unique violation. Random codes are not guessable, unlike base62 of the sequence id. Custom aliases: 3 to 32 chars, `[A-Za-z0-9_-]`, reserved words (`api`, `docs`, `healthz`, `readyz`, `openapi.json`) rejected.

## Phases

### Phase 0: Skeleton (half a day) - DONE 2026-11-02

Goal: `task run` starts a server that answers `/healthz`.

- `go mod init` with `go 1.27`.
- Layout from `tech-stack.md`: `cmd/api`, `internal/config`, `internal/server`, `internal/db`, `internal/link`, `migrations`, `deploy`, `test/integration`.
- `internal/config`: `caarlos0/env/v11` struct. `PORT`, `DATABASE_URL`, `BASE_URL`, `LOG_LEVEL`, `SHUTDOWN_TIMEOUT`.
- `internal/server`: chi router, `http.Server` with read/write/idle timeouts, graceful shutdown on `signal.NotifyContext`.
- Middleware: request id, `slog` request logger, recover, request timeout.
- `log/slog` JSON handler, level from config.
- `Taskfile.yml`: `run`, `build`, `test`, `lint`, `migrate`, `sqlc`, `up` (docker compose).
- `.golangci.yml` with the modernize analyzer enabled.
- `deploy/docker-compose.yml`: Postgres 17.

Done when: `task up && task run` then `curl localhost:8080/healthz` returns 200, `task lint` is clean.

Status: complete. Module path `github.com/vanthao03596/go-backend`. Uses stdlib `uuid` (v7) for request ids and `encoding/json/v2` for responses. `.env` is created from `.env.example` and git-ignored.

### Phase 1: Database layer (half a day) - DONE 2026-11-02

Goal: schema exists, typed queries compile, pool wired in `main.go`.

- `migrations/0001_init.sql`: `api_keys`, `links`, indexes on `links(code)` where `deleted_at is null`, `links(api_key_id, created_at)`.
- `internal/db/db.go`: `pgxpool.New` with config, health ping for `/readyz`.
- Embed migrations and run goose at startup behind a `MIGRATE_ON_START` flag, plus a `task migrate` target.
- `sqlc.yaml` targeting pgx/v5, output to `internal/db/sqlc`. Commit generated code.
- Queries: `CreateLink`, `GetLinkByCode`, `ListLinksByAPIKey`, `SoftDeleteLink`, `IncrementClick`, `GetAPIKeyByHash`.
- `task sqlc-check` runs `sqlc diff` for CI.

Done when: `task migrate` applies cleanly to the compose Postgres, `go build ./...` passes without sqlc installed.

Status: complete. Notes from implementation:

- Migrations are embedded via `migrations/embed.go` and applied with goose's `NewProvider` API over a `database/sql` handle borrowed from the pgx pool (`stdlib.OpenDBFromPool`). No second connection string.
- `MIGRATE_ON_START=true` in `.env` for local dev. Production should run `task migrate` as a release step and keep the flag false.
- sqlc reads the goose files directly and skips `Down` sections. Filenames are zero-padded (`00001_`) because sqlc orders them lexicographically.
- Row-comparison cursor queries need explicit casts on `sqlc.arg(...)` or sqlc infers the wrong parameter type.
- Compose Postgres is published on host port 5434 (override with `POSTGRES_HOST_PORT`). Ports 5432 and 5433 are taken on the development machine by a Homebrew Postgres and another project's container.
- `/readyz` returns 503 with `postgres unavailable` when the pool cannot ping.

### Phase 2: Core link feature (1 day) - DONE 2026-11-02

Goal: create and redirect work end to end.

- `internal/link/link.go`: domain types, `ErrNotFound`, `ErrExpired`, `ErrAliasTaken`, `ErrInvalidURL`.
- `internal/link/service.go`: URL validation (http/https only, no private hosts, max 2048 chars), code generation with collision retry, alias rules, expiry check.
- `internal/link/repo.go`: thin wrapper over sqlc, maps pgx errors to domain errors.
- Huma on top of chi for `/api/v1/*`. Request and response structs carry validation tags. Error envelope via Huma's RFC 9457 problem details.
- Plain chi handler for `GET /{code}` (no Huma, it is a redirect, not JSON). Increment click count in the request path for now.
- `service_test.go`: table tests with a fake repo. Cover code retry, alias validation, expiry, URL rejection.

Done when: create via `/docs`, open the short URL in a browser, land on the target, `click_count` increments.

Status: complete. Notes from implementation:

- Huma is mounted on a chi sub-router at `/api/v1`, so docs live at `/api/v1/docs` and the spec at `/api/v1/openapi.json`. `Servers[0]` is set to `BASE_URL + /api/v1` so the generated paths resolve.
- The redirect at `GET /{code}` is a plain chi handler. chi gives static routes priority, so `/healthz`, `/readyz` and `/api/...` are never shadowed by a code.
- Redirect lookup returns deleted rows so the handler can answer 410 instead of 404. Deleted and expired both map to 410.
- Until phase 3, the management API is authorised by `DEV_API_KEY_ID` (dev only, rejected by config in other envs). `task dev:seed` creates the matching row. Without it every `/api/v1` call returns 401 and redirects still work.
- Features register through `server.WithFeature(func(root chi.Router, api huma.API))`; auth middleware for the API group goes through `server.WithAPIMiddleware`.
- Known Huma quirk: with a `localhost` Host header the `$schema` link doubles the `/api/v1` prefix. With any real host it is correct. Cosmetic only.
- Client-facing timestamps are rendered in UTC.
- After code review: unmapped handler errors are logged with the request id and returned as a generic 500; `HEAD /{code}` is served and does not count a click; the click increment runs on a detached context so a client disconnect cannot cancel it; migration 00002 drops the partial live-code index the redirect query no longer matches; startup fails fast if `DEV_API_KEY_ID` has no active row; host validation rejects IPv4 shorthand (`127.1`, `0x7f.0.0.1`), CGNAT, NAT64 and IPv4-mapped IPv6.
- Deleted codes stay reserved: the unique constraint on `code` spans soft-deleted rows, so an alias cannot be reused after deletion. This matches the roadmap default.

### Phase 3: Auth and rate limiting (half a day) - DONE 2026-11-02

Goal: `/api/v1/*` requires a key, abuse is throttled.

- `internal/apikey`: service that hashes the `Authorization: Bearer <key>` header with sha256 and looks it up. Constant-time compare not needed since the hash is the lookup key, but do not log raw keys.
- Middleware on the `/api/v1` chi group that sets the key id in context. Handlers scope every query by `api_key_id`.
- `cmd/api` subcommand or `task apikey:create -- name` that inserts a key and prints it once.
- `httprate`: 10 req/s per IP on `/api/v1/links` POST, 100 req/s per IP on redirects. Trust `X-Forwarded-For` only when `TRUSTED_PROXY=true`.
- Security headers on redirect responses: `Referrer-Policy: strict-origin-when-cross-origin`, `Cache-Control: no-store`.

Done when: unauthenticated POST returns 401, another key's link returns 404 on GET/DELETE, burst requests return 429.

Status: complete. Notes from implementation (library docs checked against chi v5.5.0, httprate v0.16.1, Huma v2.40.0):

- Keys are `sk_` plus 256 random bits in unpadded base64url (46 chars). Only the SHA-256 hash is stored. `apikey.Service.Authenticate` rejects anything without the prefix or over 128 chars before touching the database.
- `internal/apikey.Bearer` is the chi middleware on the `/api/v1` group. Missing, malformed, unknown and revoked keys all return the same 401 with `WWW-Authenticate: Bearer` and a problem-details body. Backend failures are logged with the request id and masked as 500. Raw keys are never logged.
- Huma's docs routes (`/api/v1/docs`, `/api/v1/openapi.{json,yaml}`, `/api/v1/schemas/*`) bypass the API middleware via `chi/middleware.Maybe` so the spec stays public. The spec declares a `bearerAuth` scheme and applies it to every operation.
- `DEV_API_KEY_ID`, `apikey.Static` and `task dev:seed` are gone. Issue keys with `task apikey:create -- <name>` (a `cmd/api apikey create` subcommand). The raw key is printed to stdout once; logs for that subcommand go to stderr.
- httprate v0.16.1 deprecates `LimitByIP`, `LimitByRealIP` and `Limit`; the supported entry is `LimitBy` keyed off chi's `middleware.ClientIPFrom*` resolvers. The roadmap's `TRUSTED_PROXY=true` became `TRUSTED_PROXIES` (comma-separated CIDRs) because trusting `X-Forwarded-For` from any peer is spoofable and the library refuses to offer that mode. Empty means the TCP peer is the client (`ClientIPFromRemoteAddr`). Both resolvers fail closed; a request whose IP cannot be resolved is rejected with 400 rather than pooled into a shared bucket one bad actor could exhaust. IPv6 clients are bucketed by /64.
- Limits: `RATE_LIMIT_CREATE_PER_SEC` (default 10) on `POST /api/v1/links` only, installed before `Bearer` so anonymous bursts never cost a key lookup. `RATE_LIMIT_REDIRECT_PER_SEC` (default 100) wraps `GET` and `HEAD /{code}` only, so health checks and the management API are never throttled by redirect traffic. 429 responses carry `Retry-After` and `X-RateLimit-*` headers and a problem-details body.
- Redirect security headers (`Cache-Control: no-store`, `Referrer-Policy: strict-origin-when-cross-origin`) were already in place from phase 2.
- The request log line now includes `client_ip`.

### Phase 4: Integration tests and CI (half a day) - DONE 2026-11-02

Goal: the full stack is tested against a real Postgres on every push.

- `test/integration`: testcontainers-go `modules/postgres`, run goose migrations, start the server with `httptest`, exercise create, get, redirect, delete, expiry, auth failure.
- `testing/synctest` for expiry tests instead of sleeping.
- GitHub Actions: `go vet`, `golangci-lint`, `sqlc diff`, `go test -race ./...`, build image.
- `Dockerfile`: multi-stage, distroless or scratch, non-root, `CGO_ENABLED=0`.

Done when: CI is green and `docker run` of the image serves `/healthz`.

Status: complete locally (`task ci` is green, the image answers `/healthz` and `/readyz` against the compose Postgres). CI on GitHub has not run yet because the workflow has not been pushed. Notes from implementation (library docs checked against testcontainers-go v0.44.0, golangci-lint-action v9, setup-sqlc v5, Go 1.27 release notes):

- `internal/app` is the composition root. `cmd/api` and the integration tests both call `app.New(cfg, log, pool, opts...)`, so tests exercise the wiring that ships rather than a copy of it. `app.WithClock` is the only test hook.
- Integration tests live behind `//go:build integration` in `test/integration`. `TestMain` starts one `postgres:17-alpine` container (same image as compose) with `postgres.BasicWaitStrategies()`, opens a pool and applies the embedded migrations through `db.Migrate`. Every test then builds its own server with `httptest.NewUnstartedServer` so `BASE_URL` can be the listener's real address and the `short_url` the API returns is directly requestable. Tests isolate themselves by issuing their own API key; codes are random, so they run with `t.Parallel()` on one database. The whole suite takes about three seconds plus container start.
- `testing/synctest` cannot wrap the real-Postgres tests: the package documentation says bubbles must avoid the network, and a goroutine blocked on a socket is not "durably blocked", so fake time would never advance. The expiry test against Postgres therefore moves an injected clock (`link.WithNow`) forward. `synctest` is used where it fits: `TestRedirectExpiresUnderFakeClock` in `internal/link` drives the real chi and Huma stack over `httptest.NewTestServer`'s in-memory network with the service's default `time.Now`, and `synctest.Sleep(time.Hour)` expires the link instantly.
- Rate limits default to 1000 per second in the test config so ordinary tests never trip them; `TestCreateRateLimit` lowers the create limit to 2 and asserts the third anonymous POST is a 429 before any key lookup.
- `.github/workflows/ci.yml` runs four parallel jobs: lint (`go vet -tags=integration`, `golangci-lint-action@v9` pinned to v2.15), sqlc drift (`setup-sqlc@v5` pinned to 1.31.1, `sqlc diff`), test (unit, then integration; Docker is available on `ubuntu-latest` so testcontainers needs no setup) and image (buildx with GHA layer cache, `push: false`, then `docker run --network host` against a `postgres:17-alpine` service and curl `/healthz` and `/readyz`). Nothing is pushed to a registry; add a login and `push: true` on `main` when a registry is chosen.
- `Dockerfile`: `golang:1.27.3-trixie` builder with `--platform=$BUILDPLATFORM` and `GOOS`/`GOARCH` from buildx so multi-arch builds cross-compile instead of emulating; module and build caches via `--mount=type=cache`. Runtime is `gcr.io/distroless/static-debian13:nonroot` (uid 65532, no shell, ca-certificates and tzdata included). The image is about 5.5 MB. The Debian 12 distroless tags are deprecated upstream; `tech-stack.md` has been corrected.
- `.golangci.yml` sets `run.build-tags: [integration]` so the integration package is linted too. Taskfile gained `test:all`, `image:build`, `image:run` and `ci`.

### Phase 5: Async click analytics (1 day) - DONE 2026-11-02

Goal: redirects stay fast under load and analytics are richer.

- Move `IncrementClick` out of the redirect request path. Start with a buffered channel plus a single flusher goroutine that batches `UPDATE ... SET click_count = click_count + n`. Drain on shutdown.
- Add `clicks` table and migration. Record `referrer` and truncated `user_agent`. No IP storage by default.
- `GET /api/v1/links/{code}/stats`: total, last 7 days by day.
- If batching in-process proves insufficient, swap the flusher for River with a `RecordClick` job. River reuses the pgx pool, no new infrastructure.
- Nightly job (River periodic or cron) to purge clicks older than 90 days.

Done when: redirect p99 under load test (`hey` or `vegeta`) is unchanged with or without analytics enabled.

Status: complete. Notes from implementation (library docs checked against pgx v5.12.0, sqlc v1.31.1, Huma v2.40.0, Go 1.27 `testing/synctest`):

- `internal/click` is the new bounded context. `link.Service.Resolve` no longer writes anything: it hands a `link.Click` (link id, timestamp, referrer, user agent) to a `link.ClickSink`, and `click.Buffer` is that sink. `link.Repository.IncrementClicks` and the `IncrementClick` query are gone; `click` owns every write to the counter.
- `click.Buffer` is a bounded channel (`CLICK_BUFFER_SIZE`, default 10000) drained by one goroutine. It flushes every `CLICK_FLUSH_INTERVAL` (1s) or as soon as `CLICK_FLUSH_BATCH` (500) clicks are pending. One flush is one transaction: `COPY` into `clicks` through sqlc's `:copyfrom` (which adds `CopyFrom` to the generated `DBTX`, satisfied by both the pool and `pgx.Tx`), then one `UPDATE ... FROM unnest(ids, deltas)` for the counters. `Record` never blocks: a full buffer drops the click and the drop count is logged once per flush. A failed flush keeps its batch and retries on the next tick, up to three attempts, then drops it; while a batch is held, new clicks accumulate up to `CLICK_BUFFER_SIZE` and the size trigger is suspended so a sick database is not hammered. A deterministic failure (for example a click whose link row was hard-deleted, which the FK rejects) therefore costs the batches it lands in; links are only ever soft-deleted so this does not happen in practice. River is the upgrade path if delivery guarantees become a requirement. The flusher goroutine starts on the first click, so building the app without serving traffic leaks nothing.
- Header values are sanitised before they reach Postgres: invalid UTF-8 and NUL bytes are stripped (Postgres `text` rejects both, which would fail a whole batch) and values are cut on a rune boundary at 2048 bytes for the referrer and 512 for the user agent. The referrer is reduced to scheme, host and path: query strings, fragments and user info routinely carry tokens and addresses, so they are never stored, and anything that is not an absolute http(s) URL is discarded. The table has matching `CHECK` constraints. No client address is stored.
- `server.Server` grew `WithWorker` and `WithShutdownHook` plus a public `Shutdown(ctx)`. `Run` starts workers next to the listener; `Shutdown` drains HTTP and cancels workers under `SHUTDOWN_TIMEOUT`, then runs hooks in order under a second `SHUTDOWN_TIMEOUT` of their own so a slow client cannot starve the click flush. The click buffer's `Close` is a shutdown hook, so every redirect that answered 302 has enqueued its click before the drain. Integration tests call `Shutdown` from `t.Cleanup`.
- Stats: `GET /api/v1/links/{code}/stats` returns `total_clicks` (the link's lifetime counter, which survives purges) and `days`, seven UTC calendar days oldest first, zero-filled, from `clicks` grouped by `(occurred_at AT TIME ZONE 'UTC')::date`. The `date` column is mapped to `time.Time` with an sqlc override. Ownership is checked through `link.Service.Get`, so `click` depends on `link` and never the reverse. Counts lag by at most one flush interval; the integration tests poll for them.
- Retention: `CLICK_RETENTION` (default 2160h, 90 days). `click.Service.Purge` deletes in batches of 5000 by `ctid` so a backlog never holds one long lock; `clicks(occurred_at)` is indexed for it and `clicks(link_id, occurred_at)` for stats. The in-process purger runs one minute after start and then every `CLICK_PURGE_INTERVAL` (default 24h); set it to 0 and schedule `task clicks:purge` (`api clicks purge`) externally instead. River was not adopted: a ticker plus an idempotent `DELETE` covers the requirement without a job table.
- Tests: the buffer and purger are tested under `testing/synctest` (fake tickers, no sleeps). Integration tests cover stats, header capture, the day boundary via the injected clock, scoping, and purge against real Postgres.
- Load test (2026-11-02, MacBook, compose Postgres 17 in Docker, `hey -z 15s -c 50`, one hot code, previous commit versus this one, two rounds each):

  | Binary | Requests/s | p50 | p90 | p99 | Slowest |
  |---|---|---|---|---|---|
  | Phase 4 (synchronous `UPDATE` per redirect) | 4.4k | 10.4 ms | 14.5 ms | 22.0 ms | 91 ms |
  | Phase 5 (buffered flush) | 20.4k | 2.4 ms | 2.9 ms | 4.0 ms | 24 ms |

  All 302s were counted: 675191 redirects served by the new binary produced exactly 675191 `clicks` rows, with no drop or flush-error log lines, and the lifetime counter reconciled with the old binary's share.

### Phase 6: Observability (half a day)

Goal: traces and metrics for every request.

- `internal/platform/otel`: tracer and meter providers, OTLP gRPC exporter, resource with service name and version.
- `otelhttp` middleware on the router. Instrument pgx with `otelpgx` or manual spans around repo calls.
- Metrics: redirect count by status, link creations, code collision retries, click flush batch size.
- `deploy/docker-compose.yml`: add otel-collector and Jaeger or Grafana Tempo for local viewing.
- `slogx` handler that injects `trace_id` into every log line.

Done when: a redirect shows up as a trace with HTTP and DB spans in the local UI.

### Phase 7: Cache and hardening (optional, 1 day)

Add only when redirect latency or DB load warrants it.

- Redis via `go-redis/v9`: cache `code -> target_url, expires_at` with TTL. Invalidate on delete. Negative-cache 404s for 60 s.
- Pre-generate codes into a `code_pool` table if collision retries become measurable.
- Pagination cursors signed to prevent tampering.
- Export `api/openapi.yaml` from Huma in CI and fail on uncommitted diff.
- Load test and document numbers in `docs/performance.md`.

## Decisions (locked 2026-11-02)

| # | Decision | Choice | Rationale |
|---|---|---|---|
| 1 | Code length | 7 chars base62 | About 3.5e12 combinations, comfortable to 1e9 links. Revisit to 8 if growth projections exceed that. |
| 2 | Redirect status | 302 always | Accurate click counts, links can be updated or deleted. Latency cost accepted. |
| 3 | Auth | API keys only | Callers are scripts and services. OIDC deferred until user accounts are needed. |
| 4 | Huma scope | Huma for `/api/v1`, plain chi for `/{code}` and health | Docs and validation where JSON is exchanged, thin handlers on the hot path. |
| 5 | Click counting | Synchronous in Phase 2, async in Phase 5 | Correct first, then measure with a load test before optimizing. |

## Suggested timeline

| Week | Phases |
|---|---|
| 1 | 0, 1, 2 |
| 2 | 3, 4 |
| 3 | 5, 6 |
| 4 | 7 if needed, else polish and docs |

## Open questions

- Will the redirect domain differ from the API domain (for example `s.example.com` versus `api.example.com`)? If yes, split into two chi routers behind one server keyed on `Host`, or two binaries sharing `internal/`.
- Should deleted codes be reusable? Default no, the unique index stays across soft deletes.
- Retention period for clicks. Default 90 days.
