package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestStreamCaptureKeepsChunksExactly(t *testing.T) {
	p := &plugin{capture: newCapture()}
	start := time.Date(2026, 11, 4, 12, 0, 0, 123456789, time.UTC)
	call(t, p, "request.intercept_before", map[string]any{
		"RequestID": "r1", "TraceID": "abcd1234",
		"Headers":  http.Header{"Authorization": {"Bearer secret"}, "User-Agent": {"cli"}},
		"Body":     []byte(`{"model":"m","stream":true}`),
		"Metadata": map[string]any{"request_path": "/v1/responses"},
	})
	call(t, p, "response.intercept_stream_chunk", map[string]any{
		"RequestID": "r1", "ChunkIndex": -1, "ResponseHeaders": http.Header{"Content-Type": {"text/event-stream"}},
	})
	// Codex sends unframed SSE lines; a JSON string may also be split.
	for i, chunk := range []string{"event: response.output_text.delta", `data: {"delta":"he`, `llo"}`} {
		call(t, p, "response.intercept_stream_chunk", map[string]any{"RequestID": "r1", "ChunkIndex": i, "Body": []byte(chunk)})
	}

	row := p.capture.complete(completion{
		RequestID: "r1", TraceID: "abcd1234", Outcome: "succeeded", StatusCode: 200,
		Stream: true, StartedAt: start, CompletedAt: start.Add(1500 * time.Millisecond),
	})
	if row.RequestBody != `{"model":"m","stream":true}` || row.Path != "/v1/responses" {
		t.Errorf("request body = %q path = %q", row.RequestBody, row.Path)
	}
	want := []string{"event: response.output_text.delta", `data: {"delta":"he`, `llo"}`}
	if strings.Join(row.ResponseChunks, "|") != strings.Join(want, "|") || row.ResponseBody != "" {
		t.Errorf("chunks = %q body = %q", row.ResponseChunks, row.ResponseBody)
	}
	if _, ok := row.RequestHeaders["authorization"]; ok {
		t.Error("authorization header was stored")
	}
	if row.RequestHeaders["user-agent"] != "cli" || row.ResponseHeaders["content-type"] != "text/event-stream" {
		t.Errorf("headers = %v / %v", row.RequestHeaders, row.ResponseHeaders)
	}
	if row.StartedAt != "2026-11-04 12:00:00.123456789" || row.CompletedAt != "2026-11-04 12:00:01.623456789" {
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

// --- redaction ---

const (
	knownToken = "oauth-access-token-0123456789abcdef"
	envSecret  = "env-password-fedcba9876543210"
	// A key in the format gitleaks recognises (GitHub personal access token).
	patternKey = "ghp_R2d2C3poBB8LukeLeiaHanSolo0123456789"
)

func writeSecretFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if errMkdir := os.Mkdir(authDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	writeFile(t, filepath.Join(authDir, "codex-me.json"), `{"email":"me@example.com","token":{"access_token":"`+knownToken+`","expiry":"2026"}}`)
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "PORT=8317\nCLICKHOUSE_PASSWORD="+envSecret+"\n")
	return authDir, env
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func TestRedactRequestRemovesKnownAndPatternSecrets(t *testing.T) {
	authDir, env := writeSecretFiles(t)
	r := newRedactor()
	r.configure([]string{authDir, env}, nil)

	b64 := base64.StdEncoding.EncodeToString([]byte(envSecret))
	row := requestRow{
		RequestBody:     `{"input":"token ` + knownToken + ` and ` + b64 + ` port 8317 me@example.com"}`,
		ResponseChunks:  []string{`data: {"delta":"` + envSecret + `"}`, "data: [DONE]"},
		RequestHeaders:  map[string]string{"x-custom": knownToken},
		ResponseBody:    "",
		Error:           "upstream said GITHUB_TOKEN=" + patternKey,
		ResponseHeaders: map[string]string{},
	}
	r.redactRequest(&row)

	all := row.RequestBody + strings.Join(row.ResponseChunks, "") + row.RequestHeaders["x-custom"] + row.Error
	for _, secret := range []string{knownToken, envSecret, b64, patternKey} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q survived: %s", secret, all)
		}
	}
	if !strings.Contains(row.RequestBody, "port 8317") || !strings.Contains(row.RequestBody, "me@example.com") {
		t.Errorf("non-secret content was removed: %s", row.RequestBody)
	}
	if want := marker(ruleKnown, knownToken); !strings.Contains(row.RequestBody, want) || row.RequestHeaders["x-custom"] != want {
		t.Errorf("known marker %s missing: %s / %v", want, row.RequestBody, row.RequestHeaders)
	}
	if !json.Valid([]byte(row.RequestBody)) {
		t.Errorf("redacted body is not valid JSON: %s", row.RequestBody)
	}
	if row.RedactionCount < 5 {
		t.Errorf("redaction_count = %d", row.RedactionCount)
	}
	rules := strings.Join(row.RedactionRules, ",")
	if !strings.Contains(rules, ruleKnown) || len(row.RedactionRules) < 2 {
		t.Errorf("redaction_rules = %v", row.RedactionRules)
	}
}

