# clickhouse-sink

Stores every request proxied by CLIProxyAPI in ClickHouse, with secrets
redacted, so you and your AI agents can search it for patterns: what you
asked, what came back, which tools ran or failed, and tokens by client and
model. It needs no changes to CLIProxyAPI itself.

The design, and the reasoning behind it, is in [DESIGN.md](DESIGN.md).

## Parts

| Part | Where | What it does |
|---|---|---|
| Plugin | this directory (`clickhouse-sink.so`) | Runs inside CLIProxyAPI. Captures each request, response and usage record raw, redacts secrets, and writes the `requests` and `usage` tables ([schema.sql](schema.sql)). |
| Deriver | [deriver/](deriver/README.md) | A separate service. Turns raw rows into `request_facts` and the `messages` and `request_summary` views: readable text, tools, conversations. |
| Redaction | [redact/](redact/) | Shared by both. Replaces known credentials and gitleaks pattern matches with `[REDACTED:<rule>:<hash>]`. |
| Queries | [queries.sql](queries.sql) | Ready-made searches: text, conversations, replay, tools, failures, repeated prompts, errors, tokens, latency, health checks. |

For searching, start with `messages`, then `request_facts` or
`request_summary`. The raw `requests` table holds full bodies, which are often
over 300 KB each; fetch those one `request_id` at a time.

## What the plugin captures

It uses CLIProxyAPI's plugin hooks:

- request interception for the client request;
- response and stream-chunk interception for what the client received;
- request completion for the outcome;
- usage records for tokens, latency and credentials.

The interceptor hooks only copy data and return immediately. Everything else
runs after the request has completed.

- **`requests`** has one row per client request. It holds the path, model, outcome and error. It also holds the request and response headers and bodies, with stream chunks stored exactly as delivered, one array element each. Timestamps have nanosecond precision.
- **`usage`** has one row per upstream attempt, so retries and credential fallbacks add rows. The `usage_by_request` view assigns each attempt to its request. It matches the latest request on the same connection that started at or before the attempt, which is exact even for websocket turns (DESIGN.md A1).

Calls that never name a model, such as model listings, are skipped.

Rows are batched and inserted every 5 seconds, or sooner once 8 MiB is buffered.

- **ClickHouse unreachable:** rows stay in memory, up to 256 MiB. A failed batch is retried unchanged with an insert deduplication token, so a retry after a lost acknowledgement can't duplicate rows.
- **Rows ClickHouse rejects as unparsable:** the batch is split in half until only the bad rows are left, and those are dropped.
- **Errors** go to the proxy's log, prefixed `[clickhouse-sink]`.

## Redaction

Secrets are replaced before a row reaches any buffer, so no table ever holds them.

- **Credential headers** (`authorization`, `x-api-key`, `cookie`, ...) are dropped. Client API keys are kept only as `api_key_hash`.
- **Known values** are read from `redact-files` and matched raw, JSON-escaped and base64-encoded:
  - `.json` and `.yaml` files: every value under a credential-like key;
  - `.env` files: every value;
  - any other file: one secret per line.

  Values shorter than 12 characters are ignored. The files are re-checked at most every 30 seconds, so rotated OAuth tokens are picked up.
- **Pattern rules** come from gitleaks' default rule set. Codex's `prompt_cache_key` is exempt, because it is a conversation ID, not a credential.
- **Split secrets:** a secret a model streams back a few characters per chunk is found by joining the chunks' text. Every chunk that carries part of it becomes the marker. The deriver redacts the text it reassembles a second time.
- **Failures:** if redaction fails, the row is stored with its bodies emptied and `redaction_count = -1`. Nothing is stored unredacted. Query 11 in `queries.sql` audits which rules fired.

Known gaps:
- a secret inside an image or attachment;
- a truncated secret, such as a model cut off mid-token by `max_tokens`;
- custom formats that no rule covers and no file lists.

New rules don't apply to rows already stored.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    clickhouse-sink:
      enabled: true
      url: "http://clickhouse:8123"   # ClickHouse HTTP endpoint
      database: "cliproxy"            # created on startup
      username: "default"
      password: ""                    # falls back to $CLICKHOUSE_PASSWORD
      store-bodies: true              # false: metadata only
      redact-files:                   # default: ["config.yaml"]
        - "config.yaml"               # CLIProxyAPI's config (API keys, management key)
        - "/data/auth"                # OAuth credential files
        - "/CLIProxyAPI/secrets/.env"
        - "plugins/clickhouse-sink-redact.txt"   # extra values, one per line
      redact-disabled-rules: []       # gitleaks rule IDs to skip if they misfire
```

## Build

The plugin is a cgo shared library. Build it for the same OS, CPU and C
library as the CLIProxyAPI that loads it. For the Docker image, which is
Debian bookworm, run this from the repository root:

```sh
docker run --rm -u (id -u):(id -g) -e HOME=/tmp -e GOCACHE=/tmp/gocache -v "$PWD":/src \
  -w /src/plugins/clickhouse-sink golang:1.26-bookworm \
  go build -buildvcs=false -buildmode=c-shared -o clickhouse-sink.so .
```

That command is fish syntax; in bash, use `$(id -u):$(id -g)`. The build also
writes a `clickhouse-sink.h` file, which you can ignore. Copy
`clickhouse-sink.so` into the proxy's plugin directory. The file name sets the
plugin ID, so don't rename it.

## Test

```sh
go test -race ./...            # plugin and redact package
cd deriver; and go test ./...  # deriver (see its README for integration tests)
```
