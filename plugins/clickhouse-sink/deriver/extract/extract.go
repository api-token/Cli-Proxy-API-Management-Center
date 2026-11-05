// Package extract turns one raw `requests` row into one `request_facts` row.
// It is pure: no I/O, no clock, no global state. The only input besides the
// row is priorSeen, which tells the tail rule whether an earlier request of the
// same conversation was already emitted.
package extract

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Version is stored in request_facts.extractor_version. Bump it whenever the
// output for the same raw row changes, then run a rebuild.
const Version uint16 = 2

// Parse statuses stored in request_facts.parse_status.
const (
	StatusOK              = "ok"               // request and response fully parsed
	StatusPartial         = "partial"          // response missing its terminal event (canceled/incomplete stream) or unreadable
	StatusUnsupported     = "unsupported"      // protocol not handled; only metadata is filled
	StatusEmpty           = "empty"            // no request body
	StatusRedactionFailed = "redaction_failed" // plugin emptied the bodies because redaction failed
	StatusUnparseable     = "unparseable"      // request body is not JSON, or the extractor panicked
)

// Row is a raw `requests` row as read by the deriver (column names as JSON tags).
// Timestamps are kept as ClickHouse text so they are copied without loss.
type Row struct {
	RequestID      string            `json:"request_id"`
	TraceID        string            `json:"trace_id"`
	StartedAt      string            `json:"started_at"`
	IngestedAt     string            `json:"ingested_at"`
	DurationMs     uint64            `json:"duration_ms"`
	Path           string            `json:"path"`
	SourceFormat   string            `json:"source_format"`
	Model          string            `json:"model"`
	Stream         bool              `json:"stream"`
	Outcome        string            `json:"outcome"`
	StatusCode     uint16            `json:"status_code"`
	Error          string            `json:"error"`
	RequestHeaders map[string]string `json:"request_headers"`
	RequestBody    string            `json:"request_body"`
	ResponseBody   string            `json:"response_body"`
	ResponseChunks []string          `json:"response_chunks"`
	RedactionCount int32             `json:"redaction_count"`
}

// Message is one emitted input or output item.
type Message struct {
	Direction string // "input" or "output"
	Position  uint32 // index in the normalized input list (system prompt first), or in the output list
	Role      string // system, developer, user, assistant, tool
	Type      string // protocol-native item/block type; plain text is always "message"
	ToolName  string
	CallID    string
	Arguments string // tool call arguments/input as sent (raw JSON or free text)
	IsError   bool   // tool result flagged as an error by the protocol
	Text      string
	TextHash  uint64
}

// Facts is one request_facts row.
type Facts struct {
	RequestID            string
	StartedAt            string
	IngestedAt           string
	Model                string
	SourceFormat         string
	Outcome              string
	StatusCode           uint16
	Stream               bool
	DurationMs           uint64
	Error                string
	Path                 string
	Kind                 string
	RequestKind          string
	ThreadSource         string
	TurnID               string
	Client               string
	ConversationID       string
	ConversationSource   string
	ParentConversationID string
	ResponseID           string
	PreviousResponseID   string
	SystemPromptHash     uint64
	LastUserText         string
	ReplyText            string
	ReasoningText        string
	ToolsOffered         []string
	ToolsCalled          []string
	StopReason           string
	PriorSeen            bool
	InputItems           uint32
	Messages             []Message
	ParseStatus          string
	ParseError           string
	ExtractorVersion     uint16
}

// item is the normalized form of one input or output element across protocols.
type item struct {
	role     string
	typ      string
	toolName string
	callID   string
	args     string
	text     string
	isError  bool
	model    bool // produced by the model (assistant text, reasoning, tool call)
	msg      int  // index of the protocol message the item came from (Claude/chat/Gemini blocks)
}

