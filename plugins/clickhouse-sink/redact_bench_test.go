package main

import (
	"strings"
	"testing"
)

// BenchmarkRedactLargeRequest sizes redaction for a typical agent request
// (~400 KB of history) with a few hundred stream chunks.
func BenchmarkRedactLargeRequest(b *testing.B) {
	authDir, env := writeSecretFiles(&testing.T{})
	r := newRedactor()
	r.configure([]string{authDir, env}, nil)
	body := `{"input":[` + strings.Repeat(`{"type":"message","role":"user","content":"run the tests in ./pkg and fix the failing assertion, key=value config"},`, 4000) + `{}]}`
	chunks := make([]string, 300)
	for i := range chunks {
		chunks[i] = `data: {"type":"response.output_text.delta","delta":"some words "}`
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		row := requestRow{RequestBody: body, ResponseChunks: append([]string(nil), chunks...)}
		r.redactRequest(&row)
	}
}
