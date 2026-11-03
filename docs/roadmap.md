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
| Redis cache for hot redirects | yes (phase 7, optional via `REDIS_URL`) | |
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

### Phase 6: Observability (half a day) - DONE 2026-11-03

Goal: traces and metrics for every request.

- `internal/platform/otel`: tracer and meter providers, OTLP gRPC exporter, resource with service name and version.
- `otelhttp` middleware on the router. Instrument pgx with `otelpgx` or manual spans around repo calls.
- Metrics: redirect count by status, link creations, code collision retries, click flush batch size.
- `deploy/docker-compose.yml`: add otel-collector and Jaeger or Grafana Tempo for local viewing.
- `slogx` handler that injects `trace_id` into every log line.

Done when: a redirect shows up as a trace with HTTP and DB spans in the local UI.

Status: complete. Notes from implementation (docs and sources checked against otel-go v1.47.0, otelhttp v0.72.0, otelpgx v0.13.0, chi v5.5.0, Collector 0.164.0, Jaeger 2.22.0, Prometheus v3.15.1):

- The package is `internal/platform/otelx`, not `platform/otel`. It is named like `slogx` so it can be imported next to `go.opentelemetry.io/otel` without an alias. `otelx.Setup` follows the official getting-started bootstrap. It builds tracer and meter providers with OTLP gRPC exporters, installs the W3C trace-context and baggage propagators and the globals, starts runtime metrics, and routes SDK errors (failed exports) to slog. The exporters dial lazily, so a missing collector never fails startup.
- Resource attributes:
  - `service.name` is `shortener`.
  - `service.version` comes from `-X main.version`, which `task build`, `task image:build` and CI pass, or else from the build info.
  - `service.instance.id` is a UUIDv7 per process, so replicas never share a metric series.
  - `deployment.environment.name` is derived from `APP_ENV`.
  - `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` override all of these. Every other knob (endpoint, headers, sampler, export interval) is a standard `OTEL_*` variable read by the SDK and is not duplicated in `config`.
- `TELEMETRY_ENABLED` (default false) is an app flag because the Go SDK does not implement `OTEL_SDK_DISABLED`. When it is off, every component gets no-op providers and no global state changes. The operator subcommands are never traced. Telemetry shuts down after the HTTP drain and the click-buffer flush, so the last flush is exported. Its budget is `min(SHUTDOWN_TIMEOUT, 5s)`, because `Server.Shutdown` can already use twice `SHUTDOWN_TIMEOUT`; with the 10s default the worst case is 25s, inside a 30s Kubernetes grace period. A failed final export is logged, not fatal.
- Providers are passed explicitly. The entry points are `app.WithTelemetry`, `server.WithTelemetry`, `link.WithMeterProvider`, `click.BufferConfig.{Tracer,Meter}Provider`, `click.WithTracerProvider` and `db.PoolOptions.{Tracer,Meter}Provider`. Nil falls back to the otel globals. Tests inject in-memory providers from `internal/platform/otelx/otelxtest`.
- HTTP: `otelhttp.NewHandler` wraps the chi router.
  - otelhttp names the span and sets `http.route` from `Request.Pattern` after the handler returns. chi v5.5.0 sets `Pattern` only on its own copy of the request, so out of the box every span was named `GET` and `http.server.request.duration` had no route. `middleware.TraceRoute`, first on the chi router, fixes this from chi's route context. It renames the span `"{method} {pattern}"` and adds `http.route` to the span and, through otelhttp's `Labeler`, to the request metrics. Unmatched requests keep the method-only name, so they cannot inflate cardinality.
  - `/healthz` and `/readyz` are not traced or measured.
  - Incoming `traceparent` is honoured only when the TCP peer is in `TRUSTED_PROXIES`. Any other request starts a new trace linked to the caller's (`WithPublicEndpointFn`), so an internet client cannot force sampling or graft spans onto a trace. The server extracts trace context only, never W3C baggage, from any peer: `WithPublicEndpointFn` drops only the parent, and the service has no use for caller-controlled key-values. The global propagator still includes baggage for libraries that use it.
- Database: an otelpgx tracer on `ConnConfig.Tracer` covers query, prepare, copy, connect and pool-acquire spans plus `db.client.operation.duration`, and `otelpgx.RecordStats` adds `pgxpool.*` gauges.
  - Query parameters are not recorded, because they hold target URLs and user agents. Statements are recorded and contain only placeholders.
  - otelpgx opens spans only under a recording parent. The click flusher therefore starts a root `click.flush` span per attempt (size, attempt, error status) and the scheduled purger a root `click.purge` span. Their logs carry the trace id too.
  - The first use of a statement on a connection shows up as two spans with the same name; the inner one is pgx's automatic prepare. `CopyFrom` also prepares a `SELECT` to learn column types, and the copy span is named `copy_from "clicks"` (pgx quotes the identifier).
