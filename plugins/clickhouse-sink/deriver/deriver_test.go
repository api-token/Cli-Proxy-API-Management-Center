package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/deriver/extract"
)

// Integration tests run against a real ClickHouse in a scratch database that
// is created from ../schema.sql and dropped afterwards. Enable with:
//
//	DERIVER_INTEGRATION=1 CLICKHOUSE_URL=http://127.0.0.1:8123 CLICKHOUSE_USER=... CLICKHOUSE_PASSWORD=... go test ./...
const testDB = "cliproxy_deriver_test"

func testDeriver(t *testing.T) *deriver {
	t.Helper()
	if os.Getenv("DERIVER_INTEGRATION") != "1" {
		t.Skip("set DERIVER_INTEGRATION=1 to run against ClickHouse")
	}
	ctx := context.Background()
	ch := newClickHouse(env("CLICKHOUSE_URL", "http://127.0.0.1:8123"), os.Getenv("CLICKHOUSE_USER"), os.Getenv("CLICKHOUSE_PASSWORD"))
	if err := ch.exec(ctx, "DROP DATABASE IF EXISTS "+testDB+" SYNC", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ch.exec(context.Background(), "DROP DATABASE IF EXISTS "+testDB+" SYNC", nil); err != nil {
			t.Logf("drop %s: %v", testDB, err)
		}
	})
	raw, err := os.ReadFile(filepath.Join("..", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(strings.ReplaceAll(string(raw), "{db}", testDB), "\n;\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if err := ch.exec(ctx, stmt, nil); err != nil {
			t.Fatalf("raw schema: %v: %s", err, err.(*chError).detail())
		}
	}
	d := newDeriver(ch, testDB, slog.New(slog.NewTextHandler(io.Discard, nil)), testRedactor(t))
	if err := d.setup(ctx); err != nil {
		var detail string
		if e, ok := err.(*chError); ok {
			detail = e.detail()
		}
		t.Fatalf("setup: %v %s", err, detail)
	}
	// setup is idempotent
	if err := d.setup(ctx); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	return d
}

type rawRow struct {
	extract.Row
	CompletedAt string `json:"completed_at"`
}

// fixtureRow loads an extract fixture as a raw row with a new ID and ingest time.
func fixtureRow(t *testing.T, name, id string, ingested time.Time) rawRow {
	t.Helper()
	dir := filepath.Join("extract", "testdata", name)
	var r rawRow
	data, err := os.ReadFile(filepath.Join(dir, "row.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &r.Row); err != nil {
		t.Fatal(err)
	}
	req, _ := os.ReadFile(filepath.Join(dir, "request.json"))
	r.RequestBody = string(req)
	if chunks, err := os.ReadFile(filepath.Join(dir, "chunks.sse")); err == nil {
		r.ResponseChunks = strings.Split(strings.TrimRight(string(chunks), "\n"), "\n%%\n")
	}
	if resp, err := os.ReadFile(filepath.Join(dir, "response.json")); err == nil {
		r.ResponseBody = string(resp)
	}
	r.RequestID = id
	r.TraceID = "trace-" + id
	r.StartedAt = ingested.Add(-2 * time.Second).UTC().Format("2006-01-02 15:04:05.000000000")
	r.CompletedAt = ingested.Add(-time.Second).UTC().Format("2006-01-02 15:04:05.000000000")
	r.IngestedAt = ingested.UTC().Format("2006-01-02 15:04:05.000000000")
	return r
}

// rawJSON drops duration_ms (MATERIALIZED, cannot be inserted).
func rawJSON(r rawRow) map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "duration_ms")
	return m
}

func insertRaw(t *testing.T, d *deriver, rows ...rawRow) {
	t.Helper()
	batch := make([]any, len(rows))
	for i, r := range rows {
		batch[i] = rawJSON(r)
	}
	if err := d.ch.insertJSON(context.Background(), d.db+".requests", batch); err != nil {
		t.Fatalf("insert raw: %v", err)
	}
}

