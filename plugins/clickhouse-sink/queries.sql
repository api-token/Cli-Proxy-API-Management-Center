-- Ready-made searches over the inference database (database `cliproxy`).
-- Run them in the ClickHouse page (https://<host>:8123/play, user
-- inference_reader) or over HTTP. Edit the values marked EDIT.
--
-- Message types are protocol-native (function_call, custom_tool_call,
-- tool_use, ...). Use role instead: a tool call is role = 'assistant' with a
-- tool_name, a tool result is role = 'tool'.
--
-- Where to look:
--   messages          one row per message, search here first
--   request_facts     one row per request: reply, reasoning, tools, identity
--   request_summary   request_facts + tokens/attempts/provider per request
--   requests / usage  raw rows; bodies are large, fetch one request_id at a time
-- Each query below is separated by a line holding only ';'.

-- 1. Text search across every message (user prompts, replies, tool calls
--    and tool output) in the last 30 days.
SELECT started_at, client, conversation_id, role, type, tool_name,
       substring(text, greatest(1, positionCaseInsensitive(text, 'EDIT phrase') - 120), 400) AS snippet
FROM cliproxy.messages
WHERE started_at > now() - INTERVAL 30 DAY
  AND (positionCaseInsensitive(text, 'EDIT phrase') > 0 OR positionCaseInsensitive(arguments, 'EDIT phrase') > 0)
ORDER BY started_at DESC
LIMIT 50
;
-- 2. Recent conversations: when, which client, how many requests, the
--    latest user message.
SELECT conversation_id, any(client) AS client, min(started_at) AS first_seen, max(started_at) AS last_seen,
       count() AS requests, argMax(substring(last_user_text, 1, 200), started_at) AS latest_prompt
FROM cliproxy.request_facts
WHERE kind NOT IN ('count_tokens', 'models') AND started_at > now() - INTERVAL 7 DAY
GROUP BY conversation_id
ORDER BY last_seen DESC
LIMIT 50
;
-- 3. Replay a conversation. Clients resend the full history, so the latest
--    request of the main thread (the conversation's most common system
--    prompt, which skips helper calls such as title generation) holds it all.
--    This returns that request's raw body and its reply.
WITH 'EDIT conversation_id' AS conv,
     (SELECT topK(1)(system_prompt_hash)[1] FROM cliproxy.request_facts WHERE conversation_id = conv) AS main_prompt
SELECT f.started_at, f.request_id, f.reply_text, r.request_body
FROM cliproxy.request_facts AS f
INNER JOIN cliproxy.requests AS r ON r.request_id = f.request_id
WHERE f.conversation_id = conv AND f.system_prompt_hash = main_prompt
ORDER BY f.started_at DESC
LIMIT 1
;
-- 4. Most-used tools per client in the last 30 days.
SELECT client, tool_name, count() AS calls
FROM cliproxy.messages
WHERE direction = 'output' AND role = 'assistant' AND tool_name != '' AND started_at > now() - INTERVAL 30 DAY
GROUP BY client, tool_name
ORDER BY calls DESC
LIMIT 50
;
-- 5. Failing tools: tool results flagged as errors, joined to the call that
--    produced them. Codex tool outputs carry no error flag, so for Codex this
--    falls back to the result text mentioning an error.
SELECT c.tool_name, count() AS failures, any(substring(res.text, 1, 300)) AS example
FROM cliproxy.messages AS res
INNER JOIN
(
    SELECT DISTINCT conversation_id, call_id, tool_name
    FROM cliproxy.messages
    WHERE role = 'assistant' AND tool_name != '' AND call_id != '' AND started_at > now() - INTERVAL 30 DAY
) AS c ON c.conversation_id = res.conversation_id AND c.call_id = res.call_id
WHERE res.role = 'tool' AND res.started_at > now() - INTERVAL 30 DAY
  AND (res.is_error OR match(res.text, '(?i)\\b(error|failed|exception|traceback|exit code [1-9])\\b'))
GROUP BY c.tool_name
ORDER BY failures DESC
LIMIT 30
;
-- 6. Prompts you repeat: identical user messages across conversations.
SELECT text_hash, count() AS times, uniq(conversation_id) AS conversations,
       min(started_at) AS first_seen, max(started_at) AS last_seen, any(substring(text, 1, 300)) AS prompt
FROM cliproxy.messages
WHERE role = 'user' AND type = 'message' AND length(text) > 20 AND started_at > now() - INTERVAL 90 DAY
GROUP BY text_hash
HAVING conversations > 1
ORDER BY times DESC
LIMIT 50
;
-- 7. Failed requests grouped by error, with the upstream status.
SELECT outcome, status_code, substring(error, 1, 160) AS error_start, count() AS requests,
       groupUniqArray(5)(model) AS models, max(started_at) AS last_seen
FROM cliproxy.request_summary
WHERE outcome != 'succeeded' AND started_at > now() - INTERVAL 30 DAY
GROUP BY outcome, status_code, error_start
ORDER BY requests DESC
LIMIT 50
;
-- 8. Tokens per day, client and model (memory and other background calls
--    are separated by request_kind).
SELECT toDate(started_at) AS day, client, model, request_kind, count() AS requests,
       sum(input_tokens) AS input, sum(cache_read_tokens) AS cache_read,
       sum(output_tokens) AS output, sum(reasoning_tokens) AS reasoning
FROM cliproxy.request_summary
WHERE started_at > now() - INTERVAL 30 DAY
GROUP BY day, client, model, request_kind
ORDER BY day DESC, input DESC
;
-- 9. Slowest requests in the last 7 days.
SELECT started_at, duration_ms, client, model, final_provider, attempts, output_tokens,
       substring(last_user_text, 1, 120) AS prompt
FROM cliproxy.request_summary
WHERE started_at > now() - INTERVAL 7 DAY AND kind NOT IN ('count_tokens', 'models')
ORDER BY duration_ms DESC
LIMIT 30
;
-- 10. Join checks (DESIGN.md A1). The first two catch usage attributed to the
--     wrong request or lost; expect "no usage" to flag requests whose usage is
--     still in flight for a few seconds. The third only catches clock or
--     ordering oddities. Non-zero counts for older rows mean investigate.
SELECT 'usage rows matched to no request' AS check, count() AS n
FROM cliproxy.usage_by_request
WHERE lifecycle_request_id = '' AND requested_at > now() - INTERVAL 30 DAY
UNION ALL
SELECT 'succeeded requests with no usage', count()
FROM cliproxy.request_summary
WHERE outcome = 'succeeded' AND attempts = 0 AND kind NOT IN ('count_tokens', 'models')
  AND started_at BETWEEN now() - INTERVAL 30 DAY AND now() - INTERVAL 1 MINUTE
UNION ALL
SELECT 'usage starting after its request completed + 5s', count()
FROM cliproxy.usage_by_request AS u
INNER JOIN cliproxy.requests AS r ON r.request_id = u.lifecycle_request_id
WHERE u.requested_at > r.completed_at + INTERVAL 5 SECOND AND u.requested_at > now() - INTERVAL 30 DAY
;
-- 11. Redaction audit: which rules fired and how often.
SELECT arrayJoin(redaction_rules) AS rule, count() AS requests, sum(redaction_count) AS replacements
FROM cliproxy.requests
WHERE started_at > now() - INTERVAL 30 DAY
GROUP BY rule
ORDER BY requests DESC
;
-- 12. Extraction health: rows the deriver could not fully parse.
SELECT parse_status, source_format, count() AS requests, any(parse_error) AS example_error
FROM cliproxy.request_facts
GROUP BY parse_status, source_format
ORDER BY requests DESC
