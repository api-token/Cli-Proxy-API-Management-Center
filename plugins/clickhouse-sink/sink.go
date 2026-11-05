package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed schema.sql
var schemaSQL string

const (
	tableRequests = "requests"
	tableUsage    = "usage"

	flushInterval = 5 * time.Second
	// flushBytes triggers an early flush once this much is buffered.
	flushBytes = 8 << 20
	// maxBufferedBytes bounds memory while ClickHouse is unreachable. New
	// rows are dropped past it, and the drop count is logged.
	maxBufferedBytes = 256 << 20
	// httpTimeout bounds one ClickHouse call so shutdown never hangs on it.
	httpTimeout = 30 * time.Second
)

type sinkConfig struct {
	URL         string
	Database    string
	Username    string
	Password    string
	StoreBodies bool
	// SecretFiles and DisabledRules configure redaction (see redact.go).
	SecretFiles   []string
	DisabledRules []string
}

func (c sinkConfig) equal(o sinkConfig) bool {
	return c.URL == o.URL && c.Database == o.Database && c.Username == o.Username &&
		c.Password == o.Password && c.StoreBodies == o.StoreBodies &&
		strings.Join(c.SecretFiles, "\x00") == strings.Join(o.SecretFiles, "\x00") &&
		strings.Join(c.DisabledRules, "\x00") == strings.Join(o.DisabledRules, "\x00")
}

// batch is an insert unit. Once cut, its rows and token never change, so a
// retry after an ambiguous failure is deduplicated by ClickHouse
// (insert_deduplication_token + non_replicated_deduplication_window).
type batch struct {
	table string
	rows  [][]byte
	token string
	size  int
}

func newBatch(table string, rows [][]byte) batch {
	h := sha256.New()
	size := 0
	for _, row := range rows {
		h.Write(row)
		h.Write([]byte{'\n'})
		size += len(row)
	}
	return batch{table: table, rows: rows, token: table + "-" + hex.EncodeToString(h.Sum(nil)), size: size}
}

// sink batches JSONEachRow rows per table and inserts them over HTTP.
type sink struct {
	mu        sync.Mutex
	cfg       sinkConfig
	pending   map[string][][]byte
	retry     []batch
	buffered  int
	dropped   int
	schemaOK  bool
	lastError string

	client *http.Client
	kick   chan struct{}
	quit   chan struct{}
	done   chan struct{}
	// flushMu serializes flushes from the loop, quiesce and shutdown.
	flushMu sync.Mutex
}

func newSink(cfg sinkConfig) *sink {
	return &sink{
		cfg:     cfg,
		pending: make(map[string][][]byte),
		client:  &http.Client{Timeout: httpTimeout},
		kick:    make(chan struct{}, 1),
	}
}

func (s *sink) reconfigure(cfg sinkConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.equal(cfg) {
		s.cfg = cfg
		s.schemaOK = false
	}
}

func (s *sink) start() {
	if s.quit != nil {
		return
	}
	s.quit = make(chan struct{})
	s.done = make(chan struct{})
	go s.loop()
}

func (s *sink) stop() {
	if s.quit == nil {
		return
	}
	close(s.quit)
	<-s.done
	s.quit = nil
	s.flushNow()
}

func (s *sink) loop() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.kick:
		case <-s.quit:
			return
		}
		s.flushNow()
	}
}

