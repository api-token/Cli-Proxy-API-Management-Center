package main

import "strings"

// Derived layer, owned by the deriver. The raw tables and usage_by_request
// come from ../schema.sql (owned by the clickhouse-sink plugin). Future changes:
// ALTER TABLE ... ADD COLUMN IF NOT EXISTS, or a new table; never edit the
// CREATE below in place expecting it to migrate.
var derivedSchema = []string{
	`CREATE TABLE IF NOT EXISTS {db}.request_facts
(
    request_id             String COMMENT 'Lifecycle request ID; joins requests.request_id and usage_by_request.lifecycle_request_id',
    started_at             DateTime64(9, 'UTC') COMMENT 'Copied from requests.started_at',
    ingested_at            DateTime64(9, 'UTC') COMMENT 'Copied from requests.ingested_at (no default); the deriver watermark',
    model                  LowCardinality(String) COMMENT 'Model that executed the request (copied from requests)',
    source_format          LowCardinality(String) COMMENT 'Client protocol: openai (chat), openai-response, claude, gemini, ...',
    outcome                LowCardinality(String) COMMENT 'succeeded, failed, rejected or canceled (copied from requests)',
    status_code            UInt16 COMMENT 'HTTP status returned to the client',
    stream                 Bool COMMENT 'Streaming request',
    duration_ms            UInt64 COMMENT 'requests.completed_at - started_at, milliseconds',
    error                  String COMMENT 'Terminal error message (copied from requests, redacted)',
    path                   LowCardinality(String) COMMENT 'Client request path',
    kind                   LowCardinality(String) COMMENT 'Short kind from path: chat, responses, compact, messages, count_tokens, generate, embeddings, models, unknown, ...',
    request_kind           LowCardinality(String) COMMENT 'Codex x-codex-turn-metadata request_kind (turn, memory, ...); empty for other clients',
    thread_source          LowCardinality(String) COMMENT 'Codex x-codex-turn-metadata thread_source (e.g. memory_consolidation)',
    turn_id                String COMMENT 'Codex x-codex-turn-metadata turn_id',
    client                 LowCardinality(String) COMMENT 'Calling client: originator header, claude-code, codex, user-agent product, or unknown',
    conversation_id        String COMMENT 'Conversation identity from the request alone: claude:<session>[:agent:<id>], codex:<thread>, session:<header>, or fp:<hash> when inferred; suffixed :<request_kind> when request_kind is not turn',
    conversation_source    LowCardinality(String) COMMENT 'Where conversation_id came from: claude-header, claude-metadata, codex-header, header, inferred (fingerprint of system prompt + first user message), or empty',
    parent_conversation_id String COMMENT 'Parent conversation (Claude Code subagent parent session, Codex parent thread); empty when none',
    response_id            String COMMENT 'Response/message ID returned by the upstream (resp_..., msg_..., chatcmpl-...)',
    previous_response_id   String COMMENT 'Responses API previous_response_id (websocket/stateful chains)',
    system_prompt_hash     UInt64 COMMENT 'Hash of the system prompt (top-level system/instructions, else leading system/developer messages); 0 when none',
    last_user_text         String COMMENT 'Text of the last user message in the full input (not only the emitted tail)' CODEC(ZSTD(3)),
    reply_text             String COMMENT 'Assistant text of the response, joined' CODEC(ZSTD(3)),
    reasoning_text         String COMMENT 'Reasoning summary / thinking text of the response, joined' CODEC(ZSTD(3)),
    tools_offered          Array(LowCardinality(String)) COMMENT 'Distinct tool names offered in the request (namespaces flattened)',
    tools_called           Array(LowCardinality(String)) COMMENT 'Tool names the model called in this response, in order',
    stop_reason            LowCardinality(String) COMMENT 'finish_reason / stop_reason / finishReason / Responses status or incomplete reason; empty when the stream ended early',
    prior_seen             Bool COMMENT 'An earlier request of this conversation_id was already derived, so only the input tail was emitted',
    input_items            UInt32 COMMENT 'Number of normalized input items in the request (emitted or not)',
    msg_direction          Array(LowCardinality(String)) COMMENT 'Emitted messages (parallel arrays, see the messages view): input or output',
    msg_position           Array(UInt32) COMMENT 'Index in the normalized input list (system prompt is 0 when present) or in the output list',
    msg_role               Array(LowCardinality(String)) COMMENT 'system, developer, user, assistant or tool',
    msg_type               Array(LowCardinality(String)) COMMENT 'Protocol-native item/block type; plain text is always message (e.g. reasoning, thinking, custom_tool_call, function_call_output, tool_use, tool_result, tool_call)',
    msg_tool_name          Array(LowCardinality(String)) COMMENT 'Tool name for tool calls (and results when the protocol names them)',
    msg_call_id            Array(String) COMMENT 'Tool call ID linking a call to its result',
    msg_arguments          Array(String) COMMENT 'Tool call arguments/input as sent by the model' CODEC(ZSTD(3)),
    msg_is_error           Array(Bool) COMMENT 'Tool result flagged as an error by the protocol (Claude is_error, Gemini response.error)',
    msg_text               Array(String) COMMENT 'Message text; base64 media and encrypted_content replaced by placeholders' CODEC(ZSTD(3)),
    msg_text_hash          Array(UInt64) COMMENT 'Hash of the trimmed text for grouping repeated prompts; 0 for empty text',
    parse_status           LowCardinality(String) COMMENT 'ok, partial (stream without terminal event), unsupported, empty, redaction_failed, unparseable',
    parse_error            String COMMENT 'Short reason for a non-ok parse_status (never body content)',
    extractor_version      UInt16 COMMENT 'Extractor version that produced the row; rebuild after it changes'
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(started_at)
ORDER BY (started_at, request_id)
COMMENT 'One row per raw request, derived by the deriver service: identity, client, reply, tools and the emitted messages (first request of a conversation emits the whole input, later ones only the new tail). Search here or in messages/request_summary before touching raw requests.'`,

	`CREATE OR REPLACE VIEW {db}.messages AS
SELECT
    request_id, started_at, ingested_at, model, source_format, outcome, kind, request_kind, client,
    conversation_id, parent_conversation_id, response_id, parse_status,
    direction, position, role, type, tool_name, call_id, arguments, is_error, text, text_hash
FROM {db}.request_facts
ARRAY JOIN
    msg_direction AS direction, msg_position AS position, msg_role AS role, msg_type AS type,
    msg_tool_name AS tool_name, msg_call_id AS call_id, msg_arguments AS arguments,
    msg_is_error AS is_error, msg_text AS text, msg_text_hash AS text_hash
COMMENT 'One row per emitted message (request_facts ARRAY JOIN). For text search, tool usage and repeated prompts (group by text_hash). Not for replay: later requests only carry their new input tail.'`,

	`CREATE OR REPLACE VIEW {db}.request_summary AS
SELECT
    f.* EXCEPT (msg_direction, msg_position, msg_role, msg_type, msg_tool_name, msg_call_id, msg_arguments, msg_is_error, msg_text, msg_text_hash),
    u.attempts, u.failed_attempts,
    u.input_tokens, u.output_tokens, u.reasoning_tokens, u.cached_tokens,
    u.cache_read_tokens, u.cache_creation_tokens, u.total_tokens,
    u.final_provider, u.final_auth_id, u.final_auth_type, u.response_model,
    u.cpa_session_id, u.cpa_parent_session_id
FROM {db}.request_facts AS f
LEFT JOIN
(
    SELECT
        lifecycle_request_id,
        count() AS attempts,
        countIf(failed) AS failed_attempts,
        sum(input_tokens) AS input_tokens,
        sum(output_tokens) AS output_tokens,
        sum(reasoning_tokens) AS reasoning_tokens,
        sum(cached_tokens) AS cached_tokens,
        sum(cache_read_tokens) AS cache_read_tokens,
        sum(cache_creation_tokens) AS cache_creation_tokens,
        sum(total_tokens) AS total_tokens,
        argMax(provider, requested_at) AS final_provider,
        argMax(auth_id, requested_at) AS final_auth_id,
        argMax(auth_type, requested_at) AS final_auth_type,
        argMax(response_model, requested_at) AS response_model,
        argMax(session_id, requested_at) AS cpa_session_id,
        argMax(parent_session_id, requested_at) AS cpa_parent_session_id
    FROM {db}.usage_by_request
    WHERE lifecycle_request_id != ''
    GROUP BY lifecycle_request_id
) AS u ON u.lifecycle_request_id = f.request_id
COMMENT 'request_facts (without message arrays) plus usage per request: tokens summed over attempts, attempts = upstream attempts (0 = no usage matched), final_* = last attempt, cpa_session_id = CPA session identity. For tokens by client/model/day and failure/retry analysis.'`,
}

func schemaFor(stmts []string, db string) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = strings.ReplaceAll(s, "{db}", db)
	}
	return out
}
