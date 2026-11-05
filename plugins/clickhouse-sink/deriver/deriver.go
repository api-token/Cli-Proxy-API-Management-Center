package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/deriver/extract"
)

const epoch = "1970-01-01 00:00:00.000000000"

// deriver turns raw requests into request_facts rows.
type deriver struct {
	ch        *clickhouse
	db        string
	batchSize int
	log       *slog.Logger
	// extract is extract.Extract; tests replace it to inject a poison row.
	extract func(extract.Row, bool) extract.Facts
}

func newDeriver(ch *clickhouse, db string, log *slog.Logger) *deriver {
	return &deriver{ch: ch, db: db, batchSize: 50, log: log, extract: extract.Extract}
}

// setup creates the derived table and views.
func (d *deriver) setup(ctx context.Context) error {
	for _, stmt := range schemaFor(derivedSchema, d.db) {
		if err := d.ch.exec(ctx, stmt, nil); err != nil {
			return err
		}
	}
	return nil
}

// rebuild truncates request_facts and derives everything again.
func (d *deriver) rebuild(ctx context.Context) (int, error) {
	if err := d.ch.exec(ctx, "TRUNCATE TABLE "+d.db+".request_facts", nil); err != nil {
		return 0, fmt.Errorf("truncate: %w", err)
	}
	return d.pass(ctx)
}

