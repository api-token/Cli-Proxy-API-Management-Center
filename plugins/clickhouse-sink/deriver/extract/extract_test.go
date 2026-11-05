package extract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Fixtures are synthetic. Each directory holds row.json (raw metadata),
// request.json (request_body) and either chunks.sse (response_chunks, one
// chunk per block separated by a line holding only "%%") or response.json.
func loadFixture(t *testing.T, name string) Row {
	t.Helper()
	dir := filepath.Join("testdata", name)
	var row Row
	data, err := os.ReadFile(filepath.Join(dir, "row.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("%s/row.json: %v", name, err)
	}
	req, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	row.RequestBody = string(req)
	if chunks, err := os.ReadFile(filepath.Join(dir, "chunks.sse")); err == nil {
		row.ResponseChunks = strings.Split(strings.TrimRight(string(chunks), "\n"), "\n%%\n")
	}
	if resp, err := os.ReadFile(filepath.Join(dir, "response.json")); err == nil {
		row.ResponseBody = string(resp)
	}
	row.StartedAt = "2026-11-04 10:00:00.000000001"
	row.IngestedAt = "2026-11-04 10:00:01.000000001"
	return row
}

func types(msgs []Message, direction string) []string {
	var out []string
	for _, m := range msgs {
		if m.Direction == direction {
			out = append(out, m.Role+"/"+m.Type)
		}
	}
	return out
}

func find(t *testing.T, msgs []Message, direction, typ string) Message {
	t.Helper()
	for _, m := range msgs {
		if m.Direction == direction && m.Type == typ {
			return m
		}
	}
	t.Fatalf("no %s message of type %s in %+v", direction, typ, msgs)
	return Message{}
}

func eq[T any](t *testing.T, field string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", field, got, want)
	}
}

func TestChatStream(t *testing.T) {
	f := Extract(loadFixture(t, "chat_stream"), false)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "kind", f.Kind, "chat")
	eq(t, "client", f.Client, "OpenAI")
	eq(t, "conversation_source", f.ConversationSource, "inferred")
	eq(t, "response_id", f.ResponseID, "chatcmpl-test-1")
	eq(t, "stop_reason", f.StopReason, "tool_calls")
	eq(t, "reply_text", f.ReplyText, "Let me check.")
	eq(t, "tools_offered", f.ToolsOffered, []string{"get_weather", "get_time"})
	eq(t, "tools_called", f.ToolsCalled, []string{"get_weather", "get_time"})
	eq(t, "input", types(f.Messages, "input"), []string{"system/message", "user/message"})
	eq(t, "output", types(f.Messages, "output"), []string{"assistant/message", "assistant/tool_call", "assistant/tool_call"})
	out := f.Messages[len(f.Messages)-2:]
	eq(t, "call0", [3]string{out[0].CallID, out[0].ToolName, out[0].Arguments}, [3]string{"call_a", "get_weather", `{"city":"Paris"}`})
	eq(t, "call1", [3]string{out[1].CallID, out[1].ToolName, out[1].Arguments}, [3]string{"call_b", "get_time", `{"tz":"CET"}`})
	user := f.Messages[1]
	eq(t, "user text", user.Text, "Weather and time in Paris?\n[image]")
	if f.SystemPromptHash == 0 || user.TextHash != hashText(user.Text) {
		t.Errorf("hashes not set: %d %d", f.SystemPromptHash, user.TextHash)
	}
}

