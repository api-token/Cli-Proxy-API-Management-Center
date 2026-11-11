package redact

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	knownToken = "oauth-access-token-0123456789abcdef"
	envSecret  = "env-password-fedcba9876543210"
)

func mustNew(t *testing.T) *Redactor {
	t.Helper()
	r, errNew := New()
	if errNew != nil {
		t.Fatal(errNew)
	}
	return r
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

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

func TestSplitSecretOnlyReplacesCarryingChunks(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := mustNew(t)
	r.Configure([]string{authDir}, nil)
	chunks := []string{
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"x oauth-access-tok"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"en-0123456789abcdef"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":" fine"}}`,
	}
	res := NewResult()
	r.RedactSplitSecrets(chunks, res)
	want := Marker(RuleKnown, knownToken)
	if chunks[0] != "event: content_block_delta" || chunks[1] != want || chunks[2] != want || !strings.Contains(chunks[3], "fine") {
		t.Errorf("chunks = %q", chunks)
	}
	if res.Count != 2 || strings.Join(res.Rules(), ",") != RuleKnown {
		t.Errorf("count = %d rules = %v", res.Count, res.Rules())
	}
}

func TestSecretInsideOneChunkIsLeftToApply(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := mustNew(t)
	r.Configure([]string{authDir}, nil)
	chunks := []string{`data: {"delta":"` + knownToken + `"}`, `data: {"delta":"next"}`}
	r.RedactSplitSecrets(chunks, NewResult())
	if !strings.Contains(chunks[0], knownToken) {
		t.Errorf("single-chunk secret should be handled by Apply, got %q", chunks)
	}
}

func TestConcurrentRowsNeverSeeEmptySecretSet(t *testing.T) {
	authDir, env := writeSecretFiles(t)
	r := mustNew(t)
	r.Configure([]string{authDir, env}, nil)
	var wg sync.WaitGroup
	leaks := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			row := struct{ RequestBody string }{RequestBody: "x " + knownToken + " " + envSecret}
			row.RequestBody = r.NewReplacer([]string{row.RequestBody}).Apply(row.RequestBody, NewResult())
			if strings.Contains(row.RequestBody, knownToken) || strings.Contains(row.RequestBody, envSecret) {
				leaks <- row.RequestBody
			}
		}()
	}
	wg.Wait()
	close(leaks)
	for leak := range leaks {
		t.Errorf("secret survived concurrent redaction: %s", leak)
	}
}

func TestPromptCacheKeyIsNotRedacted(t *testing.T) {
	r := mustNew(t)
	r.Configure(nil, nil)
	body := `{"previous_response_id":null,"prompt_cache_key":"019a4c1e-7d2b-7f00-9c3e-5a1b2c3d4e5f"}`
	if len(r.detector.DetectString(body)) == 0 {
		t.Fatal("expected gitleaks to flag the identifier; the test no longer covers the allowlist")
	}
	row := struct{ RequestBody string }{RequestBody: body}
	row.RequestBody = r.NewReplacer([]string{row.RequestBody}).Apply(row.RequestBody, NewResult())
	if row.RequestBody != body {
		t.Errorf("identifier was redacted: %s", row.RequestBody)
	}
}

func TestRedactorPicksUpRotatedToken(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := mustNew(t)
	r.Configure([]string{authDir}, nil)
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

	row := struct{ RequestBody string }{RequestBody: "new " + rotated}
	row.RequestBody = r.NewReplacer([]string{row.RequestBody}).Apply(row.RequestBody, NewResult())
	if strings.Contains(row.RequestBody, rotated) {
		t.Errorf("rotated token survived: %s", row.RequestBody)
	}
}