- Logs: `slogx.New` wraps the handler so any record logged with a span in its context gets `trace_id` and `span_id`. This includes the request log line, because otelhttp runs outside the chi middleware stack.
- Metrics (Prometheus names in brackets):
  - `shortener.redirects` [`shortener_redirects_total`], by `http.response.status_code`. GET and HEAD, all of 302, 404, 410 and 500.
  - `shortener.links.created`, by `shortener.link.custom_alias`.
  - `shortener.links.code_collisions`.
  - `shortener.clicks.flush.size`, a histogram by `shortener.clicks.flush.outcome` (stored, retry, dropped).
  - `shortener.clicks.dropped`, by `shortener.clicks.drop_reason`. Drops were previously only logged. The reasons:
    - `buffer_full`: traffic outran the flusher.
    - `retry_backlog_full`: a failed batch held for retry reached `CLICK_BUFFER_SIZE`, so the database is the problem.
    - `flush_failed`: a batch was given up after three attempts.
    - `closed`: the click was recorded after shutdown began.
  - Alongside: otelhttp's `http.server.request.duration` and body sizes by `http.route`, otelpgx's database and pool metrics, and Go runtime metrics.
  - A counter shows no series until its first increment.
- Local stack: `task otel:up` starts the compose profile `observability` next to Postgres, and `task otel:down` removes it. `task down` removes everything, volumes included. All ports are bound to loopback: 4317/4318 (OTLP), 16686 (Jaeger UI) and 9090 (Prometheus).
  - The Collector is the core image; it has every component used here.
  - Since Collector 0.144.0 the exporter is called `otlp_grpc` (`otlp` is a deprecated alias). The `batch` processor is being retired in favour of the exporter's `sending_queue` batching. Receivers must bind `0.0.0.0` explicitly.
  - Jaeger 2.22 runs the all-in-one with in-memory storage and receives OTLP from the Collector. Its HTTP API is `/api/v3/...`; the old `/api/traces` returns 404.
  - Prometheus scrapes the Collector's prometheus exporter with `honor_labels`, so `job` is the service name.
- Tests:
  - Unit tests cover route naming for chi and Huma routes, the route label on request metrics, the health-probe filter, trace-context trust by peer, the link and click metrics and spans (the buffer under `testing/synctest`), the slog trace handler, resource precedence, and `Setup` without a reachable collector.
  - `TestRedirectIsTraced` (integration) asserts against real Postgres that a redirect yields `GET /{code}` with a `SELECT` below it in the same trace, that the click arrives through a `click.flush` trace with `copy_from "clicks"`, and that the metrics were recorded.
- Verified 2026-11-03 on the Linux workstation:
  - With `task otel:up` and `TELEMETRY_ENABLED=true`, a redirect appears in Jaeger as `GET /{code}` (302, `http.route=/{code}`) with `pool.acquire` and `SELECT` children. Its `trace_id` matches the request log line.
  - `click.flush` traces show BEGIN, `copy_from "clicks"`, UPDATE and COMMIT.
  - Prometheus has `shortener_redirects_total{http_response_status_code="302"}`, `http_server_request_duration_seconds` by `http_route`, `db_client_operation_duration_seconds`, `pgxpool_*` and `go_*`.
- Cost of tracing on the hot path. Measured with `hey -z 10s -c 50` on one hot code, same binary, redirect rate limit lifted, and the Collector and Jaeger on the same machine:

  | Telemetry | Requests/s | p50 | p90 | p99 |
  |---|---|---|---|---|
  | Off | 37.6k | 1.2 ms | 1.8 ms | 2.6 ms |
  | On, every trace sampled (SDK default) | 20.1k | 2.0 ms | 3.9 ms | 8.6 ms |
  | On, `OTEL_TRACES_SAMPLER=parentbased_traceidratio`, `OTEL_TRACES_SAMPLER_ARG=0.01` | 42.6k | 1.1 ms | 1.7 ms | 2.5 ms |

  Instrumenting is effectively free; exporting every trace is what costs. Production should set a ratio sampler. Metrics are unaffected by sampling.
- The stripped binary grew from 13.7 MB to 24.3 MB, mostly gRPC and protobuf from the OTLP gRPC exporters. The OTLP/HTTP exporters would avoid gRPC if image size starts to matter.

### Phase 7: Cache and hardening (optional, 1 day) - DONE 2026-11-03

Add only when redirect latency or DB load warrants it.

