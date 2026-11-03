# Go Backend API: Tech Stack and Project Structure (2026)

Researched on 2026-11-02. Versions were resolved from proxy.golang.org and go.dev/dl on that date.
Toolchain target: **Go 1.27.3** (latest stable; Go 1.26.10 is the previous supported line).

## Guiding principle

Standard library first. Add small, composable libraries only where the stdlib is thin.
Since Go 1.22 the stdlib router supports method and wildcard patterns, and Go 1.26 and 1.27
closed most remaining gaps (Green Tea GC on by default, `encoding/json/v2`, stdlib `uuid`,
generic methods, goroutine leak profiles, `httptest.NewTestServer` for `testing/synctest`).

## Stack

| Concern | Library | Latest stable | Released | Notes |
|---|---|---|---|---|
| Toolchain | Go | 1.27.3 | 2026 | Set `go 1.27` in go.mod. |
| HTTP router | `net/http` ServeMux | stdlib | - | Enough for small services. |
| HTTP router (groups) | `github.com/go-chi/chi/v5` | v5.5.0 | 2026-10-14 | Plain `http.Handler`, route groups, per-group middleware. |
| Rate limiting | `github.com/go-chi/httprate` | v0.16.1 | 2026-10-08 | chi-native middleware. |
| OpenAPI (code-first) | `github.com/danielgtaylor/huma/v2` | v2.40.0 | 2026-10-27 | OpenAPI 3.1 from handler types, serves `/docs` and `/openapi.json`. Validation included. |
| OpenAPI (spec-first) | `github.com/oapi-codegen/oapi-codegen/v2` | v2.8.2 | 2026-10-31 | Use instead of Huma if the spec is the source of truth. |
| OpenAPI (spec-first, strict) | `github.com/ogen-go/ogen` | v1.24.0 | 2026-08-07 | Stricter than oapi-codegen; better unions and enums. |
| RPC alternative | `connectrpc.com/connect` | v1.21.1 | 2026-10-14 | gRPC plus plain HTTP JSON from one definition. For service-to-service APIs. |
| JSON | `encoding/json/v2` | stdlib (1.27) | - | Stricter defaults, faster Unmarshal. Opt out with `GOEXPERIMENT=nojsonv2`. |
| UUIDs | `uuid` | stdlib (1.27) | - | Replaces `github.com/google/uuid` (v1.6.0, last release 2024-01-23). |
| PostgreSQL driver | `github.com/jackc/pgx/v5` | v5.12.0 | 2026-10-31 | Use `pgxpool` directly, not `database/sql`. |
| Query codegen | `github.com/sqlc-dev/sqlc` | v1.31.1 | 2026-04-22 | Type-safe Go from SQL. Install as a tool; commit generated code. |
| Migrations | `github.com/pressly/goose/v3` | v3.29.0 | 2026-10-11 | Plain SQL files, embeddable, runnable from the binary. |
| Migrations (alt) | `ariga.io/atlas` | v1.3.0 | 2026-07-29 | Declarative schema diffing. |
| Migrations (alt) | `github.com/golang-migrate/migrate/v4` | v4.20.1 | 2026-09-09 | Widely used; goose is simpler for a single service. |
| Config | `github.com/caarlos0/env/v11` | v11.4.1 | 2026-05-01 | Env vars into a typed struct. Preferred over `kelseyhightower/envconfig` (v1.4.0, unmaintained since 2019). |
| Logging | `log/slog` | stdlib | - | JSON handler in production. Skip zap and logrus. |
| Tracing and metrics | `go.opentelemetry.io/otel` | v1.47.0 | 2026-10-02 | Pin `otel`, `otel/sdk` and exporters to the same version. |
| OTLP exporters | `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`, `.../otlpmetric/otlpmetricgrpc` | v1.47.0 | 2026-10-02 | Configured by the standard `OTEL_EXPORTER_OTLP_*` variables. Default endpoint is `https://localhost:4317`. The Go SDK does not implement `OTEL_SDK_DISABLED`. |
| HTTP instrumentation | `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.72.0 | 2026-10-02 | Contrib modules version separately from the core SDK. Names spans and sets `http.route` from `Request.Pattern`, which chi only sets on its own request copy; see phase 6 notes. |
| Runtime metrics | `go.opentelemetry.io/contrib/instrumentation/runtime` | v0.72.0 | 2026-10-02 | Goroutines, memory, GC. |
| pgx instrumentation | `github.com/exaring/otelpgx` | v0.13.0 | 2026-10-08 | Query, prepare, copy, connect and pool-acquire spans, `db.client.operation.duration`, pool stats. Spans only under a recording parent. |
| Local telemetry backends | `otel/opentelemetry-collector`, `jaegertracing/jaeger`, `prom/prometheus` | 0.164.0, 2.22.0, v3.15.1 | 2026-10 | Core collector distro is enough (OTLP receiver, `otlp_grpc`, prometheus and debug exporters). Jaeger v2 all-in-one takes OTLP directly. |
| Validation | `github.com/go-playground/validator/v10` | v10.30.5 | 2026-09-20 | Only needed if not using Huma. |
| JWT / JWK | `github.com/lestrrat-go/jwx/v4` | v4.6.0 | 2026-10-23 | v4 is current. v3 (v3.4.0) still receives releases but new code should start on v4. |
| OIDC | `github.com/coreos/go-oidc/v3` | v3.21.0 | 2026-09-01 | Delegate auth to an identity provider. |
| Background jobs | `github.com/riverqueue/river` | v0.51.0 | 2026-11-01 | Postgres-backed queue, built on pgx. Pre-1.0 but widely used. |
| Cache | `github.com/redis/go-redis/v9` | v9.23.0 | 2026-10-05 | Only if you actually need Redis. |
| CORS | `github.com/rs/cors` | v1.11.1 | 2024-08-29 | Stable, small; chi also ships `cors` middleware. |
| WebSockets | `github.com/gorilla/websocket` | v1.5.3 | 2024-06-14 | Maintained again since 2023. `github.com/coder/websocket` is a modern alternative. |
| Concurrency helpers | `golang.org/x/sync` | v0.24.0 | 2026-09-23 | `errgroup`, `semaphore`. |
| Crypto extras | `golang.org/x/crypto` | v0.58.0 | 2026-10-09 | bcrypt, argon2 if you must store passwords. |
| Assertions | `github.com/stretchr/testify` | v1.12.1 | 2026-08-17 | Optional; stdlib `testing` plus `cmp` is also fine. |
| Integration tests | `github.com/testcontainers/testcontainers-go` | v0.44.0 | 2026-08-07 | Real Postgres in tests. Use `modules/postgres` at the same version. Needs a Docker socket; GitHub's `ubuntu-latest` has one. |
| Container base | `gcr.io/distroless/static-debian13:nonroot` | rolling | - | Debian 13 is the current distroless base; the `-debian12` tags are deprecated upstream. Pin the suffix, unsuffixed tags will move. |
| CI actions | `actions/checkout@v7`, `actions/setup-go@v7`, `golangci/golangci-lint-action@v9`, `sqlc-dev/setup-sqlc@v5`, `docker/build-push-action@v7` | - | 2026-11 | Major versions as of 2026-11-02. setup-go reads `go-version-file: go.mod`. |
| Time-dependent tests | `testing/synctest` | stdlib | - | 1.27 adds `synctest.Sleep` and `httptest.NewTestServer`. |
| Lint | `github.com/golangci/golangci-lint/v2` | v2.15.0 | 2026-10-16 | Bundles the modernize analyzer. Also run `go vet` and `go fix`. |
| Task runner | `github.com/go-task/task/v3` | v3.54.0 | 2026-10-01 | Taskfile.yml. Makefile is an acceptable substitute. |
| Live reload | `github.com/air-verse/air` | v1.67.4 | 2026-08-01 | Local development only. |

### Skip for a new service

- Fiber: built on fasthttp, no HTTP/2, handlers are not `http.Handler`.
- Gin and Echo: fine, but within a few percent of `net/http` and they wrap the request in their own context type.
- GORM: in relative decline; sqlc plus pgx gives type safety without reflection.
- Viper: heavy for a single service; `caarlos0/env` covers twelve-factor config.
- zap, logrus: `log/slog` is sufficient.
- `github.com/google/uuid`: replaced by the stdlib `uuid` package in Go 1.27.
- A top-level `pkg/` directory: use `internal/` and split into a separate module when code is shared.

## Project structure

Follows the official go.dev module layout for servers: logic in `internal/`, binaries in `cmd/`,
and `internal/` organised by feature rather than by technical layer.

```
go-backend/
  go.mod
  Taskfile.yml
  Dockerfile
  .golangci.yml
  sqlc.yaml
  cmd/
    api/
      main.go              # wire config, db, server; nothing else
    worker/
      main.go              # optional background jobs binary
  internal/
    config/                # env -> typed Config struct
    server/
      server.go            # http.Server, routes(), graceful shutdown
      middleware/          # request id, logging, recover, auth, otel
      response.go          # error envelope, JSON helpers
    user/                  # one package per bounded context
      handler.go           # HTTP in, HTTP out
      service.go           # business rules, no HTTP or SQL
      repo.go              # thin wrapper over sqlc-generated code
      user.go              # domain types
      service_test.go
    order/
      ...
    db/
      db.go                # pgxpool setup
      queries/             # *.sql consumed by sqlc
      sqlc/                # generated, committed, never edited by hand
    platform/
      otelx/                 # named like slogx so it imports next to go.opentelemetry.io/otel
      slogx/
  migrations/
    0001_init.sql
  api/
    openapi.yaml           # exported from Huma in CI, or hand-written if spec-first
  deploy/
    docker-compose.yml     # local Postgres; collector, Jaeger, Prometheus behind a profile
  test/
    integration/           # testcontainers-based end-to-end tests