func scalar(t *testing.T, d *deriver, q string) string {
	t.Helper()
	rc, err := d.ch.do(context.Background(), q, nil, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return strings.TrimSpace(string(b))
}

func fixtureSet(t *testing.T, base time.Time) []rawRow {
	names := []string{"chat_stream", "responses_http", "responses_ws", "claude_code_stream", "gemini_stream", "responses_canceled", "codex_memory", "claude_prefill"}
	var rows []rawRow
	for i, n := range names {
		rows = append(rows, fixtureRow(t, n, fmt.Sprintf("req-%02d-%s", i, n), base.Add(time.Duration(i)*time.Second)))
	}
	// A second turn of responses_http's conversation, ingested last.
	rows = append(rows, fixtureRow(t, "responses_http", "req-99-second-turn", base.Add(time.Minute)))
	return rows
}

func TestCrashBetweenPassesReprocessesNothing(t *testing.T) {
	d := testDeriver(t)
	rows := fixtureSet(t, time.Now().Add(-10*time.Minute))
	insertRaw(t, d, rows...)

	// First pass "crashes" after its first batch: the context dies while the
	// second batch is being extracted, so only batch one is committed.
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	d.batchSize = 3
	d.extract = func(r extract.Row, prior bool) extract.Facts {
		calls++
		if calls == 4 {
			cancel()
		}
		return extract.Extract(r, prior)
	}
	if _, err := d.pass(ctx); err == nil {
		t.Fatal("interrupted pass should report an error")
	}
	if got := scalar(t, d, "SELECT count() FROM "+testDB+".request_facts"); got != "3" {
		t.Fatalf("facts after crash = %s, want 3", got)
	}

	// Restart: a fresh process (no in-memory state) finishes the rest.
	d2 := newDeriver(d.ch, testDB, d.log, d.redactor)
	n, err := d2.pass(context.Background())
	if err != nil || n != len(rows)-3 {
		t.Fatalf("resume pass = %d, %v; want %d", n, err, len(rows)-3)
	}
	n, err = newDeriver(d.ch, testDB, d.log, d.redactor).pass(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("idle pass = %d, %v; want 0", n, err)
	}
	if got := scalar(t, d, "SELECT count(), uniqExact(request_id) FROM "+testDB+".request_facts"); got != fmt.Sprintf("%d\t%d", len(rows), len(rows)) {
		t.Fatalf("count, uniq = %s", got)
	}
	// priorSeen crossed the restart: the conversation's first turn was in the DB.
	if got := scalar(t, d, "SELECT prior_seen, length(msg_text) FROM "+testDB+".request_facts WHERE request_id = 'req-99-second-turn'"); got != "true\t6" {
		// Its input has no model item, so the tail rule still emits all 3 input items.
		t.Fatalf("second turn prior_seen, messages = %q, want true and 6", got)
	}
	if got := scalar(t, d, "SELECT countIf(f.ingested_at != r.ingested_at) FROM "+testDB+".request_facts f JOIN "+testDB+".requests r USING request_id"); got != "0" {
		t.Fatalf("ingested_at not copied for %s rows", got)
	}
}

func TestPoisonRowDoesNotBlock(t *testing.T) {
	d := testDeriver(t)
	base := time.Now().Add(-5 * time.Minute)
	good := fixtureRow(t, "chat_stream", "req-good", base)
	poison := fixtureRow(t, "claude_prefill", "req-poison", base.Add(time.Second))
	garbage := fixtureRow(t, "chat_stream", "req-garbage", base.Add(2*time.Second))
	garbage.RequestBody = "{not json"
	garbage.ResponseChunks = []string{"\x00\xff", "data: {broken"}
	insertRaw(t, d, good, poison, garbage)

	d.extract = func(r extract.Row, prior bool) extract.Facts {
		if r.RequestID == "req-poison" {
			panic("injected")
		}
		return extract.Extract(r, prior)
	}
	n, err := d.pass(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("pass = %d, %v; want 3", n, err)
	}
	got := scalar(t, d, "SELECT request_id, parse_status FROM "+testDB+".request_facts ORDER BY request_id FORMAT TSV")
	want := "req-garbage\tunparseable\nreq-good\tok\nreq-poison\tunparseable"
	if got != want {
		t.Fatalf("statuses:\n%s\nwant:\n%s", got, want)
	}
	if n, err := d.pass(context.Background()); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v; poison row must not be retried", n, err)
	}
}

