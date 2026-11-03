# Performance

Load test results for the redirect path, the Redis cache added in phase 7, and
the click pipeline behind it. Measured on 2026-11-03. Earlier one-off numbers
live in the phase 5 and 6 notes of `roadmap.md`.

## Summary

- **The cache takes Postgres out of the redirect path.** Every redirect,
  unknown codes included, is answered from Redis: 0 database lookups per
  request against 1.0 without the cache. Postgres CPU during a redirect run
  drops from about 24 s to about 9 s per 15 s, and what remains is click
  recording.
- **It also cuts tail latency under load.** At a fixed 20k redirects/s across
  10k codes, p99 is 1.3 to 5.1 ms with the cache against 13.9 ms without. At
  25k/s it is 1.5 ms against 12 to 18 ms. Peak throughput on one instance rises
  from about 27k to about 39k redirects/s.
- **A sick Redis costs little.** Paused or stopped for a whole run, redirects
  fall back to Postgres within a fraction of a second. Every answer stays
  correct and latency stays at the no-cache level. The cache is back in use
  within a second of Redis recovering.
- **Click recording is now the ceiling.** One instance stores about 28k to 30k
  clicks/s when traffic is spread over many links. Beyond that, clicks are
  dropped and counted in `shortener.clicks.dropped`. Without the cache the
  limit arrives sooner, because redirect lookups compete with the flusher for
  Postgres. See [Click recording](#click-recording).
- **Code collisions are not measurable.** There were 0 in 10,000 creates, so
  the `code_pool` table from the roadmap is not needed. See
  [Link creation](#link-creation).

## Setup

Everything ran on one workstation, so the load generator, the API, Postgres
and Redis compete for the same CPUs. Compare rows within a table rather than
reading the absolute numbers as capacity.

| Component | Details |
|---|---|
| Host | Intel Core i5-12400 (6 cores, 12 threads), 15 GiB RAM, Linux 7.0, Docker 29.7 |
| API | `bin/api` built as `task build` builds it (`-trimpath -s -w`), Go 1.27.3 |
| Postgres | 17.11, `deploy/docker-compose.yml`, defaults |
| Redis | 8.10.3, same compose file: `maxmemory 128mb`, `allkeys-lru`, no persistence |
| Load | vegeta v12.13.0, with `-redirects=-1` so the 302 itself is measured |
| Data | 10,000 links from one API key. `clicks` grew from 0 to 16.6M rows (2.7 GB) during the session |

API settings: `APP_ENV=prod`, telemetry off, both rate limits raised to
1,000,000/s, and everything else at its default. That means 10 Postgres
connections, click buffer 10,000 with batches of 500 every 1 s, and cache TTLs
of 10 min (links) and 60 s (unknown codes).

Two load shapes were used:

- **Fixed rate:** `vegeta attack -rate=N -max-workers=200 -duration=15s`. This
  is the honest latency comparison, because both setups face the same offered
  load.
- **Maximum throughput:** `vegeta attack -rate=0 -max-workers=50 -duration=10s`.
  Fifty clients send as fast as answers come back.

The traffic patterns were:

- **10k codes:** the 10,000 links in random order, round robin.
- **Hot:** one link.
- **Unknown:** one code that does not exist (404).

Derived columns:

- **DB lookups per request:** the change in `idx_scan` of `links_code_unique`
  divided by requests. Every lookup by code is one scan of that index.
- **Postgres CPU and Redis CPU:** the change in the container cgroup's
  `usage_usec` over the run plus a 3 s settle.
- **Clicks kept:** the change in `sum(links.click_count)` divided by 302
  responses. 100% means no click was dropped.

## Redirects at a fixed rate

10k codes, 15 s per run. Where a rate was run twice, both values are shown.

| Rate | Cache | p50 | p90 | p99 | DB lookups/req | Postgres CPU | Redis CPU | Clicks kept |
|---|---|---|---|---|---|---|---|---|
| 15k/s | Redis | 0.19 ms | 0.64 ms | 4.0 ms | 0 | 9.9 s | 3.7 s | 100% |
| 15k/s | none | 0.24 ms | 0.83 ms | 5.4 ms | 1.0 | 24.0 s | 0 | 100% |
| 20k/s | Redis | 0.15 ms | 0.39 / 0.50 ms | 1.3 / 5.1 ms | 0 | 10.8 s | 3.9 s | 100% |
| 20k/s | none | 0.20 ms | 1.8 / 2.2 ms | 13.9 / 13.9 ms | 1.0 | 25.8 / 26.6 s | 0 | 100% |
| 25k/s | Redis | 0.13 ms | 0.37 / 0.39 ms | 1.5 / 1.5 ms | 0 | 11.6 / 11.8 s | 4.1 s | 100% |
| 25k/s | none | 0.28 / 0.55 ms | 5.7 / 8.3 ms | 12.2 / 17.8 ms | 1.0 | 31.7 / 34.1 s | 0 | 79.2 / 98.4% |
| 30k/s | Redis | 0.15 ms | 0.67 ms | 6.0 ms | 0 | 14.0 s | 5.0 s | 100% |
| 30k/s | none | 0.72 ms | 5.1 ms | 8.6 ms | 1.0 | 38.2 s | 0 | 80.1% |
| 35k/s | Redis | 0.17 ms | 0.76 ms | 3.5 ms | 0 | 14.1 s | 5.7 s | 83.7% |
| 40k/s | Redis | 0.24 ms | 1.5 ms | 6.6 ms | 0 | 14.0 s | 6.4 s | 68.9% |

With the cache, Postgres CPU is the click flusher alone, and it levels off at
about 14 s per run: one backend at full speed. Without the cache, Postgres
works about 2.5 times as hard at the same rate. From 25k/s it falls behind on
both lookups and clicks.

## Maximum throughput

Medians of 5 rounds with the cache and 3 without, 10 s each, 50 workers.

| Traffic | Cache | Requests/s | p50 | p90 | p99 | Postgres CPU | Redis CPU |
|---|---|---|---|---|---|---|---|
| Hot link | Redis | 39.5k | 0.96 ms | 1.9 ms | 3.5 ms | 8.0 s | 3.8 s |
| Hot link | none | 30.3k | 1.43 ms | 2.4 ms | 4.0 ms | 22.8 s | 0 |
| 10k codes | Redis | 38.6k | 0.90 ms | 2.1 ms | 4.4 ms | 9.2 s | 4.3 s |
| 10k codes | none | 27.4k | 1.49 ms | 2.7 ms | 5.2 ms | 24.2 s | 0 |
| Unknown code | Redis | 49.5k | 0.76 ms | 1.5 ms | 2.5 ms | 0.2 s | 4.4 s |
| Unknown code | none | 36.1k | 1.18 ms | 2.0 ms | 3.0 ms | 18.0 s | 0 |

Round-to-round spread was about ±15%, which is the price of a shared machine.
These runs push one instance well past its click ceiling, so many clicks were
dropped in both setups. An unknown code records no click, which is why that
row shows the cache at its cleanest: Postgres is idle.

## Redis failures

The cache was configured throughout. Traffic was a fixed 20k redirects/s over
the 10k codes for 15 s. The failure was injected with `docker pause` (Redis
accepts connections and never answers) or `docker stop` (connection refused)
before the run started.

| Run | p50 | p90 | p99 | Max | DB lookups/req | Answers |
|---|---|---|---|---|---|---|
| Healthy, for reference | 0.15 ms | 0.39 to 0.50 ms | 1.3 to 5.1 ms | 62 ms | 0 | all correct |
| No cache, for reference | 0.20 ms | 1.8 to 2.2 ms | 13.9 ms | 31 ms | 1.0 | all correct |
| Redis paused | 0.20 ms | 3.4 ms | 12.9 ms | 203 ms | 0.96 | all correct |
| After unpause | 0.15 ms | 0.44 ms | 2.1 ms | 41 ms | 0.05 | all correct |
| Redis stopped | 0.18 ms | 0.44 ms | 5.3 ms | 34 ms | 0.94 | all correct |
| After restart, empty cache | 0.15 ms | 0.52 ms | 8.7 ms | 112 ms | 0.09 | all correct |
| Next run | 0.15 ms | 0.56 ms | 6.5 ms | 126 ms | 0 | all correct |

From the server log of the paused run:

- The first commands waited out the 100 ms read timeout.
- The circuit breaker opened 0.3 s later, after five consecutive failures.
- From then on, Redis was skipped except for one probe per second. That is
  where the 203 ms maximum comes from: requests that queued behind the first
  timeouts.
- A link deleted while Redis was paused answered 204 in 0.11 s and 410
  immediately after. Its tombstone write timed out, and the repair worker
  wrote it on the tick after Redis came back. The breaker closed on the same
  tick.

The stopped run behaved the same way, but failures were instant (connection
refused) rather than timeouts. After the restart the cache was empty, and it
refilled at one database read per code.

## Click recording

The click buffer from phase 5 writes with one goroutine on one Postgres
connection. Each flush does three things in one transaction:

- `COPY` the batch into `clicks`.
- One `UPDATE` that bumps `click_count` on every distinct link in the batch.
- `COMMIT`.

How fast that backend runs is the per-instance limit on recorded clicks:

- **Spread traffic** (the 10k codes): about 28k to 30k clicks/s. At a fixed
  30k/s every click was kept. At 35k/s, 83.7% were kept (about 29.3k/s
  stored), and at 40k/s 68.9% (about 27.6k/s). Postgres CPU stays at one core
  regardless.
- **Without the cache** the limit is lower, because the flusher shares
  Postgres with a lookup per redirect: 79.2% and 98.4% kept in two runs at
  25k/s.
- **Hot traffic** depends on batch size. Three flush settings at maximum
  throughput:

  | `CLICK_FLUSH_BATCH` / `CLICK_BUFFER_SIZE` | Hot link, clicks kept | 10k codes, clicks kept |
  |---|---|---|
  | 500 / 10,000 (default) | 69.7% at 45k/s | 53.4% at 46k/s |
  | 2,000 / 20,000 | 100% at 41k/s | 58.1% at 37k/s |
  | 5,000 / 50,000 | 100% at 44k/s | 55.8% at 51k/s |

  Bigger batches help when few links take the traffic, because a flush then
  updates few counter rows. They do not help spread traffic, where the cost is
  one row copied and one counter updated per click.

Phase 5 measured 20.4k redirects/s without drops on a different machine,
below this ceiling. The cache moved the redirect path past it.

What to do about it, in order of effort:

1. Watch `shortener.clicks.dropped{shortener.clicks.drop_reason="buffer_full"}`.
   Any steady rate means an instance is over its ceiling.
2. Keep each instance under about 25k redirects/s by adding replicas. Each has
   its own flusher and Postgres connection.
3. Set `CLICK_FLUSH_BATCH=2000` and `CLICK_BUFFER_SIZE=20000` when a few links
   take most of the traffic.
4. If one instance must record more, the flusher has to change. Options:
   - Run several flush workers on separate connections.
   - Accumulate counter deltas in memory and apply them less often than rows
     are copied.
   - Move to River, the fallback phase 5 named.

## Link creation

10,000 links were created through the API with 20 concurrent clients, with
the cache on (each create also writes the new entry to Redis):

- 6.7k creates/s.
- Latency: p50 2.9 ms, p90 3.4 ms, p99 4.5 ms.
- No `generated code collided` log lines, so 0 collisions.

The math agrees: 10,000 random codes from 62^7 (about 3.5e12) give an
expected 1.4e-5 collisions. At a billion links, a new code collides with
probability 1e9 / 3.5e12, about one create in 3,500, and that create costs one
extra `INSERT`. The `code_pool` table the roadmap suggests "if collision
retries become measurable" is therefore not built. The signal to revisit is
`shortener.links.code_collisions` rising above about 1% of
`shortener.links.created`, which takes about 35 billion links. Decision 1
already says to move to 8 characters long before that.

## Cache sizing

Measured with `INFO memory` after the runs:

- 10,000 entries took about 2 MB of Redis memory, about 200 bytes per entry.
- The 128 MB local limit therefore holds about 600k links.
- With `allkeys-lru`, going over the limit evicts the least recently used
  entries. That only costs database reads, never a wrong answer.

Entries are not refreshed on read. All 10k entries in this test were written
in the same minute, so they expired together 10 minutes later. That cost one
database read per code, visible as 2% DB lookups in one run. Spread over real
traffic this is negligible, so no TTL jitter was added.

## Binary size

The stripped binary grew from 24.3 MB to 32.5 MB with go-redis v9.23.0 and
redisotel. go-redis now ships the full Redis Stack command set (search, JSON,
time series, vector sets) and cannot be trimmed.

## Reproducing

```sh
task up            # Postgres on 5434, Redis on 6380
task build
docker compose -f deploy/docker-compose.yml exec redis sh -c 'REDISCLI_AUTH=shortener redis-cli FLUSHALL'

export APP_ENV=prod MIGRATE_ON_START=true TELEMETRY_ENABLED=false \
  DATABASE_URL='postgres://shortener:shortener@localhost:5434/shortener?sslmode=disable' \
  REDIS_URL='redis://:shortener@localhost:6380/0' \
  CURSOR_SECRET="$(openssl rand -base64 32)" \
  RATE_LIMIT_REDIRECT_PER_SEC=1000000 RATE_LIMIT_CREATE_PER_SEC=1000000
bin/api &                                  # unset REDIS_URL for the no-cache runs
KEY=$(bin/api apikey create bench 2>/dev/null)

# 10,000 links, then a vegeta targets file of their codes.
hey -n 10000 -c 20 -m POST -T application/json -H "Authorization: Bearer $KEY" \
  -d '{"url":"https://example.com/load/target"}' http://localhost:8080/api/v1/links
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U shortener -Atc \
  "select 'GET http://localhost:8080/' || code from links order by random()" > targets.txt

# One run. Read the counters before and after.
vegeta attack -rate=20000 -max-workers=200 -duration=15s -redirects=-1 -targets=targets.txt \
  | vegeta report
```

Counters to read before and after each run:

```sql
SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname = 'links_code_unique';
SELECT sum(click_count) FROM links;
```

For CPU, read `cat /sys/fs/cgroup/cpu.stat` (`usage_usec`) inside each
container.
