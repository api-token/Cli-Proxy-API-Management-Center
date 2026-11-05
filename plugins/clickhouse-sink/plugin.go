package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "clickhouse-sink"
	pluginVersion = "0.1.0"
	// schemaVersion 7 is the oldest host contract that omits request bodies
	// on after-auth interception and on stream payload chunks; newer hosts
	// accept older plugin schemas.
	schemaVersion = 7

	defaultURL      = "http://clickhouse:8123"
	defaultDatabase = "cliproxy"
	defaultUser     = "default"
	passwordEnv     = "CLICKHOUSE_PASSWORD"
)

type pluginConfig struct {
	URL         string `yaml:"url"`
	Database    string `yaml:"database"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	StoreBodies *bool  `yaml:"store-bodies"`
}

func parseConfig(raw []byte) (sinkConfig, error) {
	var cfg pluginConfig
	if len(raw) > 0 {
		if errYAML := yaml.Unmarshal(raw, &cfg); errYAML != nil {
			return sinkConfig{}, fmt.Errorf("parse plugin config: %w", errYAML)
		}
	}
	out := sinkConfig{
		URL:         strings.TrimRight(strings.TrimSpace(cfg.URL), "/"),
		Database:    strings.TrimSpace(cfg.Database),
		Username:    strings.TrimSpace(cfg.Username),
		Password:    cfg.Password,
		StoreBodies: cfg.StoreBodies == nil || *cfg.StoreBodies,
	}
	if out.URL == "" {
		out.URL = defaultURL
	}
	if out.Database == "" {
		out.Database = defaultDatabase
	}
	if !validIdentifier(out.Database) {
		return sinkConfig{}, fmt.Errorf("database %q must contain only letters, digits and underscores", out.Database)
	}
	if out.Username == "" {
		out.Username = defaultUser
	}
	if out.Password == "" {
		out.Password = os.Getenv(passwordEnv)
	}
	return out, nil
}

func validIdentifier(s string) bool {
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

const registration = `{
  "schema_version": %d,
  "metadata": {
    "Name": %q,
    "Version": %q,
    "Author": "api-token",
    "GitHubRepository": "https://github.com/api-token/Cli-Proxy-API-Management-Center",
    "ConfigFields": [
      {"Name": "url", "Type": "string", "Default": %q, "Description": "ClickHouse HTTP endpoint."},
      {"Name": "database", "Type": "string", "Default": %q, "Description": "Database for the requests and usage tables. Created on startup."},
      {"Name": "username", "Type": "string", "Default": %q, "Description": "ClickHouse user."},
      {"Name": "password", "Type": "string", "Default": "", "Description": "ClickHouse password. Falls back to the CLICKHOUSE_PASSWORD environment variable."},
      {"Name": "store-bodies", "Type": "boolean", "Default": true, "Description": "Store request and response bodies and headers. When false only metadata is stored."}
    ]
  },
  "capabilities": {
    "request_interceptor": true,
    "request_interceptor_omit_body_after": true,
    "request_lifecycle_plugin": true,
    "response_interceptor": true,
    "response_stream_interceptor": true,
    "usage_plugin": true
  }
}`

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// plugin holds the process-wide state. The host keeps the library loaded for
// the life of the process, so this outlives any single call.
type plugin struct {
	mu      sync.Mutex
	sink    *sink
	capture *capture
}

var current = &plugin{capture: newCapture()}

func (p *plugin) currentSink() *sink {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sink
}

func (p *plugin) configure(raw []byte) error {
	cfg, errConfig := parseConfig(raw)
	if errConfig != nil {
		return errConfig
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink == nil {
		p.sink = newSink(cfg)
	} else {
		p.sink.reconfigure(cfg)
	}
	p.sink.start()
	p.capture.setStoreBodies(cfg.StoreBodies)
	return nil
}

// shutdown stops the flusher and writes anything pending. The host may keep
// the library mapped after shutdown, so no goroutine may outlive it.
func (p *plugin) shutdown() {
	if s := p.currentSink(); s != nil {
		s.stop()
	}
}

var emptyResult = json.RawMessage(`{}`)

// handle dispatches one host call and returns the JSON result. Interceptor
// hooks run on the request path, so they only copy what they need and
// return an empty result, which leaves the request and response unchanged.
func (p *plugin) handle(method string, request []byte) (json.RawMessage, error) {
	now := time.Now()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req lifecycleRequest
		if len(request) > 0 {
			if errDecode := json.Unmarshal(request, &req); errDecode != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", errDecode)
			}
		}
		if errConfigure := p.configure(req.ConfigYAML); errConfigure != nil {
			return nil, errConfigure
		}
		return json.RawMessage(fmt.Sprintf(registration, schemaVersion, pluginName, pluginVersion, defaultURL, defaultDatabase, defaultUser)), nil

	case "plugin.quiesce":
		if s := p.currentSink(); s != nil {
			s.flushNow()
		}
		return emptyResult, nil

	case "request.intercept_before":
		var req interceptRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, fmt.Errorf("decode intercept request: %w", errDecode)
		}
		p.capture.begin(req, now)
		return emptyResult, nil

	case "request.intercept_after":
		return emptyResult, nil

	case "response.intercept_after":
		var req responseRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, fmt.Errorf("decode response: %w", errDecode)
		}
		p.capture.response(req, now)
		return emptyResult, nil

	case "response.intercept_stream_chunk":
		var req streamChunkRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, fmt.Errorf("decode stream chunk: %w", errDecode)
		}
		p.capture.chunk(req, now)
		return emptyResult, nil

	case "request.complete":
		var req completion
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, fmt.Errorf("decode completion: %w", errDecode)
		}
		row := p.capture.complete(req)
		// Model listings and similar calls also complete; they are not inference.
		if s := p.currentSink(); s != nil && (row.Model != "" || row.RequestedModel != "") {
			s.add(tableRequests, row)
		}
		return emptyResult, nil

	case "usage.handle":
		var record usageRecord
		if errDecode := json.Unmarshal(request, &record); errDecode != nil {
			return nil, fmt.Errorf("decode usage record: %w", errDecode)
		}
		if s := p.currentSink(); s != nil {
			s.add(tableUsage, usageRowFrom(record, p.capture.storeBodies()))
		}
		return emptyResult, nil
	}
	return nil, fmt.Errorf("unknown method: %s", method)
}