func TestRebuildIsIdempotent(t *testing.T) {
	d := testDeriver(t)
	rows := fixtureSet(t, time.Now().Add(-3*time.Hour))
	insertRaw(t, d, rows...)
	d.batchSize = 2
	if _, err := d.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		return scalar(t, d, "SELECT * FROM "+testDB+".request_facts ORDER BY request_id FORMAT JSONEachRow")
	}
	first := snapshot()
	for i := 0; i < 2; i++ {
		n, err := d.rebuild(context.Background())
		if err != nil || n != len(rows) {
			t.Fatalf("rebuild %d = %d, %v", i, n, err)
		}
		if got := snapshot(); got != first {
			t.Fatalf("rebuild %d changed request_facts", i)
		}
	}
}

func TestViewsAndComments(t *testing.T) {
	d := testDeriver(t)
	base := time.Now().Add(-time.Minute)
	r := fixtureRow(t, "claude_code_stream", "req-sum", base)
	insertRaw(t, d, r)
	attempt := func(id, provider string, offset time.Duration, in, out int, failed bool) map[string]any {
		return map[string]any{
			"request_id": id, "trace_id": r.TraceID, "provider": provider, "auth_id": "auth-" + provider,
			"requested_at": base.Add(-2*time.Second + offset).UTC().Format("2006-01-02 15:04:05.000000000"),
			"session_id":   "cpa-session-1", "input_tokens": in, "output_tokens": out, "total_tokens": in + out, "failed": failed,
		}
	}
	if err := d.ch.insertJSON(context.Background(), testDB+".usage", []any{
		attempt("u1", "first", 100*time.Millisecond, 10, 0, true),
		attempt("u2", "second", 200*time.Millisecond, 10, 5, false),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := scalar(t, d, "SELECT attempts, failed_attempts, input_tokens, output_tokens, final_provider, cpa_session_id, conversation_id FROM "+testDB+".request_summary FORMAT TSV")
	if want := "2\t1\t20\t5\tsecond\tcpa-session-1\tclaude:cc-sess-0001"; got != want {
		t.Fatalf("request_summary = %q, want %q", got, want)
	}
	got = scalar(t, d, "SELECT groupArray(concat(direction, ':', role, '/', type)) FROM (SELECT * FROM "+testDB+".messages ORDER BY direction, position)")
	if !strings.Contains(got, "input:tool/tool_result") || !strings.Contains(got, "output:assistant/tool_use") {
		t.Fatalf("messages view = %s", got)
	}
	if got := scalar(t, d, "SELECT count() FROM system.columns WHERE database = '"+testDB+"' AND table = 'request_facts' AND comment = ''"); got != "0" {
		t.Fatalf("%s request_facts columns lack a COMMENT", got)
	}
	if got := scalar(t, d, "SELECT countIf(comment = '') FROM system.tables WHERE database = '"+testDB+"' AND name IN ('request_facts', 'messages', 'request_summary')"); got != "0" {
		t.Fatalf("%s derived tables/views lack a COMMENT", got)
	}
}

func TestErrorMessagesDropRowData(t *testing.T) {
	e := &chError{status: 400, msg: "Code: 27. DB::Exception: Cannot parse input: expected '\"' before: 'secret prompt text': (at row 1) (CANNOT_PARSE_INPUT_ASSERTION_FAILED) (version 25.8.33.6 (official build))"}
	if got := e.Error(); got != "clickhouse: HTTP 400, code 27 CANNOT_PARSE_INPUT_ASSERTION_FAILED" {
		t.Fatalf("Error() = %q", got)
	}
	if got := chArray([]string{"a'b", `c\d`}); got != `['a\'b','c\\d']` {
		t.Fatalf("chArray = %s", got)
	}
}
