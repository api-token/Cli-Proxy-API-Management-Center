package extract

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// Claude Messages API.

func claudeRequest(body gjson.Result) parsed {
	var p parsed
	p.system = contentText(body.Get("system"))
	for _, t := range body.Get("tools").Array() {
		if name := t.Get("name").String(); name != "" {
			p.tools = append(p.tools, name)
		} else {
			p.tools = append(p.tools, t.Get("type").String())
		}
	}
	for i, m := range body.Get("messages").Array() {
		role := m.Get("role").String()
		c := m.Get("content")
		if c.Type == gjson.String {
			p.input = append(p.input, item{role: role, typ: "message", text: c.String(), model: role == "assistant", msg: i})
			continue
		}
		for _, b := range c.Array() {
			it := claudeBlock(role, b)
			it.msg = i
			p.input = append(p.input, it)
		}
	}
	return p
}

// claudeBlock normalizes one content block. Every block of an assistant
// message is model-produced; tool_result blocks get role "tool".
func claudeBlock(role string, b gjson.Result) item {
	typ := b.Get("type").String()
	model := role == "assistant"
	switch typ {
	case "text":
		return item{role: role, typ: "message", text: b.Get("text").String(), model: model}
	case "thinking":
		return item{role: role, typ: typ, text: b.Get("thinking").String(), model: model}
	case "tool_use", "server_tool_use", "mcp_tool_use":
		args := b.Get("input")
		raw := args.Raw
		if args.Type == gjson.String {
			raw = args.String()
		}
		return item{role: role, typ: typ, toolName: b.Get("name").String(), callID: b.Get("id").String(), args: raw, model: model}
	case "tool_result":
		return item{role: "tool", typ: typ, callID: b.Get("tool_use_id").String(), text: contentText(b.Get("content")),
			isError: b.Get("is_error").Bool()}
	}
	if strings.HasSuffix(typ, "_tool_result") {
		return item{role: role, typ: typ, callID: b.Get("tool_use_id").String(), text: contentText(b.Get("content")),
			isError: b.Get("is_error").Bool(), model: model}
	}
	return item{role: role, typ: typ, text: partText(b), model: model}
}

func claudeResponse(body gjson.Result, p *parsed) {
	if body.Get("type").String() != "message" && !body.Get("content").IsArray() {
		return
	}
	p.anyResponse = true
	p.complete = true
	p.responseID = body.Get("id").String()
	p.stopReason = body.Get("stop_reason").String()
	for _, b := range body.Get("content").Array() {
		p.output = append(p.output, claudeBlock("assistant", b))
	}
}

type claudeSlot struct {
	start gjson.Result
	text  strings.Builder
	json  strings.Builder
}

// claudeStream assembles each content block from content_block_start and its
// deltas (text, thinking, input_json_delta).
func claudeStream(events []gjson.Result, p *parsed) {
	slots := map[int64]*claudeSlot{}
	slot := func(ev gjson.Result) *claudeSlot {
		idx := ev.Get("index").Int()
		s := slots[idx]
		if s == nil {
			s = &claudeSlot{}
			slots[idx] = s
		}
		return s
	}
	for _, ev := range events {
		switch ev.Get("type").String() {
		case "message_start":
			p.responseID = ev.Get("message.id").String()
		case "content_block_start":
			slot(ev).start = ev.Get("content_block")
		case "content_block_delta":
			s := slot(ev)
			d := ev.Get("delta")
			switch d.Get("type").String() {
			case "text_delta":
				s.text.WriteString(d.Get("text").String())
			case "thinking_delta":
				s.text.WriteString(d.Get("thinking").String())
			case "input_json_delta":
				s.json.WriteString(d.Get("partial_json").String())
			}
		case "message_delta":
			if r := ev.Get("delta.stop_reason").String(); r != "" {
				p.stopReason = r
				p.complete = true
			}
		case "message_stop":
			p.complete = true
		case "error":
			p.respErr = "stream error event"
		default:
			continue
		}
		p.anyResponse = true
	}
	idxs := make([]int64, 0, len(slots))
	for idx := range slots {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	for _, idx := range idxs {
		s := slots[idx]
		it := claudeBlock("assistant", s.start)
		switch it.typ {
		case "message", "thinking":
			it.text += s.text.String()
		case "tool_use", "server_tool_use", "mcp_tool_use":
			if s.json.Len() > 0 {
				it.args = s.json.String()
			}
		}
		p.output = append(p.output, it)
	}
}