// parsed is what a protocol parser returns.
type parsed struct {
	system      string
	input       []item
	tools       []string
	prevRespID  string
	output      []item
	responseID  string
	stopReason  string
	complete    bool // a terminal event/body was seen
	anyResponse bool // something response-side was parsed
	respErr     string
}

// extractImpl is swapped by tests to exercise panic recovery.
var extractImpl = extract

// Extract builds the facts row. It never panics: a panic becomes an
// `unparseable` row so a bad raw row cannot block the deriver.
func Extract(row Row, priorSeen bool) (f Facts) {
	defer func() {
		if r := recover(); r != nil {
			f = Unparseable(row, fmt.Sprintf("panic: %v", r))
		}
	}()
	return extractImpl(row, priorSeen)
}

// Unparseable returns a metadata-only row with parse_status 'unparseable'.
// reason must not contain body content.
func Unparseable(row Row, reason string) Facts {
	f := base(row)
	f.ParseStatus = StatusUnparseable
	f.ParseError = truncate(reason, 300)
	return f
}

// Conversation returns the conversation_id Extract would assign, so the caller
// can look up priorSeen before calling Extract. Empty when undeterminable.
func Conversation(row Row) (id string) {
	defer func() {
		if recover() != nil {
			id = ""
		}
	}()
	body := requestRoot(row.RequestBody)
	if !body.IsObject() {
		return ""
	}
	format := protocol(row.SourceFormat, kindOf(row.Path))
	var p parsed
	if format != "" {
		p = parseRequest(format, body)
	}
	id, _, _ = identity(row, body, p)
	return id
}

func base(row Row) Facts {
	return Facts{
		RequestID:        row.RequestID,
		StartedAt:        row.StartedAt,
		IngestedAt:       row.IngestedAt,
		Model:            row.Model,
		SourceFormat:     row.SourceFormat,
		Outcome:          row.Outcome,
		StatusCode:       row.StatusCode,
		Stream:           row.Stream,
		DurationMs:       row.DurationMs,
		Error:            row.Error,
		Path:             row.Path,
		Kind:             kindOf(row.Path),
		Client:           clientOf(row.RequestHeaders),
		ToolsOffered:     []string{},
		ToolsCalled:      []string{},
		Messages:         []Message{},
		ExtractorVersion: Version,
	}
}