func TestTruncatedKnownSecretIsRedacted(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := mustNew(t)
	r.Configure([]string{authDir}, nil)
	want := Marker(RuleKnown, knownToken)
	cases := map[string]string{
		// A reply cut off by max_tokens partway through the secret.
		"cut at end":     "the token is " + knownToken[:20],
		"cut, then text": "the token is " + knownToken[:25] + "... (truncated)",
		"whole":          "the token is " + knownToken,
		"base64 cut":     "b64 " + base64Prefix(knownToken, 24),
	}
	for name, text := range cases {
		res := NewResult()
		out := r.NewReplacer([]string{text}).Apply(text, res)
		if !strings.Contains(out, want) || strings.Contains(out, knownToken[:minTruncatedLen]) {
			t.Errorf("%s: got %q", name, out)
		}
	}
	short := "the token is " + knownToken[:minTruncatedLen-1]
	if out := r.NewReplacer([]string{short}).Apply(short, NewResult()); out != short {
		t.Errorf("prefix shorter than %d should be kept, got %q", minTruncatedLen, out)
	}
	// The cut copy is replaced whole, with no fragment of the secret left over.
	text := "x " + knownToken[:20] + " y"
	if out := r.NewReplacer([]string{text}).Apply(text, NewResult()); out != "x "+want+" y" {
		t.Errorf("got %q", out)
	}
}

func TestTruncatedSecretSplitAcrossChunks(t *testing.T) {
	authDir, _ := writeSecretFiles(t)
	r := mustNew(t)
	r.Configure([]string{authDir}, nil)
	chunks := []string{
		`data: {"delta":{"text":"x ` + knownToken[:10] + `"}}`,
		`data: {"delta":{"text":"` + knownToken[10:20] + `"}}`,
		`data: {"delta":{"stop_reason":"max_tokens"}}`,
	}
	r.RedactSplitSecrets(chunks, NewResult())
	want := Marker(RuleKnown, knownToken)
	if chunks[0] != want || chunks[1] != want {
		t.Errorf("chunks = %q", chunks)
	}
}

func base64Prefix(s string, n int) string {
	return base64.StdEncoding.EncodeToString([]byte(s))[:n]
}

func TestBodySegments(t *testing.T) {
	got := bodySegments(`{"model":"m","messages":[{"role":"user"},"x"],"stream":true}`)
	want := []string{`"model":"m"`, `{"role":"user"}`, `"x"`, `"stream":true`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("segments = %q, want %q", got, want)
	}
	if got := bodySegments("not json"); len(got) != 1 || got[0] != "not json" {
		t.Errorf("non-object body = %q", got)
	}
	if got := bodySegments(""); got != nil {
		t.Errorf("empty body = %q", got)
	}
}

func TestRequestReplacerCachesBodySegments(t *testing.T) {
	r := mustNew(t)
	r.Configure(nil, nil)
	// A key in the format gitleaks recognises (GitHub personal access token).
	const patternKey = "ghp_R2d2C3poBB8LukeLeiaHanSolo0123456789"
	turn1 := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"use ` + patternKey + `"}]}`
	turn2 := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"use ` + patternKey + `"},{"role":"user","content":"thanks"}]}`

	res := NewResult()
	out := r.NewRequestReplacer(turn1, nil).Apply(turn1, res)
	if strings.Contains(out, patternKey) || res.Count != 1 {
		t.Fatalf("turn 1 not redacted: count = %d body = %s", res.Count, out)
	}
	if len(r.segments) != 3 {
		t.Fatalf("cached segments after turn 1 = %d, want 3", len(r.segments))
	}

	// The resent message's finding comes from the cache and still redacts the
	// whole row, including a reply that repeats the key.
	reply := `data: {"delta":"` + patternKey + `"}`
	rep := r.NewRequestReplacer(turn2, []string{reply})
	res = NewResult()
	out = rep.Apply(turn2, res)
	reply = rep.Apply(reply, res)
	if strings.Contains(out, patternKey) || strings.Contains(reply, patternKey) || res.Count != 2 {
		t.Errorf("turn 2 not redacted: count = %d body = %s reply = %s", res.Count, out, reply)
	}
	if len(r.segments) != 4 {
		t.Errorf("cached segments after turn 2 = %d, want 4 (only the new message)", len(r.segments))
	}
}