func TestResponsesHTTPStream(t *testing.T) {
	f := Extract(loadFixture(t, "responses_http"), false)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "kind", f.Kind, "responses")
	eq(t, "client", f.Client, "codex_cli_rs")
	eq(t, "conversation_id", f.ConversationID, "codex:sess-0001")
	eq(t, "conversation_source", f.ConversationSource, "codex-header")
	eq(t, "request_kind", f.RequestKind, "turn")
	eq(t, "turn_id", f.TurnID, "turn-0001")
	eq(t, "response_id", f.ResponseID, "resp_test_http")
	eq(t, "stop_reason", f.StopReason, "completed")
	eq(t, "reply_text", f.ReplyText, "Listing now.")
	eq(t, "reasoning_text", f.ReasoningText, "Need to list files.")
	eq(t, "tools_offered", f.ToolsOffered, []string{"exec", "wait"})
	eq(t, "tools_called", f.ToolsCalled, []string{"exec"})
	eq(t, "input", types(f.Messages, "input"), []string{"developer/message", "developer/additional_tools", "user/message"})
	eq(t, "output", types(f.Messages, "output"), []string{"assistant/reasoning", "assistant/message", "assistant/custom_tool_call"})
	call := find(t, f.Messages, "output", "custom_tool_call")
	eq(t, "call", [2]string{call.CallID, call.Arguments}, [2]string{"call_x1", "ls -la"})
	eq(t, "last_user_text", f.LastUserText, "List the files.")
	if f.SystemPromptHash != hashText("You are a coding agent.") {
		t.Errorf("system_prompt_hash should come from the leading developer message")
	}
	for _, m := range f.Messages {
		if strings.Contains(m.Text+m.Arguments, "Z0FBQUFB") {
			t.Errorf("encrypted_content leaked into %+v", m)
		}
	}
}

func TestResponsesWebsocketTurn(t *testing.T) {
	f := Extract(loadFixture(t, "responses_ws"), true)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "turn_id from body client_metadata", f.TurnID, "turn-0002")
	eq(t, "conversation_id", f.ConversationID, "codex:sess-0002")
	eq(t, "previous_response_id", f.PreviousResponseID, "resp_test_ws_1")
	eq(t, "response_id", f.ResponseID, "resp_test_ws_2")
	eq(t, "reply_text (completed.output wins over deltas)", f.ReplyText, "Found README.md.")
	// A websocket delta turn has no model item in its input: all of it is new.
	eq(t, "input", types(f.Messages, "input"), []string{"tool/custom_tool_call_output"})
	out := find(t, f.Messages, "input", "custom_tool_call_output")
	eq(t, "output text", out.Text, "Script completed\nOutput:\nREADME.md")
	eq(t, "call_id", out.CallID, "call_w1")
}

func TestClaudeCodeStream(t *testing.T) {
	row := loadFixture(t, "claude_code_stream")
	f := Extract(row, true)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "kind", f.Kind, "messages")
	eq(t, "client", f.Client, "claude-code")
	eq(t, "conversation_id", f.ConversationID, "claude:cc-sess-0001")
	eq(t, "conversation_source", f.ConversationSource, "claude-metadata")
	eq(t, "parent", f.ParentConversationID, "")
	eq(t, "response_id", f.ResponseID, "msg_test_c1")
	eq(t, "stop_reason", f.StopReason, "tool_use")
	eq(t, "reply_text", f.ReplyText, "Reading the file.")
	eq(t, "reasoning_text", f.ReasoningText, "Look at pkg/x.")
	eq(t, "tools_offered", f.ToolsOffered, []string{"Bash", "Read"})
	eq(t, "tools_called", f.ToolsCalled, []string{"Read"})
	// priorSeen: only items after the last assistant block (tool_use) are emitted.
	eq(t, "input", types(f.Messages, "input"), []string{"tool/tool_result", "user/message"})
	res := find(t, f.Messages, "input", "tool_result")
	eq(t, "tool_result", [2]any{res.CallID, res.IsError}, [2]any{"toolu_1", true})
	eq(t, "tool_result position", res.Position, uint32(5)) // system, user, thinking, text, tool_use, tool_result
	eq(t, "output", types(f.Messages, "output"), []string{"assistant/thinking", "assistant/message", "assistant/tool_use"})
	tu := find(t, f.Messages, "output", "tool_use")
	eq(t, "tool_use args", tu.Arguments, `{"file_path":"/tmp/x.go"}`)
	eq(t, "system prompt hash", f.SystemPromptHash, hashText("You are Claude Code.\nBe brief."))

	first := Extract(row, false)
	eq(t, "first request emits all input", len(types(first.Messages, "input")), 7)
	eq(t, "input_items", first.InputItems, uint32(7))
}

