package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservedSampleClaudeWeekly(t *testing.T) {
	observedAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := observedAt.Add(3 * 24 * time.Hour)
	// Header names and values as the host captures them (quota_signals_test.go).
	sample, ok := observedSample(usageRecord{
		Provider:  "claude",
		AuthID:    "claude-a.json",
		AuthIndex: "idx-a",
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.9"},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.53"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       {fmt.Sprint(reset.Unix())},
		},
	}, observedAt)
	if !ok {
		t.Fatal("expected a sample")
	}
	if sample.Name != "claude-a.json" || sample.AuthIndex != "idx-a" || sample.Provider != "claude" {
		t.Fatalf("identity: %+v", sample)
	}
	if sample.WindowID != "seven-day" || sample.PeriodHours == nil || *sample.PeriodHours != 168 {
		t.Fatalf("window: %+v", sample)
	}
	if diff := sample.RemainingPercent - 47; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("remaining=%v want 47", sample.RemainingPercent)
	}
	if sample.ResetAt == nil || !sample.ResetAt.Equal(reset) {
		t.Fatalf("reset: %v", sample.ResetAt)
	}
}

func TestObservedSampleCodexPicksLongestWindow(t *testing.T) {
	observedAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	record := usageRecord{
		Provider: "codex",
		AuthID:   "codex-a.json",
		// Lower-case keys: raw upstream headers are not guaranteed canonical.
		ResponseHeaders: http.Header{
			"x-codex-primary-used-percent":          {"90"},
			"x-codex-primary-window-minutes":        {"300"},
			"x-codex-primary-reset-after-seconds":   {"600"},
			"x-codex-secondary-used-percent":        {"25"},
			"x-codex-secondary-window-minutes":      {"10080"},
			"x-codex-secondary-reset-after-seconds": {"86400"},
		},
	}
	sample, ok := observedSample(record, observedAt)
	if !ok || sample.WindowID != "weekly" || *sample.PeriodHours != 168 || sample.RemainingPercent != 75 {
		t.Fatalf("window: ok=%v %+v", ok, sample)
	}
	if sample.ResetAt == nil || !sample.ResetAt.Equal(observedAt.Add(24*time.Hour)) {
		t.Fatalf("reset: %v", sample.ResetAt)
	}

	// Free plans report the weekly window as primary only.
	record.ResponseHeaders = http.Header{
		"X-Codex-Primary-Used-Percent":   {"10"},
		"X-Codex-Primary-Window-Minutes": {"10080"},
		"X-Codex-Primary-Reset-At":       {fmt.Sprint(observedAt.Add(time.Hour).Unix())},
	}
	sample, ok = observedSample(record, observedAt)
	if !ok || sample.RemainingPercent != 90 || !sample.ResetAt.Equal(observedAt.Add(time.Hour)) {
		t.Fatalf("primary-only: ok=%v %+v", ok, sample)
	}
}

func TestObservedSampleSkipsUnusable(t *testing.T) {
	now := time.Now()
	for _, record := range []usageRecord{
		{},
		{Provider: "claude", AuthID: "a"},
		{Provider: "claude", AuthID: "a", ResponseHeaders: http.Header{"Retry-After": {"5"}}},
		{Provider: "claude", AuthID: "a", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"bad"}}},
		{Provider: "claude", ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.5"}}},
		{Provider: "gemini", AuthID: "a", ResponseHeaders: http.Header{"X-Codex-Primary-Used-Percent": {"5"}}},
	} {
		if _, ok := observedSample(record, now); ok {
			t.Fatalf("unexpected sample for %+v", record)
		}
	}
}