func extract(row Row, priorSeen bool) Facts {
	f := base(row)
	f.PriorSeen = priorSeen
	meta := turnMetadata(row)
	f.RequestKind = meta.Get("request_kind").String()
	f.ThreadSource = meta.Get("thread_source").String()
	f.TurnID = meta.Get("turn_id").String()

	if row.RedactionCount < 0 {
		f.ParseStatus = StatusRedactionFailed
		return f
	}
	if strings.TrimSpace(row.RequestBody) == "" {
		f.ParseStatus = StatusEmpty
		return f
	}
	body := requestRoot(row.RequestBody)
	if !body.IsObject() {
		f.ParseStatus = StatusUnparseable
		f.ParseError = "request body is not a JSON object"
		return f
	}

	format := protocol(row.SourceFormat, f.Kind)
	var p parsed
	if format != "" {
		p = parseRequest(format, body)
	}
	f.ConversationID, f.ConversationSource, f.ParentConversationID = identity(row, body, p)
	if format == "" {
		f.ParseStatus = StatusUnsupported
		return f
	}
	if f.Kind != "count_tokens" {
		parseResponse(format, row, &p)
	} else {
		p.complete = true
	}

	f.PreviousResponseID = p.prevRespID
	f.ResponseID = p.responseID
	f.StopReason = p.stopReason
	f.ToolsOffered = dedupe(p.tools)

	input := p.input
	if p.system != "" {
		input = append([]item{{role: "system", typ: "message", text: p.system, msg: -1}}, input...)
	}
	f.SystemPromptHash = hashText(sanitize(systemPrompt(p)))
	f.InputItems = uint32(len(input))
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].role == "user" && input[i].typ == "message" && strings.TrimSpace(input[i].text) != "" {
			f.LastUserText = sanitize(input[i].text)
			break
		}
	}

	start := tailStart(input, priorSeen)
	for i := start; i < len(input); i++ {
		f.Messages = append(f.Messages, toMessage("input", uint32(i), input[i]))
	}
	var reply, reasoning []string
	for i, it := range p.output {
		m := toMessage("output", uint32(i), it)
		f.Messages = append(f.Messages, m)
		switch {
		case it.model && it.typ == "message" && m.Text != "":
			reply = append(reply, m.Text)
		case (it.typ == "reasoning" || it.typ == "thinking") && m.Text != "":
			reasoning = append(reasoning, m.Text)
		}
		if it.toolName != "" && it.model {
			f.ToolsCalled = append(f.ToolsCalled, it.toolName)
		}
	}
	// Chunks that carried part of a secret are stored as a bare redaction
	// marker (see the redact package). Keep the markers in the reply so a
	// search shows that something was redacted there.
	if markers := splitMarkers(row.ResponseChunks); len(markers) > 0 {
		reply = append(reply, strings.Join(markers, " "))
		f.Messages = append(f.Messages, toMessage("output", uint32(len(p.output)), item{role: "assistant", typ: "message", text: strings.Join(markers, " "), model: true}))
	}
	f.ReplyText = strings.Join(reply, "\n")
	f.ReasoningText = strings.Join(reasoning, "\n")

	switch {
	case p.complete:
		f.ParseStatus = StatusOK
	case !p.anyResponse && (row.Outcome == "failed" || row.Outcome == "rejected"):
		f.ParseStatus = StatusOK
	default:
		f.ParseStatus = StatusPartial
		f.ParseError = p.respErr
	}
	return f
}

func toMessage(direction string, pos uint32, it item) Message {
	text := sanitize(it.text)
	return Message{
		Direction: direction,
		Position:  pos,
		Role:      it.role,
		Type:      it.typ,
		ToolName:  it.toolName,
		CallID:    it.callID,
		Arguments: sanitize(it.args),
		IsError:   it.isError,
		Text:      text,
		TextHash:  hashText(text),
	}
}

// tailStart applies the tail rule and returns the first input index to emit.
// First request of a conversation: everything. Otherwise everything after the
// last model-produced item, ignoring a trailing assistant prefill message.
func tailStart(input []item, priorSeen bool) int {
	if !priorSeen {
		return 0
	}
	end := len(input)
	if end > 0 && isPrefillMessage(input, end-1) {
		last := input[end-1].msg
		for end > 0 && input[end-1].msg == last && input[end-1].role == "assistant" {
			end--
		}
	}
	for i := end - 1; i >= 0; i-- {
		if input[i].model {
			return i + 1
		}
	}
	return 0
}

// isPrefillMessage reports whether the message containing input[i] is an
// assistant message made only of plain text (a prefill, not a model turn with
// tool calls or reasoning).
func isPrefillMessage(input []item, i int) bool {
	if input[i].role != "assistant" {
		return false
	}
	msg := input[i].msg
	for j := i; j >= 0 && input[j].msg == msg; j-- {
		if input[j].role != "assistant" || input[j].typ != "message" {
			return false
		}
	}
	return true
}

// systemPrompt is the top-level system prompt, else the leading
// system/developer messages (Codex sends its instructions that way).
func systemPrompt(p parsed) string {
	if p.system != "" {
		return p.system
	}
	var parts []string
	for _, it := range p.input {
		if (it.role != "system" && it.role != "developer") || it.typ != "message" {
			break
		}
		parts = append(parts, it.text)
	}
	return strings.Join(parts, "\n")
}

