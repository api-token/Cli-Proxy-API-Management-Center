package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "quota-history"
	pluginVersion = "0.1.0"
	// schemaVersion 6 returns management bodies raw (no HTML escaping) and is
	// accepted by every host since; newer hosts accept older plugin schemas.
	schemaVersion = 6

	historyFileName = "quota-history.jsonl"
	// defaultDataDir is relative to the proxy's working directory.
	defaultDataDir = "plugin-data/quota-history"
	flushInterval  = 30 * time.Second
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	DataDir string `yaml:"data-dir"`
}

const registration = `{
  "schema_version": %d,
  "metadata": {
    "Name": %q,
    "Version": %q,
    "Author": "api-token",
    "GitHubRepository": "https://github.com/api-token/Cli-Proxy-API-Management-Center",
    "ConfigFields": [{
      "Name": "data-dir",
      "Type": "string",
      "Default": %q,
      "Description": "Directory for quota-history.jsonl. Relative paths resolve against the proxy's working directory."
    }]
  },
  "capabilities": {"usage_plugin": true, "management_api": true}
}`

const managementRoutes = `{"routes": [
  {"Method": "GET", "Path": %q},
  {"Method": "POST", "Path": %q}
]}`

// plugin holds the process-wide state. The host keeps the library loaded for
// the life of the process, so this outlives any single call.
type plugin struct {
	mu    sync.Mutex
	store *Store
	path  string
	stop  chan struct{}
	done  chan struct{}
}

var current = &plugin{}

func (p *plugin) currentStore() *Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store
}

// configure opens the store for the configured data directory, reopening it
// only when the directory changes, and starts the background flusher once.
func (p *plugin) configure(configYAML []byte) error {
	var cfg pluginConfig
	if len(configYAML) > 0 {
		if errYAML := yaml.Unmarshal(configYAML, &cfg); errYAML != nil {
			return fmt.Errorf("parse plugin config: %w", errYAML)
		}
	}
	dir := strings.TrimSpace(cfg.DataDir)
	if dir == "" {
		dir = defaultDataDir
	}
	absDir, errAbs := filepath.Abs(dir)
	if errAbs != nil {
		return fmt.Errorf("resolve data-dir: %w", errAbs)
	}
	if errMkdir := os.MkdirAll(absDir, 0o700); errMkdir != nil {
		return fmt.Errorf("create data-dir: %w", errMkdir)
	}
	path := filepath.Join(absDir, historyFileName)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store != nil && p.path == path {
		return nil
	}
	if p.store != nil {
		p.store.Flush()
	}
	p.store = New(path)
	p.path = path
	if p.stop == nil {
		p.stop = make(chan struct{})
		p.done = make(chan struct{})
		go p.flushLoop(p.stop, p.done)
	}
	return nil
}

func (p *plugin) flushLoop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.currentStore().Flush()
		case <-stop:
			return
		}
	}
}

// shutdown stops the flusher and writes anything pending. The host may keep
// the library mapped after shutdown, so no goroutine may outlive it.
func (p *plugin) shutdown() {
	p.mu.Lock()
	stop, done := p.stop, p.done
	p.stop, p.done = nil, nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	p.currentStore().Flush()
}

// handle dispatches one host call and returns the JSON result.
func (p *plugin) handle(method string, request []byte) (json.RawMessage, error) {
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
		return json.RawMessage(fmt.Sprintf(registration, schemaVersion, pluginName, pluginVersion, defaultDataDir)), nil

	case "plugin.quiesce":
		p.currentStore().Flush()
		return json.RawMessage(`{}`), nil

	case "usage.handle":
		var record usageRecord
		if errDecode := json.Unmarshal(request, &record); errDecode != nil {
			return nil, fmt.Errorf("decode usage record: %w", errDecode)
		}
		if sample, ok := observedSample(record, time.Now()); ok {
			p.currentStore().Add(SourceObserved, sample)
		}
		return json.RawMessage(`{}`), nil

	case "management.register":
		return json.RawMessage(fmt.Sprintf(managementRoutes, routePath, routePath)), nil

	case "management.handle":
		var req managementRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, fmt.Errorf("decode management request: %w", errDecode)
		}
		raw, errMarshal := json.Marshal(handleManagement(p.currentStore(), req))
		if errMarshal != nil {
			return nil, errMarshal
		}
		return raw, nil
	}
	return nil, fmt.Errorf("unknown method: %s", method)
}
