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
	"github.com/tidwall/gjson"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
	"gopkg.in/yaml.v3"
)

const (
	// minSecretLen skips short values such as ports and booleans.
	minSecretLen = 12
	// minTruncatedLen is the shortest start of a known secret that is redacted
	// on its own, so a copy cut off mid-secret (a reply stopped by max_tokens)
	// is still caught.
	minTruncatedLen = 16
	// reloadInterval bounds how often secret files are checked for changes.
	// CPA rotates OAuth access tokens, so the known set must follow them.
	reloadInterval = 30 * time.Second
	// maxCachedSegments bounds the request-body findings cache. When it is
	// full it is emptied, and the next bodies are scanned again once.
	maxCachedSegments = 50000
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

	// cacheMu guards segments: pattern findings per request-body segment,
	// keyed by the segment's sha256. Findings depend only on the segment and
	// the fixed rule set, so a secret reload does not invalidate them.
	cacheMu  sync.Mutex
	segments map[[32]byte][]finding
}

// finding is a pattern-rule match kept for redaction.
type finding struct {
	secret string
	rule   string
}

// New returns a Redactor with gitleaks' default rules. It returns an error
// when the rules cannot be loaded, so callers can refuse to store bodies.
func New() (*Redactor, error) {
	logging.Logger = zerolog.Nop()
	d, errDetector := detect.NewDetectorDefaultConfig()
	if errDetector != nil {
		return nil, errDetector
	}
	return &Redactor{
		detector: d,
		known:    map[string]string{},
		mtimes:   map[string]time.Time{},
		segments: map[[32]byte][]finding{},
	}, nil
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
	return r.NewRequestReplacer("", texts)
}

// NewRequestReplacer is NewReplacer for a row with a request body. Agent
// clients resend the whole conversation every turn, so the body is scanned one
// segment at a time (each top-level array element, and each other top-level
// field) and each segment's findings are cached. A resent message is then not
// scanned again, and the scan cost no longer grows faster than the body.
// Pattern matches are local, so a segment yields the same findings on its own
// as inside the body. The returned replacer still applies to the whole row.
func (r *Redactor) NewRequestReplacer(body string, texts []string) *Replacer {
	r.reloadIfStale(time.Now())
	r.mu.Lock()
	known, disabled := r.known, r.disabled // replaced on reload, never mutated
	r.mu.Unlock()
	pairs := make(map[string]string, len(known))
	for secret, replacement := range known {
		pairs[secret] = replacement
	}
	all := texts
	if body != "" {
		all = append([]string{body}, texts...)
	}
	for secret, replacement := range known {
		for _, text := range all {
			for _, cut := range truncatedCopies(text, secret) {
				if _, exists := pairs[cut]; !exists {
					pairs[cut] = replacement
				}
			}
		}
	}

	if r.detector != nil {
		var findings []finding
		for _, segment := range bodySegments(body) {
			findings = append(findings, r.detectCached(segment)...)
		}
		findings = append(findings, r.detect(strings.Join(texts, "\n"))...)
		for _, f := range findings {
			if disabled[f.rule] {
				continue
			}
			if _, exists := pairs[f.secret]; !exists {
				pairs[f.secret] = Marker(f.rule, f.secret)
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

// detect runs the pattern rules over s and keeps the findings worth redacting.
func (r *Redactor) detect(s string) []finding {
	if s == "" {
		return nil
	}
	var out []finding
	for _, f := range r.detector.DetectString(s) {
		if len(f.Secret) < 8 || isIdentifierMatch(f.Match) {
			continue
		}
		out = append(out, finding{secret: f.Secret, rule: f.RuleID})
	}
	return out
}

// detectCached is detect for one request-body segment, cached by its hash.
func (r *Redactor) detectCached(segment string) []finding {
	key := sha256.Sum256([]byte(segment))
	r.cacheMu.Lock()
	cached, ok := r.segments[key]
	r.cacheMu.Unlock()
	if ok {
		return cached
	}
	found := r.detect(segment)
	r.cacheMu.Lock()
	if len(r.segments) >= maxCachedSegments {
		r.segments = map[[32]byte][]finding{}
	}
	r.segments[key] = found
	r.cacheMu.Unlock()
	return found
}

// bodySegments splits a JSON object body into the units clients resend
// unchanged: each element of a top-level array, and each other top-level
// field with its key. Any other body is a single segment.
func bodySegments(body string) []string {
	if body == "" {
		return nil
	}
	parsed := gjson.Parse(body)
	if !parsed.IsObject() {
		return []string{body}
	}
	var out []string
	parsed.ForEach(func(key, value gjson.Result) bool {
		if value.IsArray() {
			value.ForEach(func(_, element gjson.Result) bool {
				out = append(out, element.Raw)
				return true
			})
			return true
		}
		out = append(out, key.Raw+":"+value.Raw)
		return true
	})
	return out
}

// truncatedCopies returns the cut-off copies of secret in text: each place
// where at least its first minTruncatedLen characters appear but not the
// whole secret, extended for as long as the text keeps matching it.
func truncatedCopies(text, secret string) []string {
	if len(secret) <= minTruncatedLen {
		return nil
	}
	prefix := secret[:minTruncatedLen]
	var out []string
	for off := 0; ; {
		i := strings.Index(text[off:], prefix)
		if i < 0 {
			return out
		}
		start := off + i
		n := minTruncatedLen
		for n < len(secret) && start+n < len(text) && text[start+n] == secret[n] {
			n++
		}
		if n < len(secret) {
			out = append(out, secret[:n])
		}
		off = start + n
	}
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