```

### Rules

- Feature packages own their vertical slice. `internal/user` holds handler, service and repo together. Cross-feature calls go through the service type, never the repo.
- Dependencies flow inward. Services declare the small interfaces they need; repos and handlers implement or call them.
- `main.go` is composition only. Build config, pool, services and server, then block on `signal.NotifyContext` and call `Shutdown` with a timeout.
- Pass `context.Context` from handler to query. Set per-request timeouts in middleware.
- Generated code is committed so `go build` works without sqlc installed. CI runs `sqlc diff` to catch drift.
- No `pkg/`. If something becomes reusable across repos, split it into its own module.

## Where the choices are debatable

- Huma versus plain handlers: Huma adds one dependency and some learning curve in exchange for validation, docs and a spec. For an internal service with a couple of endpoints, plain `net/http` is enough.
- chi versus ServeMux: ServeMux lacks route groups and per-group middleware. Past a handful of routes with mixed auth, chi pays for itself.
- Connect or gRPC if consumers are other Go services rather than browsers.

## Sources

- Go 1.27 release notes: https://go.dev/doc/go1.27
- Go 1.26 release notes: https://go.dev/doc/go1.26
- Organizing a Go module: https://go.dev/doc/modules/layout
- Go downloads (toolchain versions): https://go.dev/dl/
- Go module proxy (library versions): https://proxy.golang.org
- Go web frameworks compared, 2026: https://glukhov.org/app-architecture/api-architecture/go-web-frameworks-stdlib-chi-gin-echo-fiber/
- chi vs Gin vs Echo, 2026: https://reintech.io/blog/go-chi-vs-gin-vs-echo-web-framework-comparison-2026
- Huma v2: https://pkg.go.dev/github.com/danielgtaylor/huma/v2
- OpenAPI Go generator comparison (Speakeasy, vendor): https://www.speakeasy.com/docs/sdks/languages/golang/oss-comparison-go
- oapi-codegen vs ogen (Zenn, Jan 2026): https://zenn.dev/otakakot/articles/43653194611d42
