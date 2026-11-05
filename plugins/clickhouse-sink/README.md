# clickhouse-sink plugin

A CLIProxyAPI plugin that writes every proxied inference request to
ClickHouse: what the client sent, what it got back, and the usage record of
each upstream attempt. It needs no changes to CLIProxyAPI itself.

## What it stores

The plugin creates its schema on startup (`schema.sql`, embedded in the
library) in the configured database:

- **`requests`**: one row per client request. Holds the client-format
  request and response bodies and headers, stream chunk count, outcome
  (`succeeded`, `failed`, `rejected`, `canceled`), status code, error, and
  start and end times. Streamed responses are stored as the concatenated
  chunks the client received.
- **`usage`**: one row per upstream attempt (retries and credential
  fallbacks each add a row). Holds provider, credential, model, token counts,
  latency, TTFT, service tier, failure status and body, and upstream response
  headers.
- **`inference`**: a view joining the two on `trace_id`. A request with
  retries appears once per attempt, so count requests with
  `uniq(request_id)`.

Credentials are never stored. The `authorization`, `proxy-authorization`,
`x-api-key`, `api-key`, `x-goog-api-key`, `cookie` and `set-cookie` headers
are dropped. Client API keys are kept only as `api_key_hash`, the first 16 hex
characters of their SHA-256.

Bodies come from the host's interceptor hooks, so a request only gets bodies
when it goes through the HTTP handlers. Calls that never name a model, such
as model listings, are skipped.

## How it works

The interceptor hooks copy the request body, response body and stream
chunks into memory, keyed by the host's per-request ID, and return without
changing anything. The terminal `request.complete` event turns the capture
into a `requests` row, and `usage.handle` produces `usage` rows. Rows are
batched and inserted over HTTP (`JSONEachRow`) every 5 seconds, or sooner
once 8 MiB is buffered.

When ClickHouse is unreachable, rows stay buffered (up to 256 MiB) and are
retried on each flush. Past that, new rows are dropped and the count is
logged. Rows that ClickHouse rejects as unparsable (HTTP 400) are dropped
so they cannot stall later rows. Errors go to the proxy's stderr, prefixed
`[clickhouse-sink]`, once per distinct error.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    clickhouse-sink:
      enabled: true
      url: "http://clickhouse:8123"   # ClickHouse HTTP endpoint
      database: "cliproxy"            # created if missing
      username: "default"
      password: ""                    # falls back to $CLICKHOUSE_PASSWORD
      store-bodies: true              # false: metadata only
```

## Build

The plugin is a cgo shared library. Build it for the same OS, CPU and C
library as the CLIProxyAPI server that loads it. It needs Go 1.26+ and a C
compiler. For the Docker image (Debian bookworm), from the repository root:

```sh
docker run --rm -u (id -u):(id -g) -e HOME=/tmp -v "$PWD":/src \
  -w /src/plugins/clickhouse-sink golang:1.26-bookworm \
  go build -buildmode=c-shared -o clickhouse-sink.so .
```

(That is fish syntax; in bash use `$(id -u):$(id -g)`.) The build also writes
a `clickhouse-sink.h` file, which you can ignore.

## Install

1. Copy `clickhouse-sink.so` into CLIProxyAPI's plugin directory
   (`plugins.dir`). The file name sets the plugin ID, so keep it.
2. Add the configuration above and restart the proxy. The log shows
   `plugin registered plugin_id=clickhouse-sink`.

## Query examples

```sql
-- Tokens per model per day
SELECT toDate(requested_at) AS day, model,
       sum(input_tokens) AS input, sum(output_tokens) AS output
FROM cliproxy.usage GROUP BY day, model ORDER BY day DESC, input DESC;

-- The last 10 conversations, with what was said
SELECT started_at, model, outcome, request_body, response_body
FROM cliproxy.requests ORDER BY started_at DESC LIMIT 10;
```

## Test

```sh
go test ./...
```
