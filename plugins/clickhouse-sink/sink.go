package main

import (
	"bytes"
	_ "embed"
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
	// maxBufferedBytes bounds memory while ClickHouse is unreachable. Newer
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
}

// sink batches JSONEachRow rows per table and inserts them over HTTP.
type sink struct {
	mu        sync.Mutex
	cfg       sinkConfig
	rows      map[string][][]byte
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
		cfg:    cfg,
		rows:   make(map[string][][]byte),
		client: &http.Client{Timeout: httpTimeout},
		kick:   make(chan struct{}, 1),
	}
}

func (s *sink) reconfigure(cfg sinkConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != cfg {
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
	s.rows[table] = append(s.rows[table], raw)
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

// flushNow inserts everything buffered. Rows from a failed insert go back
// to the front of the buffer and are retried on the next flush.
func (s *sink) flushNow() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	cfg := s.cfg
	batches := s.rows
	s.rows = make(map[string][][]byte)
	s.buffered = 0
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
			s.requeue(batches)
			s.report(errSchema)
			return
		}
		s.mu.Lock()
		if s.cfg == cfg {
			s.schemaOK = true
		}
		s.mu.Unlock()
	}
	for table, rows := range batches {
		if errInsert := s.insert(cfg, table, rows); errInsert != nil {
			if isBadData(errInsert) {
				// Retrying rows ClickHouse cannot parse would stall every later row.
				logf("%v (dropped)", errInsert)
				continue
			}
			s.requeue(map[string][][]byte{table: rows})
			s.mu.Lock()
			s.schemaOK = false
			s.mu.Unlock()
			s.report(errInsert)
			continue
		}
		s.report(nil)
	}
}

func (s *sink) requeue(batches map[string][][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for table, rows := range batches {
		size := 0
		for _, row := range rows {
			size += len(row)
		}
		if s.buffered+size > maxBufferedBytes {
			s.dropped += len(rows)
			continue
		}
		s.rows[table] = append(rows, s.rows[table]...)
		s.buffered += size
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
		if errExec := s.exec(cfg, "", stmt); errExec != nil {
			return fmt.Errorf("create schema: %w", errExec)
		}
	}
	return nil
}

func (s *sink) insert(cfg sinkConfig, table string, rows [][]byte) error {
	query := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", cfg.Database, table)
	body := bytes.Join(rows, []byte{'\n'})
	if errExec := s.exec(cfg, query, string(body)); errExec != nil {
		return fmt.Errorf("insert %d rows into %s: %w", len(rows), table, errExec)
	}
	return nil
}

// exec posts body to ClickHouse, with query in the URL when set.
func (s *sink) exec(cfg sinkConfig, query, body string) error {
	endpoint := cfg.URL + "/"
	if query != "" {
		endpoint += "?" + url.Values{"query": {query}}.Encode()
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
	_, _ = io.Copy(io.Discard, resp.Body)
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
