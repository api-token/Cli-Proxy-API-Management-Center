package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The host serializes pluginapi types with Go's default field names and
// encodes []byte as base64, which encoding/json decodes for us.

type interceptRequest struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Headers        http.Header
	Body           []byte
	Metadata       map[string]any
}

type responseRequest struct {
	RequestID       string
	ResponseHeaders http.Header
	Body            []byte
	StatusCode      int
}

type streamChunkRequest struct {
	RequestID       string
	ResponseHeaders http.Header
	Body            []byte
	ChunkIndex      int
}

type completion struct {
	RequestID      string
	TraceID        string
	SourceFormat   string
	Model          string
	RequestedModel string
	Stream         bool
	Outcome        string
	StatusCode     int
	Error          string
	StartedAt      time.Time
	CompletedAt    time.Time
	Metadata       map[string]any
}

type usageRecord struct {
	RequestID            string
	TraceID              string
	SelectionID          string
	Provider             string
	BaseURL              string
	ExecutorType         string
	Model                string
	Alias                string
	APIKey               string
	AccessProvider       string
	SessionID            string
	ParentSessionID      string
	AuthID               string
	AuthIndex            string
	AuthType             string
	Source               string
	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	ResponseModel        string
	Generate             bool
	Stream               bool
	RequestedAt          time.Time
	Latency              time.Duration
	TTFT                 time.Duration
	Failed               bool
	Failure              struct {
		StatusCode int
		Body       string
	}
	Detail struct {
		InputTokens         int64
		OutputTokens        int64
		ReasoningTokens     int64
		CachedTokens        int64
		CacheReadTokens     int64
		CacheCreationTokens int64
		TotalTokens         int64
	}
	ResponseHeaders http.Header
}

// requestRow and usageRow mirror the tables in schema.sql.
type requestRow struct {
	RequestID       string            `json:"request_id"`
	TraceID         string            `json:"trace_id"`
	StartedAt       string            `json:"started_at"`
	CompletedAt     string            `json:"completed_at"`
	SourceFormat    string            `json:"source_format"`
	Model           string            `json:"model"`
	RequestedModel  string            `json:"requested_model"`
	Path            string            `json:"path"`
	Stream          bool              `json:"stream"`
	Outcome         string            `json:"outcome"`
	StatusCode      int               `json:"status_code"`
	Error           string            `json:"error"`
	RequestHeaders  map[string]string `json:"request_headers"`
	RequestBody     string            `json:"request_body"`
	ResponseHeaders map[string]string `json:"response_headers"`
	ResponseBody    string            `json:"response_body"`
	ResponseChunks  []string          `json:"response_chunks"`
	RedactionCount  int               `json:"redaction_count"`
	RedactionRules  []string          `json:"redaction_rules"`
}

type usageRow struct {
	RequestID            string            `json:"request_id"`
	TraceID              string            `json:"trace_id"`
	SelectionID          string            `json:"selection_id"`
	RequestedAt          string            `json:"requested_at"`
	Provider             string            `json:"provider"`
	ExecutorType         string            `json:"executor_type"`
	BaseURL              string            `json:"base_url"`
	Model                string            `json:"model"`
	Alias                string            `json:"alias"`
	ResponseModel        string            `json:"response_model"`
	APIKeyHash           string            `json:"api_key_hash"`
	AccessProvider       string            `json:"access_provider"`
	SessionID            string            `json:"session_id"`
	ParentSessionID      string            `json:"parent_session_id"`
	AuthID               string            `json:"auth_id"`
	AuthIndex            string            `json:"auth_index"`
	AuthType             string            `json:"auth_type"`
	Source               string            `json:"source"`
	ReasoningEffort      string            `json:"reasoning_effort"`
	ServiceTier          string            `json:"service_tier"`
	EffectiveServiceTier string            `json:"effective_service_tier"`
	ResponseServiceTier  string            `json:"response_service_tier"`
	Stream               bool              `json:"stream"`
	Generate             bool              `json:"generate"`
	LatencyMS            int64             `json:"latency_ms"`
	TTFTMS               int64             `json:"ttft_ms"`
	Failed               bool              `json:"failed"`
	FailureStatus        int               `json:"failure_status"`
	FailureBody          string            `json:"failure_body"`
	InputTokens          int64             `json:"input_tokens"`
	OutputTokens         int64             `json:"output_tokens"`
	ReasoningTokens      int64             `json:"reasoning_tokens"`
	CachedTokens         int64             `json:"cached_tokens"`
	CacheReadTokens      int64             `json:"cache_read_tokens"`
	CacheCreationTokens  int64             `json:"cache_creation_tokens"`
	TotalTokens          int64             `json:"total_tokens"`
	ResponseHeaders      map[string]string `json:"response_headers"`
}

// pendingTTL drops captures whose completion never arrived, e.g. requests
// that were in flight when the plugin was reloaded.
const pendingTTL = time.Hour

type pending struct {
	startedAt       time.Time
	updatedAt       time.Time
	requestHeaders  http.Header
	requestBody     []byte
	path            string
	responseHeaders http.Header
	responseBody    []byte
	chunks          []string
}

// capture collects the request and response bodies of in-flight requests,
// keyed by the host's per-request ID, until the completion event arrives.
type capture struct {
	mu        sync.Mutex
	bodies    bool
	pending   map[string]*pending
	lastSweep time.Time
}

func newCapture() *capture {
	return &capture{bodies: true, pending: make(map[string]*pending)}
}

func (c *capture) setStoreBodies(v bool) {
	c.mu.Lock()
	c.bodies = v
	c.mu.Unlock()
}

