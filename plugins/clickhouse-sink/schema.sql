-- The plugin runs these statements on startup (and again after a failed
-- insert), so the schema is created on first use. {db} is replaced with the
-- configured database. Statements are separated by a line holding only ';'.

CREATE DATABASE IF NOT EXISTS {db}
;
-- One row per client request: what the client sent and what it got back.
CREATE TABLE IF NOT EXISTS {db}.requests
(
    request_id       String,
    trace_id         String,
    started_at       DateTime64(3, 'UTC'),
    completed_at     DateTime64(3, 'UTC'),
    duration_ms      UInt64 MATERIALIZED toUInt64(greatest(0, dateDiff('millisecond', started_at, completed_at))),
    source_format    LowCardinality(String),
    model            LowCardinality(String),
    requested_model  LowCardinality(String),
    stream           Bool,
    outcome          LowCardinality(String),
    status_code      UInt16,
    error            String,
    request_headers  Map(String, String),
    request_body     String CODEC(ZSTD(3)),
    response_headers Map(String, String),
    response_body    String CODEC(ZSTD(3)),
    response_chunks  UInt32
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(started_at)
ORDER BY (started_at, request_id)
;
-- One row per upstream attempt (retries and credential fallbacks add rows).
CREATE TABLE IF NOT EXISTS {db}.usage
(
    request_id             String,
    trace_id               String,
    selection_id           String,
    requested_at           DateTime64(3, 'UTC'),
    provider               LowCardinality(String),
    executor_type          LowCardinality(String),
    base_url               LowCardinality(String),
    model                  LowCardinality(String),
    alias                  LowCardinality(String),
    response_model         LowCardinality(String),
    api_key_hash           LowCardinality(String),
    access_provider        LowCardinality(String),
    session_id             String,
    parent_session_id      String,
    auth_id                LowCardinality(String),
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
    failure_body           String CODEC(ZSTD(3)),
    input_tokens           Int64,
    output_tokens          Int64,
    reasoning_tokens       Int64,
    cached_tokens          Int64,
    cache_read_tokens      Int64,
    cache_creation_tokens  Int64,
    total_tokens           Int64,
    response_headers       Map(String, String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(requested_at)
ORDER BY (requested_at, trace_id)
;
-- Requests joined to their upstream attempts. A request with retries appears
-- once per attempt, so count requests with uniq(request_id). Trace IDs are
-- short, so the join also requires the attempt to fall inside the request.
CREATE OR REPLACE VIEW {db}.inference AS
SELECT
    r.*,
    r.duration_ms,
    u.request_id AS usage_request_id,
    u.requested_at,
    u.provider,
    u.auth_id,
    u.auth_type,
    u.api_key_hash,
    u.response_model,
    u.reasoning_effort,
    u.latency_ms,
    u.ttft_ms,
    u.failed,
    u.failure_status,
    u.input_tokens,
    u.output_tokens,
    u.reasoning_tokens,
    u.cached_tokens,
    u.cache_read_tokens,
    u.cache_creation_tokens,
    u.total_tokens
FROM {db}.requests AS r
LEFT JOIN {db}.usage AS u
    ON u.trace_id = r.trace_id
    AND r.trace_id != ''
    AND u.requested_at >= r.started_at - INTERVAL 1 MINUTE
    AND u.requested_at <= r.completed_at + INTERVAL 1 MINUTE