func TestGeminiStream(t *testing.T) {
	f := Extract(loadFixture(t, "gemini_stream"), false)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "kind", f.Kind, "generate")
	eq(t, "client", f.Client, "GeminiCLI")
	eq(t, "conversation_source", f.ConversationSource, "inferred")
	if !strings.HasPrefix(f.ConversationID, "fp:") {
		t.Errorf("conversation_id = %q, want fp: prefix", f.ConversationID)
	}
	eq(t, "response_id", f.ResponseID, "gem-resp-1")
	eq(t, "stop_reason", f.StopReason, "STOP")
	eq(t, "reply_text", f.ReplyText, "There are two docs.")
	eq(t, "reasoning_text", f.ReasoningText, "Thinking about it")
	eq(t, "tools_offered", f.ToolsOffered, []string{"search_docs"})
	eq(t, "tools_called", f.ToolsCalled, []string{"search_docs"})
	eq(t, "input", types(f.Messages, "input"), []string{"system/message", "user/message", "user/inline_data", "assistant/function_call", "tool/function_response"})
	eq(t, "output", types(f.Messages, "output"), []string{"assistant/thinking", "assistant/message", "assistant/function_call"})
}

func TestResponsesCanceledStream(t *testing.T) {
	f := Extract(loadFixture(t, "responses_canceled"), false)
	eq(t, "parse_status", f.ParseStatus, StatusPartial)
	eq(t, "outcome", f.Outcome, "canceled")
	eq(t, "response_id", f.ResponseID, "resp_test_cancel")
	eq(t, "reply_text from deltas", f.ReplyText, "Once upon a time")
	eq(t, "stop_reason", f.StopReason, "")
	call := find(t, f.Messages, "output", "function_call")
	eq(t, "partial args", [2]string{call.ToolName, call.Arguments}, [2]string{"save", `{"title":"St`})
	eq(t, "tools_called", f.ToolsCalled, []string{"save"})
}

func TestCodexMemoryCall(t *testing.T) {
	f := Extract(loadFixture(t, "codex_memory"), false)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "conversation_id", f.ConversationID, "codex:sess-0001:memory")
	eq(t, "request_kind", f.RequestKind, "memory")
	eq(t, "thread_source", f.ThreadSource, "memory_consolidation")
	eq(t, "response_id", f.ResponseID, "resp_test_mem")
	eq(t, "tools_offered", f.ToolsOffered, []string{"web_search"})
	eq(t, "input", types(f.Messages, "input"), []string{"system/message", "user/message"})
	eq(t, "reply_text", f.ReplyText, `{"rollout_summary":"synthetic"}`)
	eq(t, "Conversation()", Conversation(loadFixture(t, "codex_memory")), f.ConversationID)
}

func TestClaudePrefill(t *testing.T) {
	f := Extract(loadFixture(t, "claude_prefill"), true)
	eq(t, "parse_status", f.ParseStatus, StatusOK)
	eq(t, "conversation_id", f.ConversationID, "claude:cc-sess-0002")
	eq(t, "conversation_source", f.ConversationSource, "claude-header")
	eq(t, "client", f.Client, "claude-code")
	// The trailing assistant prefill is not a boundary; the tool_use before it is.
	eq(t, "input", types(f.Messages, "input"), []string{"tool/tool_result", "user/message", "assistant/message"})
	eq(t, "reply_text", f.ReplyText, `"Ada"}`)
	eq(t, "stop_reason", f.StopReason, "end_turn")
	eq(t, "last_user_text", f.LastUserText, "Return it as JSON.")
}