// pass processes every raw row from the watermark on that has no facts row
// yet, in (ingested_at, request_id) order. The watermark is
// max(request_facts.ingested_at) - 1h, or the whole table when empty. A failed
// insert is not retried here: the pass stops, so the watermark cannot move past
// the failed rows, and the next pass's anti-join picks them up.
func (d *deriver) pass(ctx context.Context) (int, error) {
	start := time.Now()
	lower, err := d.watermark(ctx)
	if err != nil {
		return 0, fmt.Errorf("watermark: %w", err)
	}
	seen := map[string]bool{} // conversation_ids emitted in this pass
	cursorTS, cursorID := lower, ""
	total := 0
	statuses := map[string]int{}
	for {
		rows, err := d.fetch(ctx, lower, cursorTS, cursorID)
		if err != nil {
			return total, fmt.Errorf("fetch: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		convs := make([]string, len(rows))
		var lookup []string
		for i, r := range rows {
			convs[i] = extract.Conversation(r)
			if convs[i] != "" && !seen[convs[i]] {
				lookup = append(lookup, convs[i])
			}
		}
		known, err := d.knownConversations(ctx, lookup)
		if err != nil {
			return total, fmt.Errorf("prior lookup: %w", err)
		}
		facts := make([]any, 0, len(rows))
		for i, r := range rows {
			f := d.safeExtract(r, seen[convs[i]] || known[convs[i]])
			if f.ConversationID != "" {
				seen[f.ConversationID] = true
			}
			statuses[f.ParseStatus]++
			facts = append(facts, toFactsRow(f))
		}
		if err := d.ch.insertJSON(ctx, d.db+".request_facts", facts); err != nil {
			return total, fmt.Errorf("insert %d facts rows (first request_id %s): %w", len(facts), rows[0].RequestID, err)
		}
		total += len(rows)
		last := rows[len(rows)-1]
		cursorTS, cursorID = last.IngestedAt, last.RequestID
		if len(rows) < d.batchSize {
			break
		}
	}
	if total > 0 {
		d.log.Info("pass done", "rows", total, "statuses", statuses, "since", lower, "took", time.Since(start).Round(time.Millisecond))
	}
	return total, nil
}

// safeExtract is a second guard: extract.Extract recovers its own panics, but
// an injected or future extractor must not take the deriver down either.
func (d *deriver) safeExtract(r extract.Row, prior bool) (f extract.Facts) {
	defer func() {
		if p := recover(); p != nil {
			d.log.Warn("extractor panic, storing unparseable row", "request_id", r.RequestID)
			f = extract.Unparseable(r, fmt.Sprintf("panic: %v", p))
		}
	}()
	f = d.extract(r, prior)
	if f.ParseStatus == extract.StatusUnparseable {
		d.log.Warn("unparseable request", "request_id", r.RequestID, "reason", f.ParseError)
	}
	return f
}

func (d *deriver) watermark(ctx context.Context) (string, error) {
	type wm struct {
		Lower string `json:"lower"`
		Rows  uint64 `json:"rows"`
	}
	var got wm
	err := selectRows(ctx, d.ch, "SELECT max(ingested_at) - INTERVAL 1 HOUR AS lower, count() AS rows FROM "+d.db+".request_facts", nil,
		func(r wm) { got = r })
	if err != nil {
		return "", err
	}
	if got.Rows == 0 {
		return epoch, nil
	}
	return got.Lower, nil
}

func (d *deriver) fetch(ctx context.Context, lower, cursorTS, cursorID string) ([]extract.Row, error) {
	q := `SELECT request_id, trace_id, started_at, ingested_at,
    duration_ms, path, source_format, model, stream, outcome, status_code, error,
    request_headers, request_body, response_body, response_chunks, redaction_count
FROM ` + d.db + `.requests
WHERE ingested_at >= {lower:DateTime64(9, 'UTC')}
  AND (ingested_at, request_id) > ({cts:DateTime64(9, 'UTC')}, {cid:String})
  AND request_id NOT IN (
      SELECT request_id FROM ` + d.db + `.request_facts WHERE ingested_at >= {lower:DateTime64(9, 'UTC')})
ORDER BY ingested_at, request_id
LIMIT {limit:UInt32}`
	params := map[string]string{"lower": lower, "cts": cursorTS, "cid": cursorID, "limit": fmt.Sprint(d.batchSize)}
	var rows []extract.Row
	err := selectRows(ctx, d.ch, q, params, func(r extract.Row) { rows = append(rows, r) })
	return rows, err
}

func (d *deriver) knownConversations(ctx context.Context, ids []string) (map[string]bool, error) {
	known := map[string]bool{}
	if len(ids) == 0 {
		return known, nil
	}
	type conv struct {
		ID string `json:"conversation_id"`
	}
	err := selectRows(ctx, d.ch, "SELECT DISTINCT conversation_id FROM "+d.db+".request_facts WHERE conversation_id IN {ids:Array(String)}",
		map[string]string{"ids": chArray(ids)}, func(c conv) { known[c.ID] = true })
	return known, err
}

// factsRow is the JSONEachRow shape of request_facts.
type factsRow struct {
	RequestID            string   `json:"request_id"`
	StartedAt            string   `json:"started_at"`
	IngestedAt           string   `json:"ingested_at"`
	Model                string   `json:"model"`
	SourceFormat         string   `json:"source_format"`
	Outcome              string   `json:"outcome"`
	StatusCode           uint16   `json:"status_code"`
	Stream               bool     `json:"stream"`
	DurationMs           uint64   `json:"duration_ms"`
	Error                string   `json:"error"`
	Path                 string   `json:"path"`
	Kind                 string   `json:"kind"`
	RequestKind          string   `json:"request_kind"`
	ThreadSource         string   `json:"thread_source"`
	TurnID               string   `json:"turn_id"`
	Client               string   `json:"client"`
	ConversationID       string   `json:"conversation_id"`
	ConversationSource   string   `json:"conversation_source"`
	ParentConversationID string   `json:"parent_conversation_id"`
	ResponseID           string   `json:"response_id"`
	PreviousResponseID   string   `json:"previous_response_id"`
	SystemPromptHash     uint64   `json:"system_prompt_hash"`
	LastUserText         string   `json:"last_user_text"`
	ReplyText            string   `json:"reply_text"`
	ReasoningText        string   `json:"reasoning_text"`
	ToolsOffered         []string `json:"tools_offered"`
	ToolsCalled          []string `json:"tools_called"`
	StopReason           string   `json:"stop_reason"`
	PriorSeen            bool     `json:"prior_seen"`
	InputItems           uint32   `json:"input_items"`
	MsgDirection         []string `json:"msg_direction"`
	MsgPosition          []uint32 `json:"msg_position"`
	MsgRole              []string `json:"msg_role"`
	MsgType              []string `json:"msg_type"`
	MsgToolName          []string `json:"msg_tool_name"`
	MsgCallID            []string `json:"msg_call_id"`
	MsgArguments         []string `json:"msg_arguments"`
	MsgIsError           []bool   `json:"msg_is_error"`
	MsgText              []string `json:"msg_text"`
	MsgTextHash          []uint64 `json:"msg_text_hash"`
	ParseStatus          string   `json:"parse_status"`
	ParseError           string   `json:"parse_error"`
	ExtractorVersion     uint16   `json:"extractor_version"`
}

func toFactsRow(f extract.Facts) factsRow {
	r := factsRow{
		RequestID: f.RequestID, StartedAt: orEpoch(f.StartedAt), IngestedAt: orEpoch(f.IngestedAt),
		Model: f.Model, SourceFormat: f.SourceFormat, Outcome: f.Outcome, StatusCode: f.StatusCode,
		Stream: f.Stream, DurationMs: f.DurationMs, Error: f.Error, Path: f.Path, Kind: f.Kind,
		RequestKind: f.RequestKind, ThreadSource: f.ThreadSource, TurnID: f.TurnID, Client: f.Client,
		ConversationID: f.ConversationID, ConversationSource: f.ConversationSource,
		ParentConversationID: f.ParentConversationID, ResponseID: f.ResponseID,
		PreviousResponseID: f.PreviousResponseID, SystemPromptHash: f.SystemPromptHash,
		LastUserText: f.LastUserText, ReplyText: f.ReplyText, ReasoningText: f.ReasoningText,
		ToolsOffered: nonNil(f.ToolsOffered), ToolsCalled: nonNil(f.ToolsCalled), StopReason: f.StopReason,
		PriorSeen: f.PriorSeen, InputItems: f.InputItems, ParseStatus: f.ParseStatus,
		ParseError: f.ParseError, ExtractorVersion: f.ExtractorVersion,
	}
	n := len(f.Messages)
	r.MsgDirection, r.MsgRole, r.MsgType = make([]string, n), make([]string, n), make([]string, n)
	r.MsgToolName, r.MsgCallID, r.MsgArguments, r.MsgText = make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	r.MsgPosition, r.MsgIsError, r.MsgTextHash = make([]uint32, n), make([]bool, n), make([]uint64, n)
	for i, m := range f.Messages {
		r.MsgDirection[i], r.MsgPosition[i], r.MsgRole[i], r.MsgType[i] = m.Direction, m.Position, m.Role, m.Type
		r.MsgToolName[i], r.MsgCallID[i], r.MsgArguments[i] = m.ToolName, m.CallID, m.Arguments
		r.MsgIsError[i], r.MsgText[i], r.MsgTextHash[i] = m.IsError, m.Text, m.TextHash
	}
	return r
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEpoch(ts string) string {
	if ts == "" {
		return epoch
	}
	return ts
}