func TestHandleManagement(t *testing.T) {
	store := New("")
	now := time.Now().UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"claude-a.json","provider":"claude","remaining_percent":80,"period_hours":168,"observed_at":%q,"reset_at":%q},
		{"name":"codex-a.json","auth_index":"x1","provider":"codex","remaining_percent":120,"observed_at":%q},
		{"name":"","provider":"codex","remaining_percent":10},
		{"name":"missing-remaining.json","provider":"codex"}
	]}`, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339), now.Format(time.RFC3339))

	resp := handleManagement(store, managementRequest{Method: "POST", Body: []byte(body)})
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"accepted":2}` {
		t.Fatalf("post: %d %s", resp.StatusCode, resp.Body)
	}

	var payload struct {
		Samples        []Sample `json:"samples"`
		RetentionHours int      `json:"retention_hours"`
	}
	get := func(query string) managementResponse {
		t.Helper()
		values := map[string][]string{}
		for _, pair := range strings.Split(query, "&") {
			if key, value, found := strings.Cut(pair, "="); found {
				values[key] = []string{value}
			}
		}
		resp := handleManagement(store, managementRequest{Method: "GET", Query: values})
		payload.Samples = nil
		_ = json.Unmarshal(resp.Body, &payload)
		return resp
	}
	if resp = get(""); resp.StatusCode != http.StatusOK || len(payload.Samples) != 2 || payload.RetentionHours != 336 {
		t.Fatalf("get: %d %+v", resp.StatusCode, payload)
	}
	if payload.Samples[0].Source != SourceManagement || payload.Samples[1].RemainingPercent != 100 {
		t.Fatalf("samples: %+v", payload.Samples)
	}
	get("since=" + now.Add(-time.Hour).Format(time.RFC3339))
	if len(payload.Samples) != 1 || payload.Samples[0].AuthIndex != "x1" {
		t.Fatalf("since filter: %+v", payload.Samples)
	}
	get("provider=claude")
	if len(payload.Samples) != 1 || payload.Samples[0].Provider != "claude" {
		t.Fatalf("provider filter: %+v", payload.Samples)
	}
	if resp = get("since=yesterday"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad since: %d", resp.StatusCode)
	}
	if resp = handleManagement(store, managementRequest{Method: "POST", Body: []byte("{")}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: %d", resp.StatusCode)
	}
	many := `{"samples":[` + strings.TrimSuffix(strings.Repeat(`{"name":"a","provider":"codex","remaining_percent":1},`, 501), ",") + `]}`
	if resp = handleManagement(store, managementRequest{Method: "POST", Body: []byte(many)}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized batch: %d", resp.StatusCode)
	}
	if resp = handleManagement(store, managementRequest{Method: "DELETE"}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp = handleManagement(nil, managementRequest{Method: "GET"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("nil store: %d", resp.StatusCode)
	}
}

// TestPluginLifecycle drives the plugin through the same JSON calls the host
// makes: register, a usage record, management GET, shutdown, and a restart.
func TestPluginLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	p := &plugin{}
	call := func(method string, payload any) json.RawMessage {
		t.Helper()
		raw, _ := json.Marshal(payload)
		result, err := p.handle(method, raw)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return result
	}

	config := []byte("enabled: true\ndata-dir: " + dir + "\n")
	var registration struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct{ Name string }
		Capabilities  struct {
			UsagePlugin   bool `json:"usage_plugin"`
			ManagementAPI bool `json:"management_api"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(call("plugin.register", map[string]any{"config_yaml": config, "schema_version": 7}), &registration); err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != schemaVersion || registration.Metadata.Name != pluginName ||
		!registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
		t.Fatalf("registration: %+v", registration)
	}

	var routes struct {
		Routes []struct{ Method, Path string } `json:"routes"`
	}
	_ = json.Unmarshal(call("management.register", map[string]any{"BasePath": "/v0/management"}), &routes)
	if len(routes.Routes) != 2 || routes.Routes[0].Path != routePath || routes.Routes[1].Method != "POST" {
		t.Fatalf("routes: %+v", routes)
	}

	call("usage.handle", map[string]any{
		"Provider":  "codex",
		"AuthID":    "codex-a.json",
		"AuthIndex": "idx",
		"ResponseHeaders": map[string][]string{
			"X-Codex-Primary-Used-Percent":   {"30"},
			"X-Codex-Primary-Window-Minutes": {"10080"},
		},
	})

	var response struct {
		StatusCode int
		Body       []byte
	}
	_ = json.Unmarshal(call("management.handle", map[string]any{"Method": "GET", "Path": "/v0/management/quota-history"}), &response)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"remaining_percent":70`) {
		t.Fatalf("management GET: %d %s", response.StatusCode, response.Body)
	}

	// The host sends the body base64-encoded, as encoding/json does for []byte.
	post := `{"samples":[{"name":"claude-a.json","provider":"claude","remaining_percent":55}]}`
	_ = json.Unmarshal(call("management.handle", map[string]any{
		"Method": "POST",
		"Body":   base64.StdEncoding.EncodeToString([]byte(post)),
	}), &response)
	if string(response.Body) != `{"accepted":1}` {
		t.Fatalf("management POST: %s", response.Body)
	}

	p.shutdown()
	data, errRead := os.ReadFile(filepath.Join(dir, historyFileName))
	if errRead != nil || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("persisted file: %v %q", errRead, data)
	}

	// A restarted plugin reloads its history.
	restarted := &plugin{}
	if _, err := restarted.handle("plugin.register", mustJSON(map[string]any{"config_yaml": config})); err != nil {
		t.Fatal(err)
	}
	defer restarted.shutdown()
	if got := restarted.currentStore().Len(); got != 2 {
		t.Fatalf("reloaded samples: %d", got)
	}
	if _, err := restarted.handle("bogus.method", nil); err == nil {
		t.Fatal("unknown method should fail")
	}
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}
