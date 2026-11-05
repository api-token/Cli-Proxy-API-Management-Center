# clickhouse-sink design (v3.1, approved by council 2026-11-04)

Goal: single-user local store of ALL inference through CLIProxyAPI (CPA), easy to search for patterns
(repeated prompts, tool usage, failures, tokens by client/model, replaying conversations). Keep it simple.

Facts confirmed since round 1: `upstream.codex.response-steering` is not set in the user's config (default false),
so duplex steering inputs are not in play. Real Codex Responses requests average ~340 KB of request body.
TraceID is a UUIDv7 per HTTP connection, shared by all websocket turns on that connection.

## A. Capture (plugin `clickhouse-sink`, stays raw-only, in-process in CPA)
1. Usage join: NO fork patch. Each usage row is attributed with an ASOF join:
   `usage ASOF LEFT JOIN requests ON u.trace_id = r.trace_id AND u.requested_at >= r.started_at`, i.e. the latest request on
   the same trace that started at or before the attempt. completed_at is not used, so the round-3 race (a websocket turn's
   lifecycle completion recorded after the next turn has started, handlers_stream.go:594) cannot mis-attribute: every attempt
   of turn N starts before turn N+1's lifecycle starts, and every attempt starts after its own lifecycle start
   (same process clock). Verified on ClickHouse 25.8 with overlapping windows. `queries.sql` keeps a detector
   (usage rows with requested_at > their request's completed_at + 5s, or unmatched) as a tripwire; the ~30-line
   lifecycle-ID fork patch stays the fallback if it ever fires.
   Timestamps: raw `started_at`, `completed_at` and usage `requested_at` are `DateTime64(9, 'UTC')`, serialized by the
   sink with nine fractional digits (v0 truncated to ms, so a turn-N attempt and turn N+1's start in the same ms could
   mis-join). Verification includes that same-millisecond case.
   README notes: plugins that make nested model calls on the same trace (host ExecuteModel callbacks) concurrently could be
   mis-attributed; none of the loaded plugins do.
2. Stream capture: `response_chunks Array(String)`, one element per chunk exactly as received (Codex/Responses chunks are
   unframed SSE lines; framing happens downstream). Non-stream `response_body String` unchanged. No byte munging.
3. `path` column from `Metadata["request_path"]`; also store the request's raw `x-codex-turn-metadata` etc. in headers as today.
4. Sink: failed batches are kept immutable with their original `insert_deduplication_token` and retried as-is; new rows
   queue separately (table setting `non_replicated_deduplication_window`). HTTP 400 → bisect, drop only bad rows (logged).
   Check response-body read errors. `ingested_at DateTime64 DEFAULT now64()` on raw tables. Best-effort in-memory buffer.
5. Raw tables recreated now (pre-release); future schema changes via new tables or `ALTER ... ADD COLUMN IF NOT EXISTS`.

6. Secret redaction, in the plugin at completion time (async, off the hot path) and before any insert, so no layer
   ever holds the secret. Applied to request/response headers left after the header strip, request body, response body,
   every stream chunk, usage failure body and the completion error. Derived data inherits it.
   - Exact known values: OAuth access/refresh tokens and API keys from the auth dir (`/data/auth/*.json`), `api-keys`
     and the management key from `config.yaml`, values in `~/.cli-proxy-api/.env` (mounted read-only), and an optional
     extra list file (one secret per line). Matched raw, JSON-escaped and base64-encoded, with Aho-Corasick. Values
     shorter than 12 characters are ignored (ports, booleans). The set reloads when the files change (mtime check
     at most every 30 s), because CPA rotates OAuth access tokens.
   - Pattern rules: gitleaks v8 (MIT, `detect.NewDetectorDefaultConfig()` + `DetectString`, checked v8.30.1), keyword prefilter
     then regex; a small allowlist for confirmed false positives.
   - Replacement: `[REDACTED:<rule>:<first 8 hex of sha256>]` (ASCII, JSON-safe), so the same secret correlates across
     rows without being stored. Per-request `redaction_count` and `redaction_rules Array(LowCardinality(String))` on
     raw rows for auditing.
   - Known gaps (documented): a secret split across small stream deltas; secrets inside images/attachments; custom
     formats nobody listed. New rules do not apply to stored rows unless re-run with `ALTER TABLE ... UPDATE`.
   - If redaction fails for a request (e.g. panic), the row is stored with bodies emptied and `redaction_count = -1`
     rather than unredacted.

## B. Deriver (small Go binary, separate compose service, outside CPA)
- Pure package `extract(rawRow, priorSeen bool) -> facts`, formats: OpenAI chat, Responses (HTTP + websocket), Claude, Gemini.
  Responses: `response.completed`.output, deltas only when canceled/incomplete. Claude: per-block deltas incl. input_json_delta
  and thinking. Chat: tool_calls by index. Panics recovered per row → `parse_status='unparseable'` row (no poison loop).
- ONE derived table `request_facts`, one row per raw request, written in one insert. Emitted messages live in that row as
  parallel arrays (direction, position, role, type, tool_name, call_id, arguments, is_error, text, text_hash).
  `messages` is a view: `request_facts ARRAY JOIN ...`. A facts row exists iff its messages exist, so the
  anti-join "raw request_id not in request_facts" is exact; a committed-but-unacked insert is simply skipped next pass.
- Loop: every ~30 s, process raw rows with `ingested_at >= max(request_facts.ingested_at) - 1h` (whole table when empty)
  that are not yet in request_facts, in (ingested_at, request_id) order. No fixed cutoff, so downtime backlog is processed.
  `request_facts.ingested_at` is COPIED from the raw row (no DEFAULT), so the watermark only advances over rows actually
  processed and an interrupted bulk pass or rebuild resumes safely. A failed facts insert is never retried within a pass;
  the next pass's anti-join decides. Within a pass, conversation_ids already emitted are kept in memory so `priorSeen`
  is correct for rows in the same batch.
  Rebuild is a one-off command against the same service (`docker compose run --rm deriver --rebuild`, after stopping the
  loop service): truncate, process all, exit. Never part of the service's default command.
- Which input items are emitted (tail rule):
  * First request seen for a conversation_id: emit the whole input (covers resumed/imported conversations).
  * Otherwise emit input items after the boundary = the last item that is model-produced (assistant message, reasoning,
    any *_call / tool_use / thinking), ignoring a trailing assistant prefill message; user/developer/system messages,
    *_output / tool_result items and `additional_tools` are never boundaries.
  * Always emit the request's own output items.
  text_hash is for grouping repeated prompts only, never for dropping rows.
- Facts columns: started_at, model, source_format, outcome (copied from raw so `messages` stands alone;
  ORDER BY (started_at, request_id)), path/kind, request_kind + thread_source + turn_id (from `x-codex-turn-metadata`), client (originator,
  x-codex-* headers, user-agent; `unknown` fallback), conversation_id + conversation_source, parent_conversation_id
  (Claude `parent_session_id` from metadata.user_id, links sub-agents), response_id, previous_response_id,
  system_prompt_hash, last_user_text, reply_text, reasoning_text, tools_offered, tools_called, stop_reason, parse_status,
  extractor_version, ingested_at.
- conversation_id comes only from the raw request itself (no late dependencies): client session id (Codex `session-id` /
  thread-id; Claude `metadata.user_id` session), suffixed `:<request_kind>` when request_kind is not `turn` (keeps Codex
  memory-consolidation calls out of the main thread); else fingerprint marked `inferred`. CPA's usage session_id and
  previous_response_id chains are exposed in views, not baked into facts.

## C. Search
- View `request_summary`: request_facts + usage aggregated per request (summed tokens, attempts, final provider/auth,
  CPA session_id).
- `/play` + `queries.sql`: text search over `messages`, top tools, failing tools, repeated prompts by text_hash,
  errors by message, tokens by client/model/day/request_kind, slowest requests, join checks (usage rows matched to no request,
  succeeded requests with no usage, usage starting after its request completed + 5s; the first two catch attribution gaps, the third only catches clock/ordering oddities, and "no usage" can briefly flag requests whose usage is still in flight), and replay =
  latest raw request_body among the conversation's requests with its most common system_prompt_hash (skips Claude Code
  helper/title calls that share the session) + its reply (following previous_response_id only for websocket deltas).
  `messages` is for search, not replay.
