package main

import "github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/redact"

// redactRequest redacts every free-text field of a request row in place,
// including secrets split across stream chunks. On a panic, or without a
// redactor, the bodies are emptied and the count is -1, so a failure never
// stores an unredacted row.
func redactRequest(r *redact.Redactor, row *requestRow) {
	defer func() {
		if recover() != nil {
			failClosed(row)
		}
	}()
	if r == nil {
		failClosed(row)
		return
	}
	texts := []string{row.Error, row.ResponseBody}
	texts = append(texts, headerValues(row.RequestHeaders)...)
	texts = append(texts, headerValues(row.ResponseHeaders)...)
	texts = append(texts, row.ResponseChunks...)
	rep := r.NewRequestReplacer(row.RequestBody, texts)
	res := redact.NewResult()
	row.Error = rep.Apply(row.Error, res)
	row.RequestBody = rep.Apply(row.RequestBody, res)
	row.ResponseBody = rep.Apply(row.ResponseBody, res)
	for i := range row.ResponseChunks {
		row.ResponseChunks[i] = rep.Apply(row.ResponseChunks[i], res)
	}
	r.RedactSplitSecrets(row.ResponseChunks, res)
	redactHeaders(rep, row.RequestHeaders, res)
	redactHeaders(rep, row.ResponseHeaders, res)
	row.RedactionCount = res.Count
	row.RedactionRules = res.Rules()
}

func failClosed(row *requestRow) {
	row.RequestBody, row.ResponseBody, row.Error = "", "", ""
	row.ResponseChunks = nil
	row.RequestHeaders, row.ResponseHeaders = nil, nil
	row.RedactionCount, row.RedactionRules = -1, nil
}

// redactUsage redacts the free-text fields of a usage row in place.
func redactUsage(r *redact.Redactor, row *usageRow) {
	defer func() {
		if recover() != nil {
			row.FailureBody, row.ResponseHeaders = "", nil
		}
	}()
	if r == nil {
		row.FailureBody, row.ResponseHeaders = "", nil
		return
	}
	texts := append([]string{row.FailureBody}, headerValues(row.ResponseHeaders)...)
	rep := r.NewReplacer(texts)
	res := redact.NewResult()
	row.FailureBody = rep.Apply(row.FailureBody, res)
	redactHeaders(rep, row.ResponseHeaders, res)
}

func headerValues(h map[string]string) []string {
	out := make([]string, 0, len(h))
	for _, v := range h {
		out = append(out, v)
	}
	return out
}

func redactHeaders(rep *redact.Replacer, h map[string]string, res *redact.Result) {
	for k, v := range h {
		h[k] = rep.Apply(v, res)
	}
}
