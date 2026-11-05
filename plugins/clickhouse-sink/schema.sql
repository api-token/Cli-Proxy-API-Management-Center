-- Raw layer, owned by the clickhouse-sink plugin. The plugin runs these
-- statements on startup and after a failed insert. {db} is replaced with the
-- configured database. Statements are separated by a line holding only ';'.
-- Derived tables and views (request_facts, messages, request_summary) are
-- owned by the deriver service, not by this file. See DESIGN.md.

CREATE DATABASE IF NOT EXISTS {db}
;
CREATE TABLE IF NOT EXISTS {db}.requests
(
    request_id       String COMMENT 'Host lifecycle request ID (UUID), one per client request',
    trace_id         String COMMENT 'Inbound HTTP request/connection ID; shared by all websocket turns on one connection',
    started_at       DateTime64(9, 'UTC') COMMENT 'When the proxy started handling the request',
    completed_at     DateTime64(9, 'UTC') COMMENT 'When the request reached its terminal outcome',
    ingested_at      DateTime64(9, 'UTC') DEFAULT now64(9) COMMENT 'When the row was inserted; the deriver watermark',
    duration_ms      UInt64 MATERIALIZED toUInt64(greatest(0, dateDiff('millisecond', started_at, completed_at))) COMMENT 'completed_at - started_at',
    path             LowCardinality(String) COMMENT 'Client request path, e.g. /v1/responses, /v1/messages/count_tokens',
    source_format    LowCardinality(String) COMMENT 'Client protocol: openai, openai-response, claude, gemini, ...',
    model            LowCardinality(String) COMMENT 'Model that executed the request',
    requested_model  LowCardinality(String) COMMENT 'Model the client asked for, before aliasing',
    stream           Bool,
    outcome          LowCardinality(String) COMMENT 'succeeded, failed, rejected or canceled',
    status_code      UInt16,
    error            String COMMENT 'Terminal error message, redacted',
    request_headers  Map(String, String) COMMENT 'Client request headers, lower-case; credentials removed, values redacted',
    request_body     String COMMENT 'Client request body as sent, redacted' CODEC(ZSTD(3)),
    response_headers Map(String, String) COMMENT 'Response headers delivered to the client, redacted',
    response_body    String COMMENT 'Non-streaming response body, redacted; empty for streams' CODEC(ZSTD(3)),
    response_chunks  Array(String) COMMENT 'Stream chunks exactly as delivered (one element each), redacted; empty for non-streams' CODEC(ZSTD(3)),
    redaction_count  Int32 COMMENT 'Secrets replaced in this row; -1 means redaction failed and bodies were emptied',
    redaction_rules  Array(LowCardinality(String)) COMMENT 'Distinct rule names that matched (known-value, gitleaks rule IDs)'
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(started_at)
ORDER BY (started_at, request_id)
SETTINGS non_replicated_deduplication_window = 1000
COMMENT 'Raw client requests and responses, one row per request. Source of truth; large bodies, query request_facts/messages first.'
;
CREATE TABLE IF NOT EXISTS {db}.usage
(
    request_id             String COMMENT 'Per-attempt usage ID (not the lifecycle request_id; see usage_by_request)',
    trace_id               String,
    selection_id           String,
    requested_at           DateTime64(9, 'UTC') COMMENT 'When this upstream attempt started',
    ingested_at            DateTime64(9, 'UTC') DEFAULT now64(9),
    provider               LowCardinality(String),
    executor_type          LowCardinality(String),
    base_url               LowCardinality(String),
    model                  LowCardinality(String),
    alias                  LowCardinality(String),
    response_model         LowCardinality(String),
    api_key_hash           LowCardinality(String) COMMENT 'First 16 hex of sha256(client API key)',
    access_provider        LowCardinality(String),
    session_id             String COMMENT 'CPA session identity',
    parent_session_id      String,
    auth_id                LowCardinality(String) COMMENT 'Upstream credential used',
    auth_index             LowCardinality(String),
    auth_type              LowCardinality(String),
    source                 LowCardinality(String),
    reasoning_effort       LowCardinality(String),
    service_tier           LowCardinality(String),
    effective_service_tier LowCardinality(String),
    response_service_tier  LowCardinality(String),
    stream                 Bool,
    generate               Bool,
    latency_ms             UInt64,
    ttft_ms                UInt64,
    failed                 Bool,
    failure_status         UInt16,
    failure_body           String COMMENT 'Upstream failure body, redacted' CODEC(ZSTD(3)),
    input_tokens           Int64,
    output_tokens          Int64,
    reasoning_tokens       Int64,
    cached_tokens          Int64,
    cache_read_tokens      Int64,
    cache_creation_tokens  Int64,
    total_tokens           Int64,
    response_headers       Map(String, String) COMMENT 'Upstream response headers, redacted'
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(requested_at)
ORDER BY (requested_at, trace_id)
SETTINGS non_replicated_deduplication_window = 1000
COMMENT 'One row per upstream attempt; retries and credential fallbacks add rows.'
;
-- Attributes each usage attempt to the latest request on the same trace that
-- started at or before it (see DESIGN.md A1). lifecycle_request_id is empty
-- when no request matched.
CREATE OR REPLACE VIEW {db}.usage_by_request AS
SELECT
    r.request_id AS lifecycle_request_id,
    u.*
FROM {db}.usage AS u
ASOF LEFT JOIN
(
    SELECT request_id, trace_id, started_at FROM {db}.requests WHERE trace_id != ''
) AS r
ON u.trace_id = r.trace_id AND u.requested_at >= r.started_at