- Redis via `go-redis/v9`: cache `code -> target_url, expires_at` with TTL. Invalidate on delete. Negative-cache 404s for 60 s.
- Pre-generate codes into a `code_pool` table if collision retries become measurable.
- Pagination cursors signed to prevent tampering.
- Export `api/openapi.yaml` from Huma in CI and fail on uncommitted diff.
- Load test and document numbers in `docs/performance.md`.

Status: complete. The `code_pool` table was measured and not needed. Notes from implementation (docs and sources checked against go-redis v9.23.0, redisotel v9.23.0, Redis 8.10.3, testcontainers-go `modules/redis` v0.44.0, Huma v2.40.0, Go 1.27 `crypto/hmac`, vegeta v12.13.0):

- Cache design:
  - `link.Cache` stores a `Destination` per code: link id, target URL, expiry, and whether the link is deleted or missing.
  - `link.RedisCache` keeps one string key per code, `shortener:link:v1:<code>`, holding a small JSON document. The version in the key changes with the format.
  - TTLs are `LINK_CACHE_TTL` (default 10m) and `LINK_CACHE_NEGATIVE_TTL` (default 60s, for unknown codes).
  - Expiry is stored rather than folded into the TTL, so an expired link answers 410 from the cache. A deleted link is a tombstone entry.
  - An empty `REDIS_URL` disables the cache and every redirect reads Postgres.
- Coherence. Readers *fill*: after a database read they `SET NX`. Writers *put*: after a create or delete they `SET`. A fill racing a put therefore never overwrites the newer state.
  - Create writes the new entry. This warms the cache and replaces a cached 404 for a just-claimed alias.
  - Delete writes the tombstone.
  - Links have no update API, so these are the only writes.
- The cache fails open:
  - Any cache error means a database read. There is no fill after a failed get, since a failing cache would only fail again.
  - A failed tombstone write is queued, bounded at 10k. The `link cache repair` worker retries it every second until Redis takes it. That is always safe because deletion is final: codes are never reused.
  - Workers stop as soon as shutdown begins, before in-flight requests drain. A shutdown hook therefore makes one last attempt after the drain, which also covers deletes handled during it. Tombstones still unwritten are logged as an error, not returned: failing the shutdown would turn a clean exit into exit code 1. The queue lives in memory, so a crash loses it; the TTL bounds the damage.
  - A failed put after create is only logged. The worst case is a cached 404 for up to the negative TTL.
  - Redis is not part of `/readyz`, and a failed startup ping only logs.
- Redis reads an expiry of 0 as "never". `NewRedisCache` therefore replaces non-positive TTLs with the defaults. An integration test caught entries written without expiry when the config left TTLs at zero.
- `internal/platform/redisx` builds the client:
  - The go-redis defaults suit a primary store, not a cache in front of a 1 ms database: 5s dial, 5s read, 3 retries, 5 dial attempts per connection.
  - Unless `REDIS_URL` says otherwise, the defaults are 250ms dial, 100ms read and write, 100ms pool wait, no retries and one dial attempt.
  - Any option can be set as a URL query parameter (`?read_timeout=250ms`); `ParseURL` maps snake_case option names. An explicit 0 means no timeout, so only absent settings get the defaults.
  - Parse errors are stripped of the URL, which may hold a password.
  - The docs and the source disagree: redis.io says read and write default to 3 s and retry backoff to 8 to 512 ms, while v9.23.0's `options.go` says 5 s and 10 ms to 1 s.
- Circuit breaker (`redisx.Breaker`):
  - Five consecutive transport failures open it for one second. Then a single call probes.
  - Only a normal reply counts as success, including `redis.Nil` (a miss). Error replies count as failures: WRONGPASS, READONLY after a failover, or OOM mean every call is a wasted round trip. An integration test checks that a rejected password opens it. Cancelled callers count as neither.
  - It wraps whole cache operations through `link.GuardCache`.
  - The first version was installed as go-redis `Options.Limiter` and never recovered after an outage. go-redis consults the Limiter for every command, including the HELLO, AUTH and CLIENT SETINFO that a new connection runs inside the probing command, so the half-open breaker refused its own probe's handshake. The load test found it: the cache stayed bypassed after Redis came back. `TestCacheRecoversAfterAnOutage` starts against a black-holed proxy and fails with the Limiter version.
