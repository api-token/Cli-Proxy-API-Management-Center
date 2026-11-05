package extract

import (
	"strings"

	"github.com/tidwall/gjson"
)

// streamEvents turns stream chunks into JSON events. It accepts every shape
// the capture stores: unframed SSE lines (`event: x` and `data: {...}` as
// separate chunks, as Codex/Responses delivers them), framed SSE blocks with
// several lines per chunk, bare JSON lines (websocket messages, chat
// passthrough) and a JSON array split into fragments (Gemini without alt=sse).
func streamEvents(chunks []string) []gjson.Result {
	var events []gjson.Result
	for _, chunk := range chunks {
		for _, line := range strings.Split(chunk, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				line = strings.TrimSpace(line[len("data:"):])
			}
			if line == "" || line == "[DONE]" || (line[0] != '{' && line[0] != '[') || !gjson.Valid(line) {
				continue
			}
			r := gjson.Parse(line)
			if r.IsArray() {
				events = append(events, r.Array()...)
			} else if r.IsObject() {
				events = append(events, r)
			}
		}
	}
	if len(events) == 0 {
		joined := strings.TrimSpace(strings.Join(chunks, ""))
		if gjson.Valid(joined) {
			if r := gjson.Parse(joined); r.IsArray() {
				events = r.Array()
			}
		}
	}
	return events
}