func TestRedactorPicksUpRotatedToken(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := newRedactor()
	r.configure([]string{authDir}, nil)
	r.reloadIfStale(time.Now())

	rotated := "rotated-access-token-aaaabbbbccccdddd"
	path := filepath.Join(authDir, "codex-me.json")
	writeFile(t, path, `{"token":{"access_token":"`+rotated+`"}}`)
	future := time.Now().Add(time.Minute)
	if errTouch := os.Chtimes(path, future, future); errTouch != nil {
		t.Fatal(errTouch)
	}
	r.mu.Lock()
	r.lastCheck = time.Time{} // let the next row re-check the files
	r.mu.Unlock()

	row := requestRow{RequestBody: "new " + rotated}
	r.redactRequest(&row)
	if strings.Contains(row.RequestBody, rotated) {
		t.Errorf("rotated token survived: %s", row.RequestBody)
	}
}

func TestRedactionFailureEmptiesBodies(t *testing.T) {
	var r *redactor // a nil redactor panics inside; the row must fail closed
	row := requestRow{RequestBody: knownToken, ResponseChunks: []string{knownToken}, Error: knownToken}
	r.redactRequest(&row)
	if row.RequestBody != "" || row.ResponseChunks != nil || row.Error != "" || row.RedactionCount != -1 {
		t.Errorf("row = %+v", row)
	}
}

func TestRedactUsageFailureBody(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := newRedactor()
	r.configure([]string{authDir}, nil)
	row := usageRow{FailureBody: `{"error":"bad token ` + knownToken + `"}`, ResponseHeaders: map[string]string{"x-echo": knownToken}}
	r.redactUsage(&row)
	if strings.Contains(row.FailureBody+row.ResponseHeaders["x-echo"], knownToken) {
		t.Errorf("usage row kept secret: %+v", row)
	}
}

// --- sink ---

// fakeClickHouse mimics the parts of ClickHouse the sink relies on: insert
// deduplication by token, rejection of unparsable rows, and outages.
type fakeClickHouse struct {
	mu       sync.Mutex
	fail     int  // status for every insert while non-zero
	loseAck  bool // commit the insert, then fail the response once
	queries  []string
	inserts  map[string][]string
	seenTokn map[string]bool
}

func newFake() *fakeClickHouse {
	return &fakeClickHouse{inserts: map[string][]string{}, seenTokn: map[string]bool{}}
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
	if strings.Contains(string(body), "bad") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	token := r.URL.Query().Get("insert_deduplication_token")
	if !f.seenTokn[token] {
		f.seenTokn[token] = true
		table := strings.Fields(query)[2]
		f.inserts[table] = append(f.inserts[table], strings.Split(string(body), "\n")...)
	}
	if f.loseAck {
		f.loseAck = false
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func TestSinkCreatesSchemaInsertsAndRetries(t *testing.T) {
	fake := newFake()
	fake.fail = http.StatusServiceUnavailable
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
	if !strings.Contains(strings.Join(fake.queries, "\n"), "CREATE TABLE IF NOT EXISTS cliproxy.requests") {
		t.Fatalf("schema not created: %v", fake.queries)
	}
	if s.buffered != 0 {
		t.Errorf("buffered = %d after success", s.buffered)
	}
}

// An insert that committed but whose response was lost must not duplicate
// rows, even when new rows arrive before the retry.
func TestSinkLostAckRetryDoesNotDuplicate(t *testing.T) {
	fake := newFake()
	fake.loseAck = true
	server := httptest.NewServer(fake)
	defer server.Close()

	s := newSink(sinkConfig{URL: server.URL, Database: "cliproxy"})
	s.add(tableUsage, map[string]int{"a": 1})
	s.flushNow() // committed, response lost
	s.add(tableUsage, map[string]int{"a": 2})
	s.flushNow() // retries the first batch unchanged, then the new one

	got := fake.inserts["cliproxy.usage"]
	if strings.Join(got, ",") != `{"a":1},{"a":2}` {
		t.Errorf("inserted = %v", got)
	}
	if s.buffered != 0 || len(s.retry) != 0 {
		t.Errorf("buffered = %d retry = %d", s.buffered, len(s.retry))
	}
}

func TestSinkBisectsAndDropsOnlyBadRows(t *testing.T) {
	fake := newFake()
	server := httptest.NewServer(fake)
	defer server.Close()

	s := newSink(sinkConfig{URL: server.URL, Database: "cliproxy"})
	for _, v := range []string{"ok1", "ok2", "bad", "ok3", "ok4"} {
		s.add(tableUsage, map[string]string{"v": v})
	}
	s.flushNow()
	got := strings.Join(fake.inserts["cliproxy.usage"], ",")
	for _, v := range []string{"ok1", "ok2", "ok3", "ok4"} {
		if !strings.Contains(got, v) {
			t.Errorf("good row %s dropped: %s", v, got)
		}
	}
	if strings.Contains(got, "bad") || s.buffered != 0 {
		t.Errorf("inserted = %s buffered = %d", got, s.buffered)
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
	if len(cfg.SecretFiles) != 1 || cfg.SecretFiles[0] != "config.yaml" {
		t.Errorf("default redact files = %v", cfg.SecretFiles)
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