- No skip indexes or SimHash initially.

## D. Access (web page over Tailscale, AI agents)
1. Read-only ClickHouse user `inference_reader` (users.d XML mounted into the clickhouse container, password
   `CLICKHOUSE_READER_PASSWORD` in `.env`): SELECT on `cliproxy.*` only, `readonly=2` (so /play and clients may send
   format settings) with settings constraints capping `max_execution_time` (60 s), `max_result_bytes` (a few MB,
   `result_overflow_mode=throw` so an agent gets an error telling it to narrow, not a silently truncated result) and
   `max_memory_usage`. The `cliproxy` user stays write-capable for the plugin and deriver only.
2. Tailscale: `tailscale serve --bg --https=8123 http://127.0.0.1:8123`, tailnet-only like the proxy on 8317.
   The web page is https://<tailnet-host>:8123/play (sign in as `inference_reader`). The same URL is the
   query endpoint for agents on other tailnet machines. ClickHouse itself stays bound to loopback.
3. Self-describing schema: `COMMENT` on every derived table and column (what it holds, units, which table to use for what),
   so `DESCRIBE`/`system.columns` explains it to any agent.
4. Agent skill `inference-db` in `~/git/AI-Tool-Configs/skills/shared/` (already symlinked for Claude Code and Codex):
   when to use it, how to connect (credentials from `~/.config/inference-db/reader.env`, never inlined; localhost or the
   tailnet URL), a schema cheat-sheet, and query rules: search `messages`/`request_facts`/`request_summary` first; filter
   by time; always LIMIT; preview long text with `substring(text, 1, 500)`; never `SELECT *` from raw `requests`
   (bodies are ~340 KB each); fetch a raw body only for one request_id. Points at `queries.sql` for recipes.
