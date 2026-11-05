package extract

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// OpenAI Responses (HTTP SSE, websocket and non-stream), including Codex's
// `additional_tools` input items and namespaced tools.

func responsesRequest(body gjson.Result) parsed {
	p := parsed{
		system:     body.Get("instructions").String(),
		prevRespID: body.Get("previous_response_id").String(),
	}
	for _, t := range body.Get("tools").Array() {
		p.tools = append(p.tools, responsesToolNames(t)...)
	}
	input := body.Get("input")
	if input.Type == gjson.String {
		p.input = append(p.input, item{role: "user", typ: "message", text: input.String()})
		return p
	}
	for i, x := range input.Array() {
		it := responsesItem(x)
		it.msg = i
		p.input = append(p.input, it)
		if it.typ == "additional_tools" {
			for _, t := range x.Get("tools").Array() {
				p.tools = append(p.tools, responsesToolNames(t)...)
			}
		}
	}
	return p
}

// responsesToolNames flattens a tool definition; namespaces contribute their members.
func responsesToolNames(t gjson.Result) []string {
	if t.Get("type").String() == "namespace" || t.Get("tools").IsArray() {
		var names []string
		for _, sub := range t.Get("tools").Array() {
			names = append(names, responsesToolNames(sub)...)
		}
		return names
	}
	if name := t.Get("name").String(); name != "" {
		return []string{name}
	}
	if name := t.Get("function.name").String(); name != "" {
		return []string{name}
	}
	if typ := t.Get("type").String(); typ != "" {
		return []string{typ}
	}
	return nil
}

func responsesItem(x gjson.Result) item {
	typ := x.Get("type").String()
	role := x.Get("role").String()
	if typ == "" && role != "" {
		typ = "message"
	}
	switch {
	case typ == "message":
		if role == "" {
			role = "user"
		}
		return item{role: role, typ: typ, text: contentText(x.Get("content")), model: role == "assistant"}
	case typ == "reasoning":
		var parts []string
		for _, s := range x.Get("summary").Array() {
			if t := s.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
		}
		for _, c := range x.Get("content").Array() {
			if t := c.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
		}
		return item{role: "assistant", typ: typ, text: strings.Join(parts, "\n"), model: true}
	case typ == "additional_tools":
		var names []string
		for _, t := range x.Get("tools").Array() {
			names = append(names, responsesToolNames(t)...)
		}
		if role == "" {
			role = "developer"
		}
		return item{role: role, typ: typ, text: strings.Join(names, ", ")}
	case strings.HasSuffix(typ, "_output"):
		out := x.Get("output")
		text := out.String()
		if out.IsArray() || out.IsObject() {
			text = contentText(out)
		}
		return item{role: "tool", typ: typ, callID: x.Get("call_id").String(), text: text,
			isError: x.Get("status").String() == "failed"}
	case strings.HasSuffix(typ, "_call"):
		name := x.Get("name").String()
		if name == "" {
			name = strings.TrimSuffix(typ, "_call")
		}
		callID := x.Get("call_id").String()
		if callID == "" {
			callID = x.Get("id").String()
		}
		var args string
		for _, k := range []string{"arguments", "input", "action", "query"} {
			if v := x.Get(k); v.Exists() {
				if v.Type == gjson.String {
					args = v.String()
				} else {
					args = v.Raw
				}
				break
			}
		}
		return item{role: "assistant", typ: typ, toolName: name, callID: callID, args: args, model: true}
	}
	return item{role: role, typ: typ, text: contentText(x.Get("content"))}
}

func responsesResponse(body gjson.Result, p *parsed) {
	r := body
	if inner := body.Get("response"); inner.IsObject() {
		r = inner
	}
	if !r.Get("id").Exists() && !r.Get("output").Exists() {
		return
	}
	p.anyResponse = true
	p.complete = true
	p.responseID = r.Get("id").String()
	p.stopReason = responsesStop(r)
	for _, x := range r.Get("output").Array() {
		p.output = append(p.output, responsesItem(x))
	}
}

func responsesStop(r gjson.Result) string {
	if reason := r.Get("incomplete_details.reason").String(); reason != "" {
		return reason
	}
	return r.Get("status").String()
}

type responsesSlot struct {
	added, done          gjson.Result
	text, reasoning, arg strings.Builder
}

// responsesStream uses the terminal event's output when it has one. Codex's
// backend sends response.completed with an empty output array, so the items
// come from response.output_item.done, and from added items plus deltas for
// anything not finished (canceled or incomplete streams).
func responsesStream(events []gjson.Result, p *parsed) {
	slots := map[int64]*responsesSlot{}
	slot := func(ev gjson.Result) *responsesSlot {
		idx := ev.Get("output_index").Int()
		s := slots[idx]
		if s == nil {
			s = &responsesSlot{}
			slots[idx] = s
		}
		return s
	}
	var terminal gjson.Result
	for _, ev := range events {
		if id := ev.Get("response.id").String(); id != "" {
			p.responseID = id
		}
		switch ev.Get("type").String() {
		case "response.output_item.added":
			slot(ev).added = ev.Get("item")
		case "response.output_item.done":
			slot(ev).done = ev.Get("item")
		case "response.output_text.delta", "response.refusal.delta":
			slot(ev).text.WriteString(ev.Get("delta").String())
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			slot(ev).reasoning.WriteString(ev.Get("delta").String())
		case "response.reasoning_summary_part.added":
			if s := slot(ev); s.reasoning.Len() > 0 {
				s.reasoning.WriteString("\n")
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			slot(ev).arg.WriteString(ev.Get("delta").String())
		case "response.completed", "response.incomplete", "response.failed", "response.done":
			terminal = ev.Get("response")
		case "error":
			p.respErr = "stream error event"
		default:
			continue
		}
		p.anyResponse = true
	}
	if terminal.Exists() {
		p.complete = true
		p.stopReason = responsesStop(terminal)
		if out := terminal.Get("output").Array(); len(out) > 0 {
			for _, x := range out {
				p.output = append(p.output, responsesItem(x))
			}
			return
		}
	}
	idxs := make([]int64, 0, len(slots))
	for idx := range slots {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	for _, idx := range idxs {
		s := slots[idx]
		if s.done.Exists() {
			p.output = append(p.output, responsesItem(s.done))
			continue
		}
		it := responsesItem(s.added)
		if !s.added.Exists() {
			it = item{role: "assistant", typ: "message", model: true}
			if s.reasoning.Len() > 0 && s.text.Len() == 0 {
				it.typ = "reasoning"
			}
		}
		switch {
		case it.typ == "message" && it.text == "":
			it.text = s.text.String()
		case it.typ == "reasoning" && it.text == "":
			it.text = s.reasoning.String()
		case strings.HasSuffix(it.typ, "_call") && it.args == "":
			it.args = s.arg.String()
		}
		p.output = append(p.output, it)
	}
}