func (s *sink) add(table string, row any) {
	raw, errMarshal := json.Marshal(row)
	if errMarshal != nil {
		logf("encode %s row: %v", table, errMarshal)
		return
	}
	s.mu.Lock()
	if s.buffered+len(raw) > maxBufferedBytes {
		s.dropped++
		s.mu.Unlock()
		return
	}
	s.pending[table] = append(s.pending[table], raw)
	s.buffered += len(raw)
	full := s.buffered >= flushBytes
	s.mu.Unlock()
	if full {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// flushNow retries earlier batches unchanged, then cuts and inserts new ones.
func (s *sink) flushNow() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	cfg := s.cfg
	batches := s.retry
	s.retry = nil
	for table, rows := range s.pending {
		batches = append(batches, newBatch(table, rows))
	}
	s.pending = make(map[string][][]byte)
	dropped := s.dropped
	s.dropped = 0
	schemaOK := s.schemaOK
	s.mu.Unlock()

	if dropped > 0 {
		logf("buffer full, dropped %d rows", dropped)
	}
	if len(batches) == 0 {
		return
	}
	if !schemaOK {
		if errSchema := s.ensureSchema(cfg); errSchema != nil {
			s.keep(batches)
			s.report(errSchema)
			return
		}
		s.mu.Lock()
		if s.cfg.equal(cfg) {
			s.schemaOK = true
		}
		s.mu.Unlock()
	}
	for i, b := range batches {
		if errInsert := s.insertBatch(cfg, b); errInsert != nil {
			// Keep this batch and every later one; ClickHouse is likely down.
			s.keep(batches[i:])
			s.mu.Lock()
			s.schemaOK = false
			s.mu.Unlock()
			s.report(errInsert)
			return
		}
		s.release(b.size)
	}
	s.report(nil)
}

// insertBatch inserts b. When ClickHouse rejects the data itself (HTTP 400)
// it bisects, so only the unparsable rows are dropped. Any other error is
// returned with the remaining rows kept in b for an unchanged retry.
func (s *sink) insertBatch(cfg sinkConfig, b batch) error {
	errInsert := s.insert(cfg, b)
	if errInsert == nil || !isBadData(errInsert) {
		return errInsert
	}
	if len(b.rows) == 1 {
		logf("%v (row dropped)", errInsert)
		return nil
	}
	mid := len(b.rows) / 2
	for _, half := range [][][]byte{b.rows[:mid], b.rows[mid:]} {
		part := newBatch(b.table, half)
		if errPart := s.insertBatch(cfg, part); errPart != nil {
			return errPart
		}
	}
	return nil
}

// keep queues batches for an unchanged retry; they stay counted in buffered.
func (s *sink) keep(batches []batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retry = append(batches, s.retry...)
}

func (s *sink) release(size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffered -= size
	if s.buffered < 0 {
		s.buffered = 0
	}
}

// report logs an error once until it changes or a later flush succeeds.
func (s *sink) report(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		if s.lastError != "" {
			logf("ClickHouse writes recovered")
		}
		s.lastError = ""
		return
	}
	if msg := err.Error(); msg != s.lastError {
		s.lastError = msg
		logf("%s (rows kept for retry)", msg)
	}
}

func (s *sink) ensureSchema(cfg sinkConfig) error {
	for _, stmt := range strings.Split(schemaSQL, "\n;\n") {
		stmt = strings.TrimSpace(strings.ReplaceAll(stmt, "{db}", cfg.Database))
		if stmt == "" {
			continue
		}
		if errExec := s.exec(cfg, nil, stmt); errExec != nil {
			return fmt.Errorf("create schema: %w", errExec)
		}
	}
	return nil
}

func (s *sink) insert(cfg sinkConfig, b batch) error {
	params := url.Values{
		"query":                      {fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", cfg.Database, b.table)},
		"insert_deduplication_token": {b.token},
	}
	body := bytes.Join(b.rows, []byte{'\n'})
	if errExec := s.exec(cfg, params, string(body)); errExec != nil {
		return fmt.Errorf("insert %d rows into %s: %w", len(b.rows), b.table, errExec)
	}
	return nil
}

// exec posts body to ClickHouse with params in the URL.
func (s *sink) exec(cfg sinkConfig, params url.Values, body string) error {
	endpoint := cfg.URL + "/"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	req, errReq := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if errReq != nil {
		return errReq
	}
	req.Header.Set("X-ClickHouse-User", cfg.Username)
	if cfg.Password != "" {
		req.Header.Set("X-ClickHouse-Key", cfg.Password)
	}
	resp, errDo := s.client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return httpError{status: resp.StatusCode, msg: fmt.Sprintf("ClickHouse %s: %s", resp.Status, strings.TrimSpace(string(msg)))}
	}
	// ClickHouse can report an error after a 200 header; only a fully read
	// body counts as success.
	if _, errRead := io.Copy(io.Discard, resp.Body); errRead != nil {
		return fmt.Errorf("read ClickHouse response: %w", errRead)
	}
	return nil
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

// isBadData reports a ClickHouse rejection of the rows themselves (HTTP 400),
// as opposed to an outage, auth failure or missing table.
func isBadData(err error) bool {
	var he httpError
	return errors.As(err, &he) && he.status == http.StatusBadRequest
}

// logf writes to stderr, which lands in the proxy's container log.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] %s\n", pluginName, fmt.Sprintf(format, args...))
}