5. Not in v1: an MCP server (ClickHouse's official `mcp-clickhouse`). Add it only if an agent without a shell
   (e.g. open-webui) needs access.
Note: full bodies can contain secrets pasted into prompts or tool output; access is tailnet-only and read-only.

## E. Verification
- Fixtures per format and stream shape: chat, Responses HTTP + websocket (unframed Codex lines), Claude Code with thinking +
  tools (and a subagent call once captured), Gemini, a canceled stream, a Codex memory-consolidation call, a prefill.
- Sink tests: "insert committed, response lost, new rows arrive, retry" yields no duplicates; 400 bisect.
- Deriver tests: crash between passes reprocesses nothing twice; poison row; rebuild idempotent.
- Redaction tests: known token raw/escaped/base64 in body, chunk and header; a gitleaks-pattern key; a token rotated
  in the auth dir is picked up; a short .env value (port) is left alone; a failure stores emptied bodies.
- Access checks: reader user cannot INSERT/ALTER or raise limits; /play works over the tailnet URL; an agent using the
  skill answers a sample question ("which tools failed most last week?") without pulling raw bodies.
- Live checks: HTTP stream + non-stream, websocket multi-turn on one connection (mis-join detector = 0), retry, cancel.

## Rejected as scope creep (with reason)
- Local disk spool / quarantine file: single user, ClickHouse local in the same stack; bisect + idempotent retry covers the realistic failures.
- Per-turn duplex websocket events (host change): steering is off; README documents that steering inputs are not captured if enabled.
- Cost/price lookup: tokens only for now.
- Full per-request `message_occurrences` (duplicates entire history again): tail emission instead.
- The A1 fork patch (deferred behind the mis-join detector, see A1); late identity revisiting (identity uses only the raw request).
- ReplacingMergeTree + fingerprint dedupe, SimHash clustering, bloom/text indexes, custom UI.

## Implementation notes (where the build differs from the plan)
- Responses reassembly: real Codex streams send `response.completed` with an empty `output`, so the deriver uses the
  final output when present, else `response.output_item.done` items, else added items plus deltas.
- Codex identity (session, thread, turn metadata) is read from the body's `client_metadata` before headers: on a
  websocket the headers belong to the upgrade request and are stale for later turns.
- A failed `request_facts` insert stops the pass rather than continuing, so the watermark cannot move past failed rows.
- Split secrets (found live): a model repeating a secret a few characters per delta left the pieces in raw chunks and
  the deriver reassembled them into searchable `reply_text`. The plugin now joins each stream's text pieces, finds
  secrets spanning chunks and replaces every carrying chunk with the marker; the deriver redacts all derived text again
  with the same sources (shared `redact` package) and keeps those markers in `reply_text`. A truncated secret (model cut
  off by `max_tokens`) is still not caught by exact matching.
- The first known-secret load is synchronous and a reload holds a lock until done (a race let one row through live).
- `prompt_cache_key` is exempt from gitleaks' generic rule: it is Codex's conversation ID.
- Message `type` stays protocol-native; queries identify tool calls by `role = 'assistant' AND tool_name != ''` and
  tool results by `role = 'tool'`. Codex tool outputs have no error flag, so "failing tools" also matches error text.
- ClickHouse logs at `warning` with `text_log` off, so query text (which can quote stored inference) is not written to
  server log files. `inference_reader` cannot read `system.*` tables.