func (c *capture) storeBodies() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies
}

func (c *capture) begin(req interceptRequest, now time.Time) {
	if req.RequestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	entry := &pending{startedAt: now, updatedAt: now, path: metadataString(req.Metadata, "request_path")}
	if c.bodies {
		entry.requestHeaders = req.Headers
		entry.requestBody = req.Body
	}
	c.pending[req.RequestID] = entry
}

func (c *capture) response(req responseRequest, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.pending[req.RequestID]
	if entry == nil || !c.bodies {
		return
	}
	entry.updatedAt = now
	entry.responseHeaders = req.ResponseHeaders
	entry.responseBody = req.Body
}

func (c *capture) chunk(req streamChunkRequest, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.pending[req.RequestID]
	if entry == nil || !c.bodies {
		return
	}
	entry.updatedAt = now
	if len(req.ResponseHeaders) > 0 {
		entry.responseHeaders = req.ResponseHeaders
	}
	if req.ChunkIndex < 0 || len(req.Body) == 0 {
		return
	}
	// Chunks are kept exactly as delivered: some formats send unframed SSE
	// lines and the client handler adds the framing.
	entry.chunks = append(entry.chunks, string(req.Body))
}

// complete removes the capture for a finished request and returns its row.
// Requests the plugin never saw begin still get a metadata-only row.
func (c *capture) complete(ev completion) requestRow {
	c.mu.Lock()
	entry := c.pending[ev.RequestID]
	delete(c.pending, ev.RequestID)
	c.mu.Unlock()

	row := requestRow{
		RequestID:      ev.RequestID,
		TraceID:        ev.TraceID,
		StartedAt:      formatTime(ev.StartedAt),
		CompletedAt:    formatTime(ev.CompletedAt),
		SourceFormat:   ev.SourceFormat,
		Model:          ev.Model,
		RequestedModel: ev.RequestedModel,
		Path:           metadataString(ev.Metadata, "request_path"),
		Stream:         ev.Stream,
		Outcome:        ev.Outcome,
		StatusCode:     ev.StatusCode,
		Error:          ev.Error,
	}
	if entry != nil {
		row.RequestHeaders = flattenHeaders(entry.requestHeaders)
		row.RequestBody = string(entry.requestBody)
		row.ResponseHeaders = flattenHeaders(entry.responseHeaders)
		row.ResponseBody = string(entry.responseBody)
		row.ResponseChunks = entry.chunks
		if row.Path == "" {
			row.Path = entry.path
		}
	}
	return row
}

func (c *capture) sweepLocked(now time.Time) {
	if now.Sub(c.lastSweep) < time.Minute {
		return
	}
	c.lastSweep = now
	for id, entry := range c.pending {
		if now.Sub(entry.updatedAt) > pendingTTL {
			delete(c.pending, id)
		}
	}
}

func (c *capture) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func usageRowFrom(r usageRecord, withHeaders bool) usageRow {
	row := usageRow{
		RequestID:            r.RequestID,
		TraceID:              r.TraceID,
		SelectionID:          r.SelectionID,
		RequestedAt:          formatTime(r.RequestedAt),
		Provider:             r.Provider,
		ExecutorType:         r.ExecutorType,
		BaseURL:              r.BaseURL,
		Model:                r.Model,
		Alias:                r.Alias,
		ResponseModel:        r.ResponseModel,
		APIKeyHash:           hashKey(r.APIKey),
		AccessProvider:       r.AccessProvider,
		SessionID:            r.SessionID,
		ParentSessionID:      r.ParentSessionID,
		AuthID:               r.AuthID,
		AuthIndex:            r.AuthIndex,
		AuthType:             r.AuthType,
		Source:               r.Source,
		ReasoningEffort:      r.ReasoningEffort,
		ServiceTier:          r.ServiceTier,
		EffectiveServiceTier: r.EffectiveServiceTier,
		ResponseServiceTier:  r.ResponseServiceTier,
		Stream:               r.Stream,
		Generate:             r.Generate,
		LatencyMS:            r.Latency.Milliseconds(),
		TTFTMS:               r.TTFT.Milliseconds(),
		Failed:               r.Failed,
		FailureStatus:        r.Failure.StatusCode,
		FailureBody:          r.Failure.Body,
		InputTokens:          r.Detail.InputTokens,
		OutputTokens:         r.Detail.OutputTokens,
		ReasoningTokens:      r.Detail.ReasoningTokens,
		CachedTokens:         r.Detail.CachedTokens,
		CacheReadTokens:      r.Detail.CacheReadTokens,
		CacheCreationTokens:  r.Detail.CacheCreationTokens,
		TotalTokens:          r.Detail.TotalTokens,
	}
	if row.LatencyMS < 0 {
		row.LatencyMS = 0
	}
	if row.TTFTMS < 0 {
		row.TTFTMS = 0
	}
	if withHeaders {
		row.ResponseHeaders = flattenHeaders(r.ResponseHeaders)
	}
	return row
}

// secretHeaders never reach the database.
var secretHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"api-key":             true,
	"x-goog-api-key":      true,
	"cookie":              true,
	"set-cookie":          true,
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		key := strings.ToLower(name)
		if secretHeaders[key] {
			continue
		}
		out[key] = strings.Join(values, ", ")
	}
	return out
}

// hashKey identifies a client API key without storing it.
func hashKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	// Nine digits: usage attribution compares attempt and request start
	// times, which can fall in the same millisecond (DESIGN.md A1).
	return t.UTC().Format("2006-01-02 15:04:05.000000000")
}

func metadataString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
