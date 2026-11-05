package extract

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Gemini generateContent / streamGenerateContent (and Gemini CLI's wrapped form).

func geminiRequest(body gjson.Result) parsed {
	var p parsed
	sys := body.Get("systemInstruction")
	if !sys.Exists() {
		sys = body.Get("system_instruction")
	}
	p.system = geminiPartsText(sys.Get("parts"))
	for _, t := range body.Get("tools").Array() {
		decls := t.Get("functionDeclarations")
		if !decls.Exists() {
			decls = t.Get("function_declarations")
		}
		if decls.Exists() {
			for _, d := range decls.Array() {
				p.tools = append(p.tools, d.Get("name").String())
			}
			continue
		}
		t.ForEach(func(k, _ gjson.Result) bool {
			p.tools = append(p.tools, k.String())
			return true
		})
	}
	for i, c := range body.Get("contents").Array() {
		role := geminiRole(c.Get("role").String())
		for _, part := range c.Get("parts").Array() {
			it := geminiPart(role, part)
			it.msg = i
			p.input = append(p.input, it)
		}
	}
	return p
}

func geminiRole(r string) string {
	switch r {
	case "model":
		return "assistant"
	case "function", "tool":
		return "tool"
	case "":
		return "user"
	}
	return r
}

func geminiPartsText(parts gjson.Result) string {
	var out []string
	for _, p := range parts.Array() {
		if t := p.Get("text").String(); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

func geminiPart(role string, part gjson.Result) item {
	model := role == "assistant"
	switch {
	case part.Get("functionCall").Exists():
		fc := part.Get("functionCall")
		return item{role: role, typ: "function_call", toolName: fc.Get("name").String(), callID: fc.Get("id").String(),
			args: fc.Get("args").Raw, model: model}
	case part.Get("functionResponse").Exists():
		fr := part.Get("functionResponse")
		return item{role: "tool", typ: "function_response", toolName: fr.Get("name").String(), callID: fr.Get("id").String(),
			text: fr.Get("response").Raw, isError: fr.Get("response.error").Exists()}
	case part.Get("thought").Bool():
		return item{role: role, typ: "thinking", text: part.Get("text").String(), model: model}
	case part.Get("text").Exists():
		return item{role: role, typ: "message", text: part.Get("text").String(), model: model}
	case part.Get("inlineData").Exists(), part.Get("inline_data").Exists():
		return item{role: role, typ: "inline_data", text: "[media]", model: model}
	case part.Get("fileData").Exists(), part.Get("file_data").Exists():
		return item{role: role, typ: "file_data", text: "[file]", model: model}
	case part.Get("executableCode").Exists():
		return item{role: role, typ: "executable_code", toolName: "code_execution", args: part.Get("executableCode.code").String(), model: model}
	case part.Get("codeExecutionResult").Exists():
		return item{role: role, typ: "code_execution_result", text: part.Get("codeExecutionResult.output").String(), model: model}
	}
	return item{role: role, typ: "unknown", model: model}
}

func geminiResponse(body gjson.Result, p *parsed) {
	geminiStream([]gjson.Result{body}, p)
}

// geminiStream concatenates consecutive text (and thought) parts of candidate 0
// across chunks; function calls stay separate items.
func geminiStream(events []gjson.Result, p *parsed) {
	for _, ev := range events {
		if inner := ev.Get("response"); inner.IsObject() {
			ev = inner // Gemini CLI wraps each chunk
		}
		if id := ev.Get("responseId").String(); id != "" {
			p.responseID = id
		}
		if ev.Get("error").Exists() {
			p.respErr = "stream error event"
		}
		cand := ev.Get("candidates.0")
		if !cand.Exists() {
			continue
		}
		p.anyResponse = true
		for _, part := range cand.Get("content.parts").Array() {
			it := geminiPart("assistant", part)
			if n := len(p.output); n > 0 && (it.typ == "message" || it.typ == "thinking") && p.output[n-1].typ == it.typ {
				p.output[n-1].text += it.text
				continue
			}
			p.output = append(p.output, it)
		}
		if fr := cand.Get("finishReason").String(); fr != "" {
			p.stopReason = fr
			p.complete = true
		}
	}
}
