package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/deriver/extract"
	"github.com/api-token/Cli-Proxy-API-Management-Center/plugins/clickhouse-sink/redact"
)

func testRedactor(t *testing.T, files ...string) *redact.Redactor {
	t.Helper()
	r, errNew := redact.New()
	if errNew != nil {
		t.Fatal(errNew)
	}
	r.Configure(files, nil)
	return r
}

// A secret the model streamed a few characters per delta is whole again in
// the reassembled reply, so the deriver must redact it.
func TestRedactFactsCatchesReassembledSecret(t *testing.T) {
	secret := "canary-secret-not-real-0123456789"
	list := filepath.Join(t.TempDir(), "secrets.txt")
	if errWrite := os.WriteFile(list, []byte(secret+"\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	r := testRedactor(t, list)
	f := extract.Facts{
		ReplyText:   "here it is: " + secret,
		ParseStatus: "ok",
		Messages: []extract.Message{
			{Text: "here it is: " + secret, TextHash: extract.HashText("here it is: " + secret)},
			{Arguments: `{"cmd":"echo ` + secret + `"}`},
		},
	}
	redactFacts(r, &f)
	all := f.ReplyText + f.Messages[0].Text + f.Messages[1].Arguments
	if strings.Contains(all, secret) {
		t.Fatalf("secret survived: %s", all)
	}
	if f.Messages[0].TextHash != extract.HashText(f.Messages[0].Text) {
		t.Error("text_hash not recomputed after redaction")
	}
	if f.ParseStatus != "ok" {
		t.Errorf("parse_status = %s", f.ParseStatus)
	}
}

func TestRedactFactsWithoutRedactorClearsText(t *testing.T) {
	f := extract.Facts{ReplyText: "x", ParseStatus: "ok", Messages: []extract.Message{{Text: "y", TextHash: 1}}}
	redactFacts(nil, &f)
	if f.ReplyText != "" || f.Messages[0].Text != "" || f.ParseStatus != statusRedactionFailed {
		t.Errorf("facts = %+v", f)
	}
}
