package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// clickhouse is a minimal ClickHouse HTTP interface client.
type clickhouse struct {
	baseURL  string
	user     string
	password string
	http     *http.Client
}

func newClickHouse(baseURL, user, password string) *clickhouse {
	return &clickhouse{baseURL: strings.TrimRight(baseURL, "/") + "/", user: user, password: password, http: &http.Client{}}
}

// do runs one statement. With data != nil the statement goes into the URL and
// data is the request body (INSERT ... FORMAT JSONEachRow); otherwise the
// statement is the body. params become ClickHouse query parameters ({name:Type}).
func (c *clickhouse) do(ctx context.Context, query string, params map[string]string, data []byte) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("output_format_json_quote_64bit_integers", "0")
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	body := []byte(query)
	if data != nil {
		q.Set("query", query)
		body = data
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if errClose := resp.Body.Close(); errClose != nil {
			return nil, fmt.Errorf("clickhouse: HTTP %d (close: %v)", resp.StatusCode, errClose)
		}
		return nil, &chError{status: resp.StatusCode, msg: string(msg)}
	}
	return resp.Body, nil
}

// exec runs a statement and discards the result.
func (c *clickhouse) exec(ctx context.Context, query string, params map[string]string) error {
	rc, err := c.do(ctx, query, params, nil)
	if err != nil {
		return err
	}
	_, errCopy := io.Copy(io.Discard, rc)
	if errClose := rc.Close(); errCopy == nil {
		errCopy = errClose
	}
	return errCopy
}

// selectRows decodes a JSONEachRow result row by row into fn.
func selectRows[T any](ctx context.Context, c *clickhouse, query string, params map[string]string, fn func(T)) error {
	rc, err := c.do(ctx, query+"\nFORMAT JSONEachRow", params, nil)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	dec := json.NewDecoder(rc)
	for {
		var row T
		if err := dec.Decode(&row); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode row: %w", err)
		}
		fn(row)
	}
}

// insertJSON inserts rows with FORMAT JSONEachRow.
func (c *clickhouse) insertJSON(ctx context.Context, table string, rows []any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	rc, err := c.do(ctx, "INSERT INTO "+table+" FORMAT JSONEachRow", nil, buf.Bytes())
	if err != nil {
		return err
	}
	_, errCopy := io.Copy(io.Discard, rc)
	if errClose := rc.Close(); errCopy == nil {
		errCopy = errClose
	}
	return errCopy
}

type chError struct {
	status int
	msg    string
}

var (
	chCode = regexp.MustCompile(`Code: (\d+)`)
	chName = regexp.MustCompile(`\(([A-Z][A-Z0-9_]+)\) \(version`)
)

// Error keeps only the ClickHouse error code and name: parse errors quote the
// offending input, which can be body content, and that must not reach logs.
func (e *chError) Error() string {
	if m := chCode.FindStringSubmatch(e.msg); m != nil {
		name := ""
		if n := chName.FindStringSubmatch(e.msg); n != nil {
			name = " " + n[1]
		}
		return fmt.Sprintf("clickhouse: HTTP %d, code %s%s", e.status, m[1], name)
	}
	return fmt.Sprintf("clickhouse: HTTP %d", e.status)
}

// detail is the full (truncated) server message. Only for DDL errors, whose
// messages never contain row data.
func (e *chError) detail() string {
	msg := strings.TrimSpace(e.msg)
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

// chArray formats a ClickHouse Array(String) query parameter value.
func chArray(vals []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('\'')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v))
		b.WriteByte('\'')
	}
	b.WriteByte(']')
	return b.String()
}
