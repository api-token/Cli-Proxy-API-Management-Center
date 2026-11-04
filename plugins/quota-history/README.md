# quota-history plugin

A CLIProxyAPI plugin that records per-credential quota readings, so the
dashboard's Quota page can chart each provider's pooled capacity over time
and project when it runs dry. It needs no changes to CLIProxyAPI itself.

- **Passive recording:** every proxied Claude or Codex request carries the
  weekly quota in its upstream response headers
  (`anthropic-ratelimit-unified-7d-*`, `x-codex-primary/secondary-*`). The
  plugin reads them from the host's usage records, so history keeps growing
  while nobody has the dashboard open. Only credentials that receive traffic
  produce readings this way.
- **Dashboard readings:** the Quota page posts every reading it loads, for
  all providers.
- **Storage:** 14 days in `quota-history.jsonl`, at most one reading per
  credential window every 5 minutes. Resets always get through. Writes are
  flushed every 30 seconds and on shutdown.

## API

Plugin routes can only live under `/v0/management`, and they use the same
management key as the rest of the API.

- `GET /v0/management/quota-history?since=<RFC3339>&provider=<name>` returns
  `{"samples": [...], "retention_hours": 336}`, oldest first.
- `POST /v0/management/quota-history` accepts `{"samples": [...]}`, at most
  500 per request, and returns `{"accepted": n}`.

A sample is `name`, `auth_index`, `provider`, `observed_at`, `window_id`,
`period_hours`, `remaining_percent` (0-100) and `reset_at`. The server adds
`source` (`management` or `observed`).

## Build

The plugin is a cgo shared library. Build it for the same OS, CPU and C
library as the CLIProxyAPI server that loads it. It needs Go 1.26+ and a C
compiler.

On the server machine, run from the repository root:

```sh
cd plugins/quota-history
go build -buildmode=c-shared -o quota-history.so .
```

Use `.dylib` on macOS and `.dll` on Windows. The build also writes a
`quota-history.h` file, which you can ignore.

For the official Docker image (Debian bookworm), build in a matching
container so the library links against the same glibc:

```sh
docker run --rm -v "$PWD":/src -w /src/plugins/quota-history golang:1.26-bookworm \
  go build -buildmode=c-shared -o quota-history.so .
```

## Install

1. Copy `quota-history.so` into CLIProxyAPI's plugin directory
   (`plugins.dir`; `./plugins` in the Docker compose file). The file name sets
   the plugin ID, so keep it `quota-history.<ext>`.
2. Enable plugins and this plugin in CLIProxyAPI's `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       quota-history:
         enabled: true
         # Where quota-history.jsonl lives. Relative paths resolve against the
         # proxy's working directory. Use a persisted path: in Docker, the
         # plugins mount survives restarts.
         data-dir: "plugins/quota-history-data"
   ```

3. Restart CLIProxyAPI. Its log should show
   `pluginhost: plugin registered plugin_id=quota-history`.

Without the plugin, the Quota page still works but only charts readings from
the current visit.

## Develop

```sh
cd plugins/quota-history
go test ./...
```
