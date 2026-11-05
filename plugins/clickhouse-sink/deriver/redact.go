package main

import (
	"github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/deriver/extract"
	"github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/redact"
)

// statusRedactionFailed marks facts whose text was dropped because the
// redaction pass failed.
const statusRedactionFailed = "redaction_failed"

// redactFacts runs secret redaction over the text the extractor produced.
// The plugin already redacted the raw row, but reassembling a stream can join
// a secret that was split across chunks, so derived text is checked again.
// On failure every text field is cleared rather than stored unredacted.
func redactFacts(r *redact.Redactor, f *extract.Facts) {
	defer func() {
		if recover() != nil {
			clearText(f)
		}
	}()
	if r == nil {
		clearText(f)
		return
	}
	texts := []string{f.LastUserText, f.ReplyText, f.ReasoningText, f.Error, f.ParseError}
	for _, m := range f.Messages {
		texts = append(texts, m.Text, m.Arguments)
	}
	rep := r.NewReplacer(texts)
	res := redact.NewResult()
	f.LastUserText = rep.Apply(f.LastUserText, res)
	f.ReplyText = rep.Apply(f.ReplyText, res)
	f.ReasoningText = rep.Apply(f.ReasoningText, res)
	f.Error = rep.Apply(f.Error, res)
	f.ParseError = rep.Apply(f.ParseError, res)
	for i := range f.Messages {
		m := &f.Messages[i]
		m.Arguments = rep.Apply(m.Arguments, res)
		if text := rep.Apply(m.Text, res); text != m.Text {
			m.Text = text
			m.TextHash = extract.HashText(text)
		}
	}
}

func clearText(f *extract.Facts) {
	f.LastUserText, f.ReplyText, f.ReasoningText, f.Error, f.ParseError = "", "", "", "", ""
	for i := range f.Messages {
		f.Messages[i].Text, f.Messages[i].Arguments, f.Messages[i].TextHash = "", "", 0
	}
	f.ParseStatus = statusRedactionFailed
}
