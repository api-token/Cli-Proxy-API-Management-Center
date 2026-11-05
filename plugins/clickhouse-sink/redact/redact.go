// Package redact replaces credentials in captured inference with
// [REDACTED:<rule>:<first 8 hex of sha256(secret)>]. The clickhouse-sink
// plugin applies it before anything is stored, and the deriver applies it
// again to the text it reassembles, so a secret split across stream chunks
// cannot reappear in derived data.
package redact

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
	"gopkg.in/yaml.v3"
)

const (
	// minSecretLen skips short values such as ports and booleans.
	minSecretLen = 12
	// reloadInterval bounds how often secret files are checked for changes.
	// CPA rotates OAuth access tokens, so the known set must follow them.
	reloadInterval = 30 * time.Second
	// RuleKnown names matches of configured secret values.
	RuleKnown = "known"
)

// identifierKeys are request fields that gitleaks' generic rule flags but
// that hold identifiers, not credentials (Codex sends its conversation ID as
// prompt_cache_key). Findings whose match names one of them are kept.
var identifierKeys = []string{"prompt_cache_key"}

func isIdentifierMatch(match string) bool {
	for _, key := range identifierKeys {
		if strings.Contains(match, key) {
			return true
		}
	}
	return false
}

// secretKey matches JSON/YAML keys whose string values are credentials.
var secretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[-_]?key|apikey|cookie|credential|private[-_]?key|authorization|session[-_]?key)`)

// Redactor holds the known secret values and the pattern rules.
type Redactor struct {
	// reloadMu is held for a whole reload, so no row is redacted with a
	// half-loaded secret set.
	reloadMu  sync.Mutex
	mu        sync.Mutex
	files     []string
	mtimes    map[string]time.Time
	lastCheck time.Time
	known     map[string]string // encoded form -> replacement
	disabled  map[string]bool

	detector *detect.Detector
}

// New returns a Redactor with gitleaks' default rules. It returns an error
// when the rules cannot be loaded, so callers can refuse to store bodies.
func New() (*Redactor, error) {
	logging.Logger = zerolog.Nop()
	d, errDetector := detect.NewDetectorDefaultConfig()
	if errDetector != nil {
		return nil, errDetector
	}
	return &Redactor{detector: d, known: map[string]string{}, mtimes: map[string]time.Time{}}, nil
}

// Configure sets the secret sources and loads them before returning, so the
// first request is never redacted against an empty set.
func (r *Redactor) Configure(files, disabledRules []string) {
	r.mu.Lock()
	r.files = files
	r.disabled = make(map[string]bool, len(disabledRules))
	for _, rule := range disabledRules {
		r.disabled[rule] = true
	}
	r.lastCheck = time.Time{}
	r.mtimes = map[string]time.Time{}
	r.mu.Unlock()
	r.reloadIfStale(time.Now())
}

// Result collects what was replaced in one row.
type Result struct {
	Count int
	rules map[string]bool
}

// NewResult returns an empty Result.
func NewResult() *Result { return &Result{rules: map[string]bool{}} }

// Rules returns the distinct rule names that matched, sorted.
func (rd *Result) Rules() []string {
	out := make([]string, 0, len(rd.rules))
	for rule := range rd.rules {
		out = append(out, rule)
	}
	sort.Strings(out)
	return out
}

// Replacer replaces the secrets found for one row.
type Replacer struct {
	rep   *strings.Replacer
	pairs map[string]string
}

// Apply replaces secrets in s and records each replacement in res.
func (rp *Replacer) Apply(s string, res *Result) string {
	if rp == nil || rp.rep == nil || s == "" {
		return s
	}
	return apply(rp.rep, s, res)
}

// NewReplacer builds a replacer for one row: the known secrets plus whatever
// the pattern rules find anywhere in the row's texts.
func (r *Redactor) NewReplacer(texts []string) *Replacer {
	r.reloadIfStale(time.Now())
	r.mu.Lock()
	pairs := make(map[string]string, len(r.known))
	for secret, replacement := range r.known {
		pairs[secret] = replacement
	}
	disabled := r.disabled
	r.mu.Unlock()

	if r.detector != nil {
		joined := strings.Join(texts, "\n")
		for _, finding := range r.detector.DetectString(joined) {
			secret := finding.Secret
			if len(secret) < 8 || disabled[finding.RuleID] || isIdentifierMatch(finding.Match) {
				continue
			}
			if _, exists := pairs[secret]; !exists {
				pairs[secret] = Marker(finding.RuleID, secret)
			}
		}
	}
	if len(pairs) == 0 {
		return &Replacer{}
	}
	// Longest first, so a secret wins over any shorter value inside it.
	keys := make([]string, 0, len(pairs))
	for secret := range pairs {
		keys = append(keys, secret)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	args := make([]string, 0, 2*len(keys))
	for _, secret := range keys {
		args = append(args, secret, pairs[secret])
	}
	return &Replacer{rep: strings.NewReplacer(args...), pairs: pairs}
}

// apply replaces secrets in s and records each replacement.
func apply(rep *strings.Replacer, s string, rd *Result) string {
	out := rep.Replace(s)
	if out == s {
		return s
	}
	added := strings.Count(out, "[REDACTED:") - strings.Count(s, "[REDACTED:")
	if added > 0 {
		rd.Count += added
	}
	for _, m := range markerRule.FindAllStringSubmatch(out, -1) {
		rd.rules[m[1]] = true
	}
	return out
}

var markerRule = regexp.MustCompile(`\[REDACTED:([A-Za-z0-9_.-]+):[0-9a-f]{8}\]`)

// Marker is the replacement text for secret matched by rule.
func Marker(rule, secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "[REDACTED:" + rule + ":" + hex.EncodeToString(sum[:4]) + "]"
}

// reloadIfStale re-reads the secret files when any of them changed.
func (r *Redactor) reloadIfStale(now time.Time) {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	r.mu.Lock()
	if now.Sub(r.lastCheck) < reloadInterval {
		r.mu.Unlock()
		return
	}
	files := r.files
	old := r.mtimes
	r.mu.Unlock()

	paths := expandFiles(files)
	mtimes := make(map[string]time.Time, len(paths))
	changed := len(paths) != len(old)
	for _, p := range paths {
		info, errStat := os.Stat(p)
		if errStat != nil {
			continue
		}
		mtimes[p] = info.ModTime()
		if !old[p].Equal(info.ModTime()) {
			changed = true
		}
	}
	if !changed {
		r.mu.Lock()
		r.lastCheck = now
		r.mu.Unlock()
		return
	}
	known := make(map[string]string)
	for _, p := range paths {
		for _, secret := range readSecrets(p) {
			addKnown(known, secret)
		}
	}
	r.mu.Lock()
	r.known = known
	r.mtimes = mtimes
	r.lastCheck = now
	r.mu.Unlock()
}

// expandFiles turns directories into the files inside them (one level).
func expandFiles(files []string) []string {
	var out []string
	for _, f := range files {
		info, errStat := os.Stat(f)
		if errStat != nil {
			continue
		}
		if !info.IsDir() {
			out = append(out, f)
			continue
		}
		entries, errRead := os.ReadDir(f)
		if errRead != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				out = append(out, filepath.Join(f, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

// addKnown registers a secret in the forms it can appear in a body: raw,
// JSON-escaped and base64. Every form maps to the raw secret's marker.
func addKnown(known map[string]string, secret string) {
	secret = strings.TrimSpace(secret)
	if len(secret) < minSecretLen {
		return
	}
	m := Marker(RuleKnown, secret)
	known[secret] = m
	if quoted, errQuote := json.Marshal(secret); errQuote == nil {
		if escaped := string(quoted[1 : len(quoted)-1]); escaped != secret {
			known[escaped] = m
		}
	}
	known[base64.StdEncoding.EncodeToString([]byte(secret))] = m
	known[base64.RawURLEncoding.EncodeToString([]byte(secret))] = m
}

// readSecrets extracts credentials from one file, chosen by its name:
// .json and .yaml/.yml by key name, .env by value, anything else one per line.
func readSecrets(path string) []string {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil
	}
	name := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasSuffix(name, ".json"):
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return nil
		}
		return collectSecrets(v, false)
	case strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml"):
		var v any
		if yaml.Unmarshal(raw, &v) != nil {
			return nil
		}
		return collectSecrets(v, false)
	case name == ".env" || strings.HasSuffix(name, ".env"):
		var out []string
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if _, value, ok := strings.Cut(line, "="); ok {
				out = append(out, strings.Trim(strings.TrimSpace(value), `"'`))
			}
		}
		return out
	default:
		var out []string
		sc := bufio.NewScanner(bytes.NewReader(raw))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
				out = append(out, line)
			}
		}
		return out
	}
}

// collectSecrets returns every string under a key that names a credential.
func collectSecrets(v any, under bool) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			out = append(out, collectSecrets(child, under || secretKey.MatchString(k))...)
		}
	case []any:
		for _, child := range t {
			out = append(out, collectSecrets(child, under)...)
		}
	case string:
		if under {
			out = append(out, t)
		}
	}
	return out
}