- Telemetry:
  - `redisotel` tracing and metrics hooks get explicit providers.
  - `WithDBStatement(false)` because values hold target URLs. `WithCallerEnabled(false)` because caller attribution walks the stack on every command.
  - Unlike otelpgx, redisotel opens spans without a recording parent. Its attributes use the older `db.system=redis`.
  - `extra/redisotel-native`, which emits the semconv `db.client.operation.duration`, is a process-wide singleton (`GetObservabilityInstance().Init`) and does not fit per-component providers.
  - go-redis's internal log lines go to slog through `redisx.RouteLogs`, which is process-wide.
  - New metrics: `shortener.links.cache.lookups` by `shortener.cache.result` (hit, miss, error) and `shortener.links.cache.write_errors` by `shortener.cache.operation` (fill, put). redisotel adds `db.client.connections.*`.
  - Verified in Jaeger. A cached redirect is `GET /{code}` with a single `get` child and no Postgres spans. An unknown code shows `get`, the `SELECT`, then a `set` for the negative entry.
- Not used: go-redis client-side caching (RESP3 tracking with an in-process copy) is in v9.23.0 but marked experimental.
- Local Redis is the compose service `redis:8.10-alpine`:
  - `maxmemory 128mb` and `allkeys-lru`, which Redis recommends for a pure cache. No persistence, so a restart can only empty the cache, never serve a stale entry.
  - Published like Postgres so `task image:run` reaches it through `host.docker.internal`, hence the development password `shortener`.
  - Redis 8 is licensed RSALv2, SSPLv1 or AGPLv3. Valkey speaks the same protocol if that matters.
  - Integration tests start the same image with testcontainers.
- Code pool not built:
  - 10,000 creates under load produced 0 collisions, against an expected 1.4e-5.
  - At a billion links one create in about 3,500 needs a second `INSERT`.
  - Revisit if `shortener.links.code_collisions` passes about 1% of `shortener.links.created`. Decision 1 moves to 8 characters well before that.
- Cursors:
  - The format is base64url of version, `created_at` nanos, id and a tag, 44 characters. The tag is HMAC-SHA256 over a context string, the version, the caller's API key id and the position, truncated to 128 bits (RFC 2104 allows truncation to half the hash).
  - A cursor only works for the key it was issued to.
  - `CURSOR_SECRET` (at least 32 bytes) is required when `APP_ENV=prod`. Elsewhere a random per-process key is used and a warning is logged.
  - Cursors are signed, not encrypted: a client that decodes one sees the row's `created_at` and internal id. Encryption was considered. The stdlib's random-nonce AES-GCM caps one key at 2^32 messages; XChaCha20-Poly1305 from `x/crypto` would be the choice if ids ever need hiding.
  - Unsigned cursors issued before this change, and cursors issued before a key rotation, now get 422. The client starts again from page one.
- OpenAPI export:
  - `api openapi` prints the YAML. This is Huma's documented pattern: a CLI command around `api.OpenAPI().YAML()` that never starts the server.
  - It is built from `config.Defaults()`, an empty environment, so the output is the same for everyone. `app.OpenAPI` assembles the application without a pool or cache.
  - `task openapi` writes `api/openapi.yaml`, `task openapi:check` diffs, and the CI job "OpenAPI drift" regenerates the file and runs `git diff --exit-code`.
  - The output is byte-identical across runs. With the default `BASE_URL`, the served `/api/v1/openapi.yaml` equals the committed file.
  - `DATABASE_URL` moved from the env tag's `required` into `validate()` so `Defaults` can parse an empty environment.
  - The CI image smoke test now sets `CURSOR_SECRET`, since prod requires it.
- Load test: see `docs/performance.md`. Briefly:
  - The cache removes every database lookup from redirects. At 20k redirects/s, p99 drops from 13.9 ms to 1.3 to 5.1 ms, and Postgres CPU falls by about 60%.
  - A paused or stopped Redis costs no wrong answers and no more latency than having no cache.
  - Click recording, not lookups, is now the per-instance ceiling at about 28k to 30k clicks/s. See Open questions.
- The stripped binary grew from 24.3 MB to 32.5 MB. go-redis v9.23 carries the whole Redis Stack command set.
- Known issue, predates this phase and is not fixed: the `$schema` links in responses read `/api/v1/api/v1/schemas/...`, which answers 401. Huma prefixes the server URL, which already ends in `/api/v1`.

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

- The click pipeline records at most about 28k to 30k clicks/s per instance (one flusher, one Postgres backend; see `docs/performance.md`). With the cache, one instance can serve more redirects than that. Beyond adding replicas, choose one: several flush workers, counter deltas aggregated in memory and applied less often than rows are copied, or River (the phase 5 fallback).

- Will the redirect domain differ from the API domain (for example `s.example.com` versus `api.example.com`)? If yes, split into two chi routers behind one server keyed on `Host`, or two binaries sharing `internal/`.
- Should deleted codes be reusable? Default no, the unique index stays across soft deletes.
- Retention period for clicks. Default 90 days.