// protocol picks the parser from source_format, falling back to the path kind.
func protocol(sourceFormat, kind string) string {
	switch sourceFormat {
	case "openai":
		return "chat"
	case "openai-response":
		return "responses"
	case "claude":
		return "claude"
	case "gemini", "gemini-cli":
		return "gemini"
	}
	switch kind {
	case "chat":
		return "chat"
	case "responses", "compact":
		return "responses"
	case "messages":
		return "claude"
	case "generate":
		return "gemini"
	}
	return ""
}

func parseRequest(format string, body gjson.Result) parsed {
	switch format {
	case "chat":
		return chatRequest(body)
	case "responses":
		return responsesRequest(body)
	case "claude":
		return claudeRequest(body)
	case "gemini":
		return geminiRequest(body)
	}
	return parsed{}
}

func parseResponse(format string, row Row, p *parsed) {
	chunks := row.ResponseChunks
	if len(chunks) == 0 {
		raw := strings.TrimSpace(row.ResponseBody)
		if raw == "" {
			return
		}
		if body := gjson.Parse(raw); gjson.Valid(raw) && body.IsObject() {
			switch format {
			case "chat":
				chatResponse(body, p)
			case "responses":
				responsesResponse(body, p)
			case "claude":
				claudeResponse(body, p)
			case "gemini":
				geminiResponse(body, p)
			}
			return
		}
		// A JSON array (Gemini without alt=sse) or newline-joined events (v0 rows).
		chunks = []string{raw}
	}
	events := streamEvents(chunks)
	if len(events) == 0 {
		p.respErr = "response is not JSON"
		return
	}
	switch format {
	case "chat":
		chatStream(events, p)
	case "responses":
		responsesStream(events, p)
	case "claude":
		claudeStream(events, p)
	case "gemini":
		geminiStream(events, p)
	}
}

// requestRoot parses the request body, unwrapping Gemini CLI's {"request": {...}}.
// Invalid JSON yields an empty result (gjson alone is lenient).
func requestRoot(raw string) gjson.Result {
	if !gjson.Valid(raw) {
		return gjson.Result{}
	}
	body := gjson.Parse(raw)
	if req := body.Get("request"); req.IsObject() && !body.Get("contents").Exists() {
		return req
	}
	return body
}