// Tail rule on a Codex-shaped input: reasoning and custom_tool_call are
// boundaries; *_output, additional_tools and user messages are not.
func TestTailRuleCodexShape(t *testing.T) {
	body := `{"input":[
	 {"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"q1"}]},
	 {"type":"reasoning","summary":[],"encrypted_content":"x"},
	 {"type":"custom_tool_call","call_id":"c1","name":"exec","input":"ls"},
	 {"type":"custom_tool_call_output","call_id":"c1","output":"ok"},
	 {"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"late_tool"}]},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"q2"}]}
	]}`
	row := Row{SourceFormat: "openai-response", Path: "/v1/responses", RequestBody: body, RequestHeaders: map[string]string{"session-id": "s"}}
	f := Extract(row, true)
	eq(t, "input", types(f.Messages, "input"), []string{"tool/custom_tool_call_output", "developer/additional_tools", "user/message"})
	eq(t, "positions", []uint32{f.Messages[0].Position, f.Messages[2].Position}, []uint32{4, 6})
	eq(t, "tools_offered", f.ToolsOffered, []string{"late_tool"})
	eq(t, "no response is partial", f.ParseStatus, StatusPartial)

	// A function_call/reasoning ending the input is itself the boundary: nothing new.
	body2 := `{"input":[{"type":"message","role":"user","content":"q"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}`
	f2 := Extract(Row{SourceFormat: "openai-response", RequestBody: body2}, true)
	eq(t, "input2", types(f2.Messages, "input"), []string(nil))
}

// Tail rule on a Claude Code-shaped input with several tool rounds.
func TestTailRuleClaudeShape(t *testing.T) {
	body := `{"messages":[
	 {"role":"user","content":"start"},
	 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r1"}]},
	 {"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"tool_use","id":"t2","name":"Read","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"r2"},{"type":"text","text":"<system-reminder>x</system-reminder>"}]}
	]}`
	f := Extract(Row{SourceFormat: "claude", Path: "/v1/messages", RequestBody: body}, true)
	eq(t, "input", types(f.Messages, "input"), []string{"tool/tool_result", "user/message"})
	eq(t, "call", f.Messages[0].CallID, "t2")
	f0 := Extract(Row{SourceFormat: "claude", Path: "/v1/messages", RequestBody: body}, false)
	eq(t, "first sees all", len(f0.Messages), 7)
}

func TestClaudeSubagentParent(t *testing.T) {
	body := `{"metadata":{"user_id":"{\"session_id\":\"child-1\",\"parent_session_id\":\"parent-1\"}"},"messages":[{"role":"user","content":"hi"}]}`
	f := Extract(Row{SourceFormat: "claude", RequestBody: body}, false)
	eq(t, "conversation_id", f.ConversationID, "claude:child-1")
	eq(t, "parent", f.ParentConversationID, "claude:parent-1")

	legacy := `{"metadata":{"user_id":"user_abc_account_def_session_0a1b2c3d-0000-4000-8000-000000000001"},"messages":[]}`
	f = Extract(Row{SourceFormat: "claude", RequestBody: legacy}, false)
	eq(t, "legacy conversation_id", f.ConversationID, "claude:0a1b2c3d-0000-4000-8000-000000000001")

	agent := Extract(Row{SourceFormat: "claude", RequestBody: `{"messages":[]}`,
		RequestHeaders: map[string]string{"x-claude-code-session-id": "s1", "x-claude-code-agent-id": "a7"}}, false)
	eq(t, "agent conversation_id", agent.ConversationID, "claude:s1:agent:a7")
	eq(t, "agent parent", agent.ParentConversationID, "claude:s1")
}

func TestCodexSubagentParent(t *testing.T) {
	h := map[string]string{"session-id": "root-1", "thread-id": "child-9"}
	f := Extract(Row{SourceFormat: "openai-response", RequestBody: `{"input":"x"}`, RequestHeaders: h}, false)
	eq(t, "conversation_id", f.ConversationID, "codex:child-9")
	eq(t, "parent", f.ParentConversationID, "codex:root-1")
}

