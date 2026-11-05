package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func call(t *testing.T, p *plugin, method string, payload any) {
	t.Helper()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errHandle := p.handle(method, raw); errHandle != nil {
		t.Fatalf("%s: %v", method, errHandle)
	}
}

func TestStreamCaptureBuildsRow(t *testing.T) {
	p := &plugin{capture: newCapture()}
	start := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	call(t, p, "request.intercept_before", map[string]any{
		"RequestID": "r1", "TraceID": "abcd1234",
		"Headers": http.Header{"Authorization": {"Bearer secret"}, "User-Agent": {"cli"}},
		"Body":    []byte(`{"model":"m","stream":true}`),
	})
	call(t, p, "response.intercept_stream_chunk", map[string]any{
		"RequestID": "r1", "ChunkIndex": -1, "ResponseHeaders": http.Header{"Content-Type": {"text/event-stream"}},
	})
	call(t, p, "response.intercept_stream_chunk", map[string]any{"RequestID": "r1", "ChunkIndex": 0, "Body": []byte("data: a")})
	call(t, p, "response.intercept_stream_chunk", map[string]any{"RequestID": "r1", "ChunkIndex": 1, "Body": []byte("data: b\n")})

	row := p.capture.complete(completion{
		RequestID: "r1", TraceID: "abcd1234", Outcome: "succeeded", StatusCode: 200,
		Stream: true, StartedAt: start, CompletedAt: start.Add(1500 * time.Millisecond),
	})
	if row.RequestBody != `{"model":"m","stream":true}` {
		t.Errorf("request body = %q", row.RequestBody)
	}
	if row.ResponseBody != "data: a\ndata: b\n" || row.ResponseChunks != 2 {
		t.Errorf("response = %q chunks=%d", row.ResponseBody, row.ResponseChunks)
	}
	if _, ok := row.RequestHeaders["authorization"]; ok {
		t.Error("authorization header was stored")
	}
	if row.RequestHeaders["user-agent"] != "cli" || row.ResponseHeaders["content-type"] != "text/event-stream" {
		t.Errorf("headers = %v / %v", row.RequestHeaders, row.ResponseHeaders)
	}
	if row.StartedAt != "2026-11-04 12:00:00.000" || row.CompletedAt != "2026-11-04 12:00:01.500" {
		t.Errorf("times = %s / %s", row.StartedAt, row.CompletedAt)
	}
	if p.capture.size() != 0 {
		t.Error("capture not released after completion")
	}
}

func TestStoreBodiesFalseKeepsMetadataOnly(t *testing.T) {
	p := &plugin{capture: newCapture()}
	p.capture.setStoreBodies(false)
	call(t, p, "request.intercept_before", map[string]any{"RequestID": "r1", "Body": []byte(`{}`)})
	call(t, p, "response.intercept_after", map[string]any{"RequestID": "r1", "Body": []byte(`{"ok":1}`)})
	row := p.capture.complete(completion{RequestID: "r1", Outcome: "succeeded"})
	if row.RequestBody != "" || row.ResponseBody != "" || row.Outcome != "succeeded" {
		t.Errorf("row = %+v", row)
	}
}

func TestUsageRowHashesKeyAndConvertsDurations(t *testing.T) {
	var record usageRecord
	record.APIKey = "sk-live"
	record.Latency = 2500 * time.Millisecond
	record.TTFT = 300 * time.Millisecond
	record.Detail.InputTokens = 10
	row := usageRowFrom(record, false)
	if row.APIKeyHash == "" || strings.Contains(row.APIKeyHash, "sk-live") {
		t.Errorf("api key hash = %q", row.APIKeyHash)
	}
	if row.LatencyMS != 2500 || row.TTFTMS != 300 || row.InputTokens != 10 {
		t.Errorf("row = %+v", row)
	}
}

// fakeClickHouse records statements and fails inserts while fail is set.
type fakeClickHouse struct {
	mu      sync.Mutex
	fail    int
	queries []string
	inserts map[string][]string
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	query := r.URL.Query().Get("query")
	if query == "" {
		f.queries = append(f.queries, string(body))
		return
	}
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		return
	}
	table := strings.Fields(query)[2]
	f.inserts[table] = append(f.inserts[table], strings.Split(string(body), "\n")...)
}

func TestSinkCreatesSchemaInsertsAndRetries(t *testing.T) {
	fake := &fakeClickHouse{inserts: map[string][]string{}, fail: http.StatusServiceUnavailable}
	server := httptest.NewServer(fake)
	defer server.Close()

	s := newSink(sinkConfig{URL: server.URL, Database: "cliproxy", Username: "default"})
	s.add(tableUsage, map[string]int{"a": 1})
	s.add(tableUsage, map[string]int{"a": 2})
	s.flushNow()
	if len(fake.inserts) != 0 || s.buffered == 0 {
		t.Fatalf("rows should stay buffered while ClickHouse fails")
	}

	fake.mu.Lock()
	fake.fail = 0
	fake.mu.Unlock()
	s.flushNow()
	if got := fake.inserts["cliproxy.usage"]; len(got) != 2 || got[0] != `{"a":1}` {
		t.Fatalf("inserted = %v", got)
	}
	if len(fake.queries) == 0 || !strings.Contains(strings.Join(fake.queries, "\n"), "CREATE TABLE IF NOT EXISTS cliproxy.requests") {
		t.Fatalf("schema not created: %v", fake.queries)
	}
	if s.buffered != 0 {
		t.Errorf("buffered = %d after success", s.buffered)
	}
}

func TestSinkDropsRowsClickHouseRejects(t *testing.T) {
	fake := &fakeClickHouse{inserts: map[string][]string{}, fail: http.StatusBadRequest}
	server := httptest.NewServer(fake)
	defer server.Close()

	s := newSink(sinkConfig{URL: server.URL, Database: "cliproxy"})
	s.add(tableUsage, map[string]int{"a": 1})
	s.flushNow()
	if s.buffered != 0 || len(s.rows) != 0 {
		t.Errorf("rejected rows should be dropped, buffered = %d", s.buffered)
	}
}

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	t.Setenv(passwordEnv, "from-env")
	cfg, errParse := parseConfig(nil)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if cfg.URL != defaultURL || cfg.Database != defaultDatabase || cfg.Password != "from-env" || !cfg.StoreBodies {
		t.Errorf("cfg = %+v", cfg)
	}
	if _, errBad := parseConfig([]byte("database: \"x; DROP\"")); errBad == nil {
		t.Error("invalid database name accepted")
	}
}

func TestRegistrationIsValidJSON(t *testing.T) {
	p := &plugin{capture: newCapture()}
	defer p.shutdown()
	raw, errHandle := p.handle("plugin.register", []byte(`{"config_yaml":""}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var reg struct {
		SchemaVersion int             `json:"schema_version"`
		Capabilities  map[string]bool `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(raw, &reg); errDecode != nil {
		t.Fatalf("registration: %v\n%s", errDecode, raw)
	}
	if reg.SchemaVersion != schemaVersion || !reg.Capabilities["response_stream_interceptor"] {
		t.Errorf("registration = %+v", reg)
	}
}
