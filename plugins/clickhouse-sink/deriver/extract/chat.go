package extract

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// OpenAI chat completions.

func chatRequest(body gjson.Result) parsed {
	var p parsed
	for _, t := range body.Get("tools").Array() {
		if name := t.Get("function.name").String(); name != "" {
			p.tools = append(p.tools, name)
		} else if name := t.Get("name").String(); name != "" {
			p.tools = append(p.tools, name)
		} else {
			p.tools = append(p.tools, t.Get("type").String())
		}
	}
	for i, m := range body.Get("messages").Array() {
		for _, it := range chatMessageItems(m) {
			it.msg = i
			p.input = append(p.input, it)
		}
	}
	return p
}

// chatMessageItems splits one chat message into items: reasoning, text, then
// one item per tool call.
func chatMessageItems(m gjson.Result) []item {
	role := m.Get("role").String()
	switch role {
	case "tool", "function":
		return []item{{role: "tool", typ: "tool_result", callID: m.Get("tool_call_id").String(),
			toolName: m.Get("name").String(), text: contentText(m.Get("content"))}}
	case "":
		role = "user"
	}
	model := role == "assistant"
	var items []item
	if r := m.Get("reasoning_content").String(); r != "" {
		items = append(items, item{role: role, typ: "reasoning", text: r, model: model})
	}
	if text := contentText(m.Get("content")); text != "" || !m.Get("tool_calls").Exists() {
		items = append(items, item{role: role, typ: "message", text: text, model: model})
	}
	for _, tc := range m.Get("tool_calls").Array() {
		items = append(items, item{role: role, typ: "tool_call", toolName: tc.Get("function.name").String(),
			callID: tc.Get("id").String(), args: tc.Get("function.arguments").String(), model: model})
	}
	return items
}

func chatResponse(body gjson.Result, p *parsed) {
	choice := body.Get("choices.0")
	if !choice.Exists() {
		return
	}
	p.anyResponse = true
	p.complete = true
	p.responseID = body.Get("id").String()
	p.stopReason = choice.Get("finish_reason").String()
	p.output = chatMessageItems(choice.Get("message"))
	for i := range p.output {
		p.output[i].role = "assistant"
		p.output[i].model = true
	}
}

type chatToolCall struct {
	id, name string
	args     strings.Builder
}

// chatStream collects content, reasoning and tool_calls deltas (by index) of choice 0.
func chatStream(events []gjson.Result, p *parsed) {
	var text, reasoning strings.Builder
	calls := map[int64]*chatToolCall{}
	for _, ev := range events {
		if id := ev.Get("id").String(); id != "" && p.responseID == "" {
			p.responseID = id
		}
		if ev.Get("error").Exists() {
			p.respErr = "stream error event"
		}
		for _, ch := range ev.Get("choices").Array() {
			if ch.Get("index").Int() != 0 {
				continue
			}
			p.anyResponse = true
			d := ch.Get("delta")
			text.WriteString(d.Get("content").String())
			reasoning.WriteString(d.Get("reasoning_content").String())
			for _, tc := range d.Get("tool_calls").Array() {
				idx := tc.Get("index").Int()
				c := calls[idx]
				if c == nil {
					c = &chatToolCall{}
					calls[idx] = c
				}
				if id := tc.Get("id").String(); id != "" {
					c.id = id
				}
				if name := tc.Get("function.name").String(); name != "" {
					c.name = name
				}
				c.args.WriteString(tc.Get("function.arguments").String())
			}
			if fr := ch.Get("finish_reason").String(); fr != "" {
				p.stopReason = fr
				p.complete = true
			}
		}
	}
	if !p.anyResponse {
		return
	}
	if reasoning.Len() > 0 {
		p.output = append(p.output, item{role: "assistant", typ: "reasoning", text: reasoning.String(), model: true})
	}
	if text.Len() > 0 || len(calls) == 0 {
		p.output = append(p.output, item{role: "assistant", typ: "message", text: text.String(), model: true})
	}
	idxs := make([]int64, 0, len(calls))
	for idx := range calls {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	for _, idx := range idxs {
		c := calls[idx]
		p.output = append(p.output, item{role: "assistant", typ: "tool_call", toolName: c.name, callID: c.id, args: c.args.String(), model: true})
	}
}