func TestChatNonStreamAndKinds(t *testing.T) {
	row := Row{SourceFormat: "openai", Path: "/v1/chat/completions",
		RequestBody:  `{"messages":[{"role":"user","content":"hi"}]}`,
		ResponseBody: `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`}
	f := Extract(row, false)
	eq(t, "reply", f.ReplyText, "hello")
	eq(t, "status", f.ParseStatus, StatusOK)
	eq(t, "client", f.Client, "unknown")

	for path, want := range map[string]string{
		"/v1/messages/count_tokens":        "count_tokens",
		"/v1/responses/compact":            "compact",
		"/v1beta/models/m:generateContent": "generate",
		"/v1beta/models/m:countTokens":     "count_tokens",
		"/v1/models":                       "models",
		"":                                 "unknown",
		"/v1internal:streamGenerateContent?alt=sse": "generate",
		"/api/provider/openai/v1/chat/completions":  "chat",
		"/backend-api/codex/responses":              "responses",
		"/v1/images/generations":                    "generations",
	} {
		eq(t, "kind("+path+")", kindOf(path), want)
	}
}

func TestSanitize(t *testing.T) {
	long := strings.Repeat("QUJD", 200)
	in := `see data:image/png;base64,iVBORw0KGgo= and {"encrypted_content":"abc123"} and ` + long
	got := sanitize(in)
	want := `see data:image/png;base64,[omitted] and {"encrypted_content":"[omitted]"} and [base64 omitted]`
	eq(t, "sanitize", got, want)
	eq(t, "redaction markers kept", sanitize("key [REDACTED:known-value:0a1b2c3d] here"), "key [REDACTED:known-value:0a1b2c3d] here")
}

func TestStatusesWithoutBodies(t *testing.T) {
	eq(t, "empty", Extract(Row{SourceFormat: "claude"}, false).ParseStatus, StatusEmpty)
	eq(t, "redaction failed", Extract(Row{SourceFormat: "claude", RedactionCount: -1}, false).ParseStatus, StatusRedactionFailed)
	eq(t, "not json", Extract(Row{SourceFormat: "claude", RequestBody: "garbage"}, false).ParseStatus, StatusUnparseable)
	eq(t, "unsupported", Extract(Row{SourceFormat: "codex-images", Path: "/v1/images/generations", RequestBody: `{"prompt":"x"}`}, false).ParseStatus, StatusUnsupported)
	failed := Extract(Row{SourceFormat: "claude", Outcome: "failed", RequestBody: `{"messages":[]}`, ResponseBody: `{"type":"error","error":{"message":"overloaded"}}`}, false)
	eq(t, "failed with error body", failed.ParseStatus, StatusOK)
}

func TestPanicIsRecovered(t *testing.T) {
	old := extractImpl
	extractImpl = func(Row, bool) Facts { panic("boom") }
	defer func() { extractImpl = old }()
	f := Extract(Row{RequestID: "r1", IngestedAt: "2026-11-04 00:00:00.000000000", RequestBody: "{}"}, false)
	eq(t, "status", f.ParseStatus, StatusUnparseable)
	eq(t, "request_id", f.RequestID, "r1")
	eq(t, "ingested_at", f.IngestedAt, "2026-11-04 00:00:00.000000000")
	if !strings.Contains(f.ParseError, "boom") {
		t.Errorf("parse_error = %q", f.ParseError)
	}
}

// v0 rows stored stream events newline-joined in response_body.
func TestNewlineJoinedBody(t *testing.T) {
	row := loadFixture(t, "responses_http")
	row.ResponseBody = strings.Join(row.ResponseChunks, "\n")
	row.ResponseChunks = nil
	f := Extract(row, false)
	eq(t, "status", f.ParseStatus, StatusOK)
	eq(t, "reply", f.ReplyText, "Listing now.")
}

func TestInvalidJSONRequest(t *testing.T) {
	f := Extract(Row{SourceFormat: "openai", RequestBody: "{not json"}, false)
	eq(t, "status", f.ParseStatus, StatusUnparseable)
	eq(t, "conversation", Conversation(Row{SourceFormat: "openai", RequestBody: "{not json"}), "")
}
