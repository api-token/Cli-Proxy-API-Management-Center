package redact

import (
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// textKeys are the JSON keys whose string values carry streamed text or tool
// arguments in the supported protocols (OpenAI chat and Responses, Claude,
// Gemini).
var textKeys = map[string]bool{
	"delta": true, "text": true, "content": true, "partial_json": true, "thinking": true,
	"arguments": true, "reasoning_content": true, "reasoning": true, "refusal": true,
}

// RedactSplitSecrets finds secrets that only appear once the streamed text of
// consecutive chunks is joined (a model repeating a secret a few characters
// per delta), and replaces every chunk that carries part of one with the
// secret's marker. Run it after Apply, which handles secrets inside a single
// chunk.
func (r *Redactor) RedactSplitSecrets(chunks []string, res *Result) {
	if len(chunks) < 2 {
		return
	}
	starts := make([]int, len(chunks))
	var b strings.Builder
	for i, c := range chunks {
		starts[i] = b.Len()
		b.WriteString(chunkText(c))
	}
	joined := b.String()
	if joined == "" {
		return
	}

	r.reloadIfStale(time.Now())
	r.mu.Lock()
	known, disabled := r.known, r.disabled // replaced on reload, never mutated
	r.mu.Unlock()

	type hit struct {
		start, end int
		marker     string
	}
	var hits []hit
	addAll := func(secret, marker string) {
		for off := 0; ; {
			i := strings.Index(joined[off:], secret)
			if i < 0 {
				return
			}
			hits = append(hits, hit{off + i, off + i + len(secret), marker})
			off += i + len(secret)
		}
	}
	for secret, marker := range known {
		addAll(secret, marker)
	}
	if r.detector != nil {
		for _, finding := range r.detector.DetectString(joined) {
			if len(finding.Secret) < 8 || disabled[finding.RuleID] || isIdentifierMatch(finding.Match) {
				continue
			}
			addAll(finding.Secret, Marker(finding.RuleID, finding.Secret))
		}
	}

	replace := map[int]string{}
	for _, h := range hits {
		first, last := chunkAt(starts, h.start), chunkAt(starts, h.end-1)
		if first == last {
			continue
		}
		for i := first; i <= last; i++ {
			replace[i] = h.marker
		}
	}
	for i, m := range replace {
		chunks[i] = m
		res.Count++
		if sub := markerRule.FindStringSubmatch(m); sub != nil {
			res.rules[sub[1]] = true
		}
	}
}

// chunkAt returns the index of the chunk whose text contains offset.
func chunkAt(starts []int, offset int) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
}

// chunkText returns the streamed text in one chunk, in document order.
func chunkText(chunk string) string {
	var b strings.Builder
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(line[len("data:"):])
		}
		if line == "" || (line[0] != '{' && line[0] != '[') || !gjson.Valid(line) {
			continue
		}
		collectText(gjson.Parse(line), &b)
	}
	return b.String()
}

func collectText(v gjson.Result, b *strings.Builder) {
	v.ForEach(func(key, value gjson.Result) bool {
		switch {
		case value.Type == gjson.String && textKeys[key.String()]:
			b.WriteString(value.String())
		case value.IsObject() || value.IsArray():
			collectText(value, b)
		}
		return true
	})
}
