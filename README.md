# URL Shortener API

[![CI](https://github.com/vanthao03596/go-backend/actions/workflows/ci.yml/badge.svg)](https://github.com/vanthao03596/go-backend/actions/workflows/ci.yml)

A URL shortener written in Go. It turns a long URL into a short code and redirects
visitors. Behind the API key–protected management API it also counts clicks
without slowing redirects, caches lookups in Redis, and exports OpenTelemetry
traces and metrics.

- **Short links:** random 7-character base62 codes or a custom alias, with optional expiry and soft delete.
- **Fast redirects:** `302` from a plain chi handler, with an optional Redis cache that falls back to Postgres if Redis fails.
- **Click analytics:** clicks are buffered in memory and written in batches with `COPY`. There is a per-link counter and 7-day stats, and old clicks are purged after a retention period.
- **API keys and rate limits:** Bearer keys stored as SHA-256 hashes, and per-client-IP limits that only honour `X-Forwarded-For` from trusted proxy CIDRs.
- **OpenAPI 3.1:** generated from the handler types by [Huma](https://huma.rocks), with interactive docs at `/api/v1/docs`. CI fails if `api/openapi.yaml` is out of date.
- **Observability:** OTLP traces and metrics, HTTP, Postgres and Redis spans, and `trace_id` in every log line. A local Collector, Jaeger and Prometheus stack is one command away.

Stack: Go 1.27, chi, Huma, pgx + sqlc, goose, go-redis, OpenTelemetry, testcontainers.
The reasoning behind each choice is in [docs/tech-stack.md](docs/tech-stack.md).

## Quick start

Requirements: Go 1.27.3+, Docker with Compose, and [Task](https://taskfile.dev).
If Task is not installed, prefix each command with `go run github.com/go-task/task/v3/cmd/task@v3.54.0` instead of `task`.

```sh
cp .env.example .env   # local settings; Task loads it automatically
task up                # Postgres on :5434, Redis on :6380
task run               # applies migrations (MIGRATE_ON_START=true) and serves on :8080
```

In another terminal, issue an API key. It is printed once and only its hash is stored:

```sh
KEY=$(task apikey:create -- my-script)
```

Create a link and follow it:

```sh
curl -s localhost:8080/api/v1/links \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://go.dev/doc/", "alias": "godoc"}'

curl -i localhost:8080/godoc   # HTTP/1.1 302 Found, Location: https://go.dev/doc/
```

Open <http://localhost:8080/api/v1/docs> to try every endpoint in the browser.

`task down` stops the containers and deletes their volumes.

## API

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/api/v1/links` | API key | Create a link. Body: `url`, optional `alias`, optional `expires_at` (RFC 3339). Returns `201`, or `409` if the alias is taken. |
| `GET` | `/api/v1/links` | API key | List the caller's links, newest first. Query: `limit` (1–100, default 20), `cursor`. |
| `GET` | `/api/v1/links/{code}` | API key | Get a link, including its `click_count`. |
| `GET` | `/api/v1/links/{code}/stats` | API key | `total_clicks` and clicks per UTC day for the last 7 days. |
| `DELETE` | `/api/v1/links/{code}` | API key | Soft delete. Returns `204`; the short URL then answers `410`. |
| `GET`, `HEAD` | `/{code}` | none | `302` to the target URL. `404` if the code is unknown, `410` if the link has expired or been deleted. `HEAD` is not counted as a click. |
| `GET` | `/healthz` | none | Liveness probe. |
| `GET` | `/readyz` | none | Readiness probe; pings Postgres. |
| `GET` | `/api/v1/docs`, `/api/v1/openapi.{json,yaml}` | none | Interactive docs and the OpenAPI description. |

Things to know when calling the API:

- **Keys** look like `sk_…` and go in an `Authorization: Bearer` header. Each key only sees its own links; another key's link answers `404`.
- **Errors** from `/api/v1` are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457) problem details (`application/problem+json`). Redirect errors are plain text.
- **Target URLs** must be absolute `http` or `https` URLs of at most 2048 characters, with no credentials in them. Private, loopback and other non-public IP addresses are rejected, and so are hosts such as `localhost`, `*.local` and `*.internal`. DNS is not resolved.
- **Aliases** are 3 to 32 characters from `[A-Za-z0-9_-]`. Route names such as `api`, `docs` and `healthz` are reserved, and a deleted link's code is never reused.
- **Cursors** are signed and tied to the API key that received them. A tampered cursor, or one signed with an old `CURSOR_SECRET`, answers `422`.
- **Stats** lag by up to one flush interval (`CLICK_FLUSH_INTERVAL`, default 1s).
- **Rate limits** exceeded answer `429` with `Retry-After` and `X-RateLimit-*` headers.

The full contract is in [api/openapi.yaml](api/openapi.yaml).

## Configuration

All settings come from environment variables. [.env.example](.env.example) is a working local setup. The defaults below are what applies when a variable is unset.

**Server**

| Variable | Default | Description |
|---|---|---|
| `APP_ENV` | `dev` | `dev`, `test` or `prod`. `dev` logs human-readable text; the others log JSON. |
| `PORT` | `8080` | Listen port. |
| `BASE_URL` | `http://localhost:8080` | Public origin used to build `short_url` and the OpenAPI server URL. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `REQUEST_TIMEOUT` | `15s` | Per-request deadline. |
| `SHUTDOWN_TIMEOUT` | `10s` | Budget for draining HTTP on SIGTERM. Flushing buffered clicks gets the same budget again. |
| `TRUSTED_PROXIES` | empty | Comma-separated CIDRs allowed to set `X-Forwarded-For` and `traceparent`. Empty means the TCP peer is the client. |
| `CURSOR_SECRET` | empty | HMAC key for pagination cursors, at least 32 bytes and the same on every replica (`openssl rand -base64 32`). Required when `APP_ENV=prod`. |
| `RATE_LIMIT_CREATE_PER_SEC` | `10` | Per-IP limit on `POST /api/v1/links`. |
| `RATE_LIMIT_REDIRECT_PER_SEC` | `100` | Per-IP limit on `GET`/`HEAD /{code}`. |

**Postgres**

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | (required) | pgx connection string. |
| `MIGRATE_ON_START` | `false` | Apply the embedded migrations at startup. |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `10` / `2` | Pool size. |
| `DB_MAX_CONN_LIFETIME` / `DB_MAX_CONN_IDLE_TIME` | `1h` / `30m` | Connection recycling. |
| `DB_CONNECT_TIMEOUT` | `5s` | Dial timeout. |

**Redirect cache**

| Variable | Default | Description |
|---|---|---|
| `REDIS_URL` | empty | go-redis URL, for example `redis://:pass@host:6379/0`. Empty disables the cache. Client options such as `?read_timeout=250ms` can be added as query parameters. |
| `LINK_CACHE_TTL` | `10m` | How long a cached link lives. |
| `LINK_CACHE_NEGATIVE_TTL` | `60s` | How long an unknown code is remembered as missing. |

**Click analytics**

| Variable | Default | Description |
|---|---|---|
| `CLICK_BUFFER_SIZE` | `10000` | Clicks that can wait for a flush. When the buffer is full, clicks are dropped (and counted) rather than slowing redirects. |
| `CLICK_FLUSH_INTERVAL` | `1s` | Longest time a click waits before it is written. |
| `CLICK_FLUSH_BATCH` | `500` | Flush early once this many clicks are pending. |
| `CLICK_RETENTION` | `2160h` (90 days) | Age after which click rows are deleted. Lifetime counters are kept. |
| `CLICK_PURGE_INTERVAL` | `24h` | How often the in-process purger runs. `0` disables it; then schedule `api clicks purge` externally. |

**Telemetry**

| Variable | Default | Description |
|---|---|---|
| `TELEMETRY_ENABLED` | `false` | Export traces and metrics over OTLP gRPC. |
| `OTEL_*` | SDK defaults | Read by the OpenTelemetry SDK: `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_SAMPLER`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, and so on. |

## Development

| Command | What it does |
|---|---|
| `task run` | Run the API from source. |
| `task build` | Build a static binary at `bin/api`. |
| `task test` | Unit tests with the race detector. |
| `task test:integration` | Integration tests against real Postgres and Redis containers (needs Docker). |
| `task lint` / `task fmt` | `go vet` plus golangci-lint, or auto-fix. |
| `task sqlc` / `task sqlc:check` | Regenerate the query code, or check that it is current. |
| `task openapi` / `task openapi:check` | Export `api/openapi.yaml`, or check that it is current. |
| `task ci` | Everything CI runs: lint, both drift checks, all tests and the image build. |
| `task apikey:create -- <name>` | Issue an API key. |
| `task clicks:purge` | Delete clicks older than `CLICK_RETENTION` once. |
| `task otel:up` / `task otel:down` | Start or stop the local Collector, Jaeger and Prometheus. |
| `task up` / `task down` | Start Postgres and Redis, or remove every container and volume. |

Extra tools are needed only for some tasks: [golangci-lint](https://golangci-lint.run) v2.15 for `lint`, [sqlc](https://sqlc.dev) 1.31.1 for `sqlc`, and the [goose](https://pressly.github.io/goose/) CLI for `migrate`. The generated sqlc code is committed, so `go build` and `go test` need none of them.

The same binary also provides the operator subcommands:

```text
api                       start the HTTP server
api apikey create <name>  issue an API key and print it once
api clicks purge          delete clicks older than CLICK_RETENTION
api openapi               print the OpenAPI 3.1 description as YAML
```

### Database changes

1. Add a goose migration under `migrations/`. Use a zero-padded number, for example `00004_add_x.sql`, because sqlc reads the files in lexicographic order.
2. Write queries in `internal/db/queries/*.sql`.
3. Run `task sqlc` and commit the generated code in `internal/db/sqlc/`.

### API changes

Huma builds the spec from the Go request and response types. After changing a handler, run `task openapi` and commit `api/openapi.yaml`; the CI "OpenAPI drift" job checks it.

### Tests

Unit tests sit next to the code and use fakes. Time-dependent code (expiry, the click flusher, the purger) is tested with `testing/synctest`, so the tests never sleep.

The integration tests in `test/integration` sit behind the `integration` build tag. They use testcontainers to start `postgres:17-alpine` and `redis:8.10-alpine`, build the real application through `internal/app`, and test it over HTTP. That covers auth, scoping, expiry, stats, purge, cache coherence and recovery after a Redis outage, and tracing.

## Observability

```sh
task otel:up   # Collector on :4317, Jaeger on :16686, Prometheus on :9090
# set TELEMETRY_ENABLED=true in .env, then:
task run
```

- Open Jaeger at <http://localhost:16686>. A redirect is a `GET /{code}` span with a Redis `get` child, or Postgres spans when the cache misses. Click writes show up as separate `click.flush` traces.
- Open Prometheus at <http://localhost:9090>. Alongside the HTTP, database, pool and Go runtime metrics there are application metrics:
  - `shortener_redirects_total`, by status code
  - `shortener_links_created_total` and `shortener_links_code_collisions_total`
  - `shortener_links_cache_lookups_total`, by hit, miss or error
  - `shortener_clicks_dropped_total`, by reason
  - `shortener_clicks_flush_size`, by outcome
- Log lines written during a traced request carry `trace_id` and `span_id`.

Sending every trace roughly halves redirect throughput. In production, sample a fraction, for example `OTEL_TRACES_SAMPLER=parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG=0.01`.

## Deployment

```sh
task image:build   # builds shortener:local
task image:run     # runs it against the compose Postgres and Redis on :8080
```

The image is a multi-stage build that ends in `gcr.io/distroless/static-debian13:nonroot`: a static binary, no shell, running as UID 65532. Pass `--build-arg VERSION=<tag or sha>` to set `service.version`.

For production:

- Set `APP_ENV=prod` and a shared `CURSOR_SECRET`. The server refuses to start without the secret.
- Run migrations as a release step with `task migrate` (the goose CLI), and leave `MIGRATE_ON_START` off.
- Behind a load balancer, set `TRUSTED_PROXIES` to its CIDRs. Otherwise every client shares the proxy's rate-limit bucket.
- Use `/healthz` for liveness and `/readyz` for readiness. Redis is not part of readiness, because redirects keep working without it.
- After SIGTERM, shutdown takes at most about 25 seconds with the defaults: draining HTTP, flushing buffered clicks, then exporting the last telemetry. That fits a 30-second termination grace period.
- With several replicas, consider `CLICK_PURGE_INTERVAL=0` and a single scheduled `api clicks purge`.

## Performance

These numbers come from one 12-thread workstation running the load generator, the API, Postgres and Redis together. See [docs/performance.md](docs/performance.md) for the method and the full tables.

- Once the Redis cache is warm, redirects make no database lookups. At a fixed 20k redirects/s, p99 is 1.3 to 5.1 ms with the cache and 13.9 ms without.
- Peak throughput on one instance is about 39k redirects/s with the cache and about 27k without.
- When Redis is paused or stopped, a circuit breaker opens and redirects fall back to Postgres with every answer still correct. The cache is used again within a second of Redis recovering.
- The current per-instance limit is click recording, at about 28k to 30k clicks/s. Watch `shortener_clicks_dropped_total{shortener_clicks_drop_reason="buffer_full"}` and add replicas before an instance reaches it.

## Project layout

```text
cmd/api/              entry point and operator subcommands; composition only
internal/
  app/                composition root shared by main and the integration tests
  config/             environment variables into a typed, validated Config
  server/             chi router, Huma mount, health probes, graceful shutdown
    middleware/       request id, client IP, logging, recover, timeout, rate limit, trace route
  link/               links: handlers, service, repository, Redis cache, cursors
  click/              click buffer and flusher, stats, retention purge
  apikey/             key issuing, hashing and the Bearer middleware
  db/                 pgx pool, embedded migrations, queries/ and generated sqlc/
  platform/           otelx (OpenTelemetry), redisx (client and circuit breaker), slogx (logging)
migrations/           goose SQL migrations, embedded in the binary
api/openapi.yaml      exported OpenAPI description, checked in CI
deploy/               docker-compose and the Collector and Prometheus configs
test/integration/     end-to-end tests with testcontainers
docs/                 tech stack, roadmap with design notes, performance report
```

Each feature package (`link`, `click`, `apikey`) holds its own handler, service and repository. Services define the small interfaces they depend on, and `click` depends on `link`, never the other way round. [docs/roadmap.md](docs/roadmap.md) records how each phase was built and why, along with the open questions.