// kindOf derives a short request kind from the client path.
func kindOf(path string) string {
	p := strings.ToLower(strings.TrimSpace(path))
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(p, "/")
	switch {
	case p == "":
		return "unknown"
	case strings.HasSuffix(p, "/chat/completions"):
		return "chat"
	case strings.HasSuffix(p, "/responses/compact"):
		return "compact"
	case strings.HasSuffix(p, "/responses"):
		return "responses"
	case strings.HasSuffix(p, "/messages/count_tokens"), strings.HasSuffix(p, ":counttokens"):
		return "count_tokens"
	case strings.HasSuffix(p, "/messages"):
		return "messages"
	case strings.HasSuffix(p, ":generatecontent"), strings.HasSuffix(p, ":streamgeneratecontent"):
		return "generate"
	case strings.HasSuffix(p, "/completions"):
		return "completions"
	case strings.HasSuffix(p, "/embeddings"), strings.HasSuffix(p, ":embedcontent"), strings.HasSuffix(p, ":batchembedcontents"):
		return "embeddings"
	case strings.HasSuffix(p, "/models") || strings.Contains(p, "/models/") && !strings.Contains(p, ":"):
		return "models"
	}
	if i := strings.LastIndexAny(p, "/:"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// clientOf names the calling client from its headers, `unknown` when nothing matches.
func clientOf(h map[string]string) string {
	if v := strings.TrimSpace(h["originator"]); v != "" {
		return v
	}
	ua := strings.TrimSpace(h["user-agent"])
	if h["x-claude-code-session-id"] != "" || strings.HasPrefix(strings.ToLower(ua), "claude-cli") {
		return "claude-code"
	}
	for k := range h {
		if strings.HasPrefix(k, "x-codex-") {
			return "codex"
		}
	}
	if ua != "" {
		token := ua
		if i := strings.IndexAny(token, "/ ("); i > 0 {
			token = token[:i]
		}
		if token != "" {
			return token
		}
	}
	return "unknown"
}

// turnMetadata returns Codex's x-codex-turn-metadata. The per-request copy in
// the body's client_metadata wins: on a websocket the header belongs to the
// upgrade request and is stale for later turns.
func turnMetadata(row Row) gjson.Result {
	body := gjson.Parse(row.RequestBody)
	if v := body.Get("client_metadata.x-codex-turn-metadata").String(); gjson.Valid(v) {
		if r := gjson.Parse(v); r.IsObject() {
			return r
		}
	}
	if v := row.RequestHeaders["x-codex-turn-metadata"]; gjson.Valid(v) {
		return gjson.Parse(v)
	}
	return gjson.Result{}
}

var legacyClaudeSession = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// identity returns conversation_id, conversation_source and parent_conversation_id.
// Precedence: Claude Code session header, Claude metadata.user_id, Codex
// session/thread ids (body client_metadata, headers, turn metadata), generic
// session headers, then a fingerprint marked `inferred`. A non-`turn`
// request_kind is appended as `:<request_kind>`.
func identity(row Row, body gjson.Result, p parsed) (id, source, parent string) {
	h := row.RequestHeaders
	meta := turnMetadata(row)
	kind := meta.Get("request_kind").String()
	suffix := func(s string) string {
		if kind != "" && kind != "turn" {
			return s + ":" + kind
		}
		return s
	}

	claudeSID, claudeParent, claudeAgent := claudeMetadata(body)
	if agent := strings.TrimSpace(h["x-claude-code-agent-id"]); agent != "" {
		claudeAgent = agent
	}
	claudeIdentity := func(sid string) (string, string) {
		if claudeAgent != "" && claudeAgent != "main" {
			return "claude:" + sid + ":agent:" + claudeAgent, "claude:" + sid
		}
		if claudeParent != "" && claudeParent != sid {
			return "claude:" + sid, "claude:" + claudeParent
		}
		return "claude:" + sid, ""
	}
	if sid := strings.TrimSpace(h["x-claude-code-session-id"]); sid != "" {
		id, parent = claudeIdentity(sid)
		return suffix(id), "claude-header", parent
	}
	if claudeSID != "" {
		id, parent = claudeIdentity(claudeSID)
		return suffix(id), "claude-metadata", parent
	}

	cm := body.Get("client_metadata")
	first := func(vals ...string) string {
		for _, v := range vals {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return ""
	}
	sid := first(cm.Get("session_id").String(), h["session-id"], h["session_id"], meta.Get("session_id").String())
	tid := first(cm.Get("thread_id").String(), h["thread-id"], h["thread_id"], meta.Get("thread_id").String())
	if sid != "" || tid != "" {
		conv := first(tid, sid)
		parent = first(h["x-codex-parent-thread-id"], meta.Get("parent_thread_id").String(), meta.Get("forked_from_thread_id").String())
		if parent == "" && sid != "" && tid != "" && sid != tid {
			parent = sid
		}
		if parent != "" && parent != conv {
			parent = "codex:" + parent
		} else {
			parent = ""
		}
		return suffix("codex:" + conv), "codex-header", parent
	}

	for _, k := range []string{"x-session-id", "x-conversation-id", "conversation-id", "x-thread-id"} {
		if v := strings.TrimSpace(h[k]); v != "" {
			return suffix("session:" + v), "header", ""
		}
	}

	// Fingerprint: protocol + client + system prompt + first user message.
	// Stable across turns of a stateless chat, so later turns hit priorSeen.
	var firstUser string
	for _, it := range p.input {
		if it.role == "user" && it.typ == "message" {
			firstUser = it.text
			break
		}
	}
	if firstUser == "" && systemPrompt(p) == "" {
		return "", "", ""
	}
	sum := sha256.Sum256([]byte(row.SourceFormat + "\x00" + clientOf(h) + "\x00" + systemPrompt(p) + "\x00" + firstUser))
	return suffix(fmt.Sprintf("fp:%x", sum[:8])), "inferred", ""
}

// claudeMetadata reads session, parent session and agent from Claude's
// metadata.user_id (JSON form or legacy `..._session_<uuid>`).
func claudeMetadata(body gjson.Result) (sid, parent, agent string) {
	userID := strings.TrimSpace(body.Get("metadata.user_id").String())
	if userID == "" {
		return "", "", ""
	}
	if strings.HasPrefix(userID, "{") {
		u := gjson.Parse(userID)
		sid = strings.TrimSpace(u.Get("session_id").String())
		parent = strings.TrimSpace(u.Get("parent_session_id").String())
		agent = strings.TrimSpace(u.Get("agent_id").String())
		return sid, parent, agent
	}
	if m := legacyClaudeSession.FindStringSubmatch(userID); m != nil {
		return m[1], strings.TrimSpace(body.Get("metadata.parent_session_id").String()), strings.TrimSpace(body.Get("metadata.agent_id").String())
	}
	return "", "", ""
}

var (
	dataURL      = regexp.MustCompile(`data:([a-zA-Z0-9.+/-]+);base64,[A-Za-z0-9+/=]+`)
	encrypted    = regexp.MustCompile(`"encrypted_content"\s*:\s*"[^"]*"`)
	longBase64   = regexp.MustCompile(`[A-Za-z0-9+/]{512,}={0,2}`)
	sanitizeHint = []string{"base64,", "encrypted_content"}
)

// sanitize removes inline base64 media and encrypted reasoning blobs from text.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	for _, hint := range sanitizeHint {
		if strings.Contains(s, hint) {
			s = dataURL.ReplaceAllString(s, "data:$1;base64,[omitted]")
			s = encrypted.ReplaceAllString(s, `"encrypted_content":"[omitted]"`)
			break
		}
	}
	if len(s) >= 512 {
		s = longBase64.ReplaceAllString(s, "[base64 omitted]")
	}
	return s
}

// hashText groups identical texts (whitespace-trimmed). 0 for empty text.
func hashText(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	sum := sha256.Sum256([]byte(s))
	return binary.BigEndian.Uint64(sum[:8])
}

// contentText flattens a string or an array of content parts into text.
// Media parts become placeholders such as "[image]".
func contentText(c gjson.Result) string {
	if c.Type == gjson.String {
		return c.String()
	}
	if !c.IsArray() {
		if c.IsObject() {
			return partText(c)
		}
		return c.String()
	}
	var parts []string
	for _, p := range c.Array() {
		if t := partText(p); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func partText(p gjson.Result) string {
	if p.Type == gjson.String {
		return p.String()
	}
	typ := p.Get("type").String()
	switch typ {
	case "text", "input_text", "output_text", "":
		return p.Get("text").String()
	case "refusal":
		return p.Get("refusal").String()
	case "image", "input_image", "image_url":
		return "[image]"
	case "document", "input_file", "file":
		return "[file]"
	case "input_audio", "audio":
		return "[audio]"
	case "tool_result":
		return contentText(p.Get("content"))
	}
	if t := p.Get("text"); t.Type == gjson.String {
		return t.String()
	}
	return "[" + typ + "]"
}

func dedupe(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// HashText is the text_hash of s, for callers that rewrite message text
// after extraction (the deriver's redaction pass).
func HashText(s string) uint64 { return hashText(s) }

var markerChunk = regexp.MustCompile(`^\[REDACTED:[A-Za-z0-9_.-]+:[0-9a-f]{8}\]$`)

// splitMarkers returns the distinct redaction markers that replaced whole
// stream chunks, in order.
func splitMarkers(chunks []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range chunks {
		if markerChunk.MatchString(c) && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
