package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Non-streaming accumulator (ported from freebuff-api-kiprana
// CompletionAccumulator).
// ---------------------------------------------------------------------------

// toolCall is one assembled tool call: id/type/function.name come from the
// first fragment, function.arguments is concatenated across fragments.
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Accumulator assembles a non-streaming chat.completion response from
// upstream SSE lines. It is not safe for concurrent use (one stream, one
// accumulator).
type Accumulator struct {
	id                string
	created           int64
	model             string
	contentParts      []string
	reasoningParts    []string
	finishReason      string
	usage             any
	systemFingerprint string
	toolCalls         map[int]*toolCall
}

// NewAccumulator returns an accumulator with a fresh chatcmpl- id and
// created timestamp; model/id/created are refined by the first chunks seen.
func NewAccumulator() *Accumulator {
	return &Accumulator{
		id:        "chatcmpl-" + randHex(16),
		created:   time.Now().Unix(),
		toolCalls: make(map[int]*toolCall),
	}
}

// Add parses one SSE data line (with or without "data: " prefix; [DONE] and
// non-data lines are ignored) and accumulates its content into the response.
func (a *Accumulator) Add(line []byte) error {
	data, ok := parseSSEData(line)
	if !ok {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	var chunk map[string]any
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("convert: invalid chunk JSON: %w", err)
	}
	if errVal, ok := chunk["error"]; ok && errVal != nil {
		if errMap, ok := errVal.(map[string]any); ok {
			if msg, ok := errMap["message"].(string); ok && msg != "" {
				return fmt.Errorf("upstream error: %s", msg)
			}
		} else if errStr, ok := errVal.(string); ok && errStr != "" {
			return fmt.Errorf("upstream error: %s", errStr)
		}
		return fmt.Errorf("upstream error: %v", errVal)
	}
	a.accumulate(chunk)
	return nil
}

func (a *Accumulator) accumulate(chunk map[string]any) {
	if id, ok := chunk["id"].(string); ok && id != "" {
		a.id = id
	}
	if created, ok := numInt64(chunk["created"]); ok && created > 0 {
		a.created = created
	}
	if model, ok := chunk["model"].(string); ok && model != "" {
		a.model = model
	}
	if usage, ok := chunk["usage"]; ok && usage != nil {
		a.usage = usage
	}
	if fp, ok := chunk["system_fingerprint"].(string); ok && fp != "" {
		a.systemFingerprint = fp
	}
	for _, c := range choicesOf(chunk) {
		delta, _ := c["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			a.contentParts = append(a.contentParts, content)
		}
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			a.reasoningParts = append(a.reasoningParts, rc)
		} else if r, ok := delta["reasoning"].(string); ok && r != "" {
			a.reasoningParts = append(a.reasoningParts, r)
		}
		for _, tc := range toolCallsOf(delta) {
			a.addToolCall(tc)
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			a.finishReason = fr
		}
	}
}

// addToolCall stitches one tool-call fragment by index: id/type/name are
// taken from the first fragment that provides them, arguments are appended
// across fragments.
func (a *Accumulator) addToolCall(tc map[string]any) {
	index := 0
	if i, ok := numInt64(tc["index"]); ok {
		index = int(i)
	}
	cur, ok := a.toolCalls[index]
	if !ok {
		cur = &toolCall{ID: "call_" + randHex(12), Type: "function"}
		a.toolCalls[index] = cur
	}
	if id, ok := tc["id"].(string); ok && id != "" {
		cur.ID = id
	}
	if typ, ok := tc["type"].(string); ok && typ != "" {
		cur.Type = typ
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok && name != "" {
			cur.Function.Name = name
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			cur.Function.Arguments += args
		}
	}
}

// xmlToolCallRegex matches XML-based tool calls such as:
//
//	<tool_call>
//	<function=bash>
//	<parameter=command>...</parameter>
//	</function>
//	</tool_call>
//
// or <tool_call>{"name":"...","arguments":{...}}</tool_call>
//
// codebuff_tool_call is the upstream's own canonical XML tag (issue #144;
// reference common/src/tools/constants.ts — the CLI's stream parser
// util/stream-xml-parser.ts extracts exactly this tag from model output).
var (
	xmlToolCallBlockRe = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>|<codebuff_tool_call>(.*?)</codebuff_tool_call>|<function_call>(.*?)</function_call>|<\|?tool[_\-]?call[_\-]?start\|?>(.*?)<\|?tool[_\-]?call[_\-]?end\|?>`)
	fencedToolCallRe   = regexp.MustCompile("(?s)```(?:json|tool_?call)?\\s*\\n?(\\{\\s*\"(?:name|function)\"\\s*:\\s*.*?\\})\\s*\\n?```")
	xmlFunctionHeadRe  = regexp.MustCompile(`(?i)<function[=\s]+["']?([^>"\s]+)["']?>`)
	xmlParamRe         = regexp.MustCompile(`(?s)<parameter[=\s]+["']?([^>"\s]+)["']?>(.*?)</parameter>|<param[=\s]+["']?([^>"\s]+)["']?>(.*?)</param>`)
	danglingToolTagsRe = regexp.MustCompile(`(?i)</?(?:tool_call|codebuff_tool_call|function_call|function|parameter|param|\|?tool[_\-]?call[_\-]?(?:start|end)\|?)(?:[=\s][^>]*)?>`)
)

// extractXMLToolCalls parses text-based tool calls (Hermes/Qwen/MiMo XML format)
// that were emitted into content instead of native OpenAI tool_calls fields.
// It returns the cleaned content string and the extracted tool calls.
func extractXMLToolCalls(content string) (string, []*toolCall) {
	matches := xmlToolCallBlockRe.FindAllStringSubmatchIndex(content, -1)
	fencedMatches := fencedToolCallRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 && len(fencedMatches) == 0 {
		return content, nil
	}

	var calls []*toolCall

	// 1. Check XML block matches (<tool_call>...</tool_call>)
	for _, loc := range matches {
		block := content[loc[0]:loc[1]]
		inner := xmlToolCallBlockRe.FindStringSubmatch(block)
		if len(inner) < 2 {
			continue
		}
		raw := inner[1]
		if raw == "" && len(inner) > 2 {
			raw = inner[2]
		}
		if raw == "" && len(inner) > 3 {
			raw = inner[3]
		}
		if raw == "" && len(inner) > 4 {
			raw = inner[4]
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		if tc := parseToolCallRaw(raw); tc != nil {
			calls = append(calls, tc)
		}
	}

	// 2. Check fenced code blocks (```json {"name": "..."} ```)
	if len(calls) == 0 {
		for _, loc := range fencedMatches {
			block := content[loc[0]:loc[1]]
			inner := fencedToolCallRe.FindStringSubmatch(block)
			if len(inner) >= 2 {
				raw := strings.TrimSpace(inner[1])
				if tc := parseToolCallRaw(raw); tc != nil {
					calls = append(calls, tc)
				}
			}
		}
	}

	if len(calls) == 0 {
		return content, nil
	}

	// Clean the tool_call blocks from content
	cleaned := xmlToolCallBlockRe.ReplaceAllString(content, "")
	cleaned = fencedToolCallRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned), calls
}

// parseToolCallRaw parses a single raw tool call string in either JSON or XML format.
func parseToolCallRaw(raw string) *toolCall {
	// Try direct JSON: {"name":"...", "arguments":{...}} or {"function":{...}}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		var jObj map[string]any
		if err := json.Unmarshal([]byte(raw), &jObj); err == nil {
			name, _ := jObj["name"].(string)
			if name == "" {
				if fnObj, ok := jObj["function"].(map[string]any); ok {
					name, _ = fnObj["name"].(string)
				} else {
					name, _ = jObj["function"].(string)
				}
			}
			if name != "" {
				var argsStr string
				if argsObj, ok := jObj["arguments"].(map[string]any); ok {
					if b, err := json.Marshal(argsObj); err == nil {
						argsStr = string(b)
					}
				} else if aStr, ok := jObj["arguments"].(string); ok {
					argsStr = aStr
				} else {
					argsStr = "{}"
				}
				return &toolCall{
					ID:   "call_" + randHex(12),
					Type: "function",
					Function: toolFunction{
						Name:      name,
						Arguments: argsStr,
					},
				}
			}
		}
	}

	// Try XML format: <function=NAME><parameter=KEY>VAL</parameter></function>
	fnMatch := xmlFunctionHeadRe.FindStringSubmatch(raw)
	if len(fnMatch) >= 2 {
		fnName := strings.TrimSpace(fnMatch[1])
		paramMatches := xmlParamRe.FindAllStringSubmatch(raw, -1)
		argsMap := make(map[string]any)
		for _, pm := range paramMatches {
			pName := pm[1]
			pVal := pm[2]
			if pName == "" && len(pm) > 4 {
				pName = pm[3]
				pVal = pm[4]
			}
			pName = strings.TrimSpace(pName)
			pVal = strings.TrimSpace(pVal)
			argsMap[pName] = pVal
		}
		argsBytes, _ := json.Marshal(argsMap)
		return &toolCall{
			ID:   "call_" + randHex(12),
			Type: "function",
			Function: toolFunction{
				Name:      fnName,
				Arguments: string(argsBytes),
			},
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Streaming XML tool-call extraction (parity with the non-streaming
// accumulator's extractXMLToolCalls at Finish).
//
// Models such as MiMo/Hermes/Qwen emit tool calls as XML/JSON text blocks
// inline in delta.content (<tool_call>, <codebuff_tool_call>,
// <function_call>, <|tool_call_start|>, or fenced JSON) instead of native
// delta.tool_calls. The accumulator can parse a complete response at Finish;
// streaming relays need everything incrementally because a block may span
// many SSE fragments and text BEFORE a block must be relayed immediately.
// ---------------------------------------------------------------------------

// maxStreamXMLBuffer bounds how much content one candidate tool-call block
// may buffer before it is flushed as plain text — false-positive recovery for
// prose that merely mentions an opener tag. A var so tests can shrink it.
var maxStreamXMLBuffer = 64 * 1024

var (
	xmlStreamPipeOpenRe  = regexp.MustCompile(`<\|?tool[_\-]?call[_\-]?start\|?>`)
	xmlStreamPipeCloseRe = regexp.MustCompile(`<\|?tool[_\-]?call[_\-]?end\|?>`)
)

// xmlStreamShape identifies which block form an open candidate belongs to.
type xmlStreamShape int

const (
	xmlShapeNone xmlStreamShape = iota
	xmlShapeToolCall
	xmlShapeCodebuff
	xmlShapeFunctionCall
	xmlShapePipe
	xmlShapeFence
)

// xmlStreamClosers maps literal block shapes to their closing tag.
var xmlStreamClosers = map[xmlStreamShape]string{
	xmlShapeToolCall:     "</tool_call>",
	xmlShapeCodebuff:     "</codebuff_tool_call>",
	xmlShapeFunctionCall: "</function_call>",
}

// XMLToolCallExtractor incrementally converts XML-based tool calls embedded
// in streamed content into native toolCall values. One instance per stream;
// not safe for concurrent use.
//
// Lifecycle: Feed(contentFragment) for every content delta in order; Flush()
// once at stream end (before the terminal frame). Feed returns the text that
// is safe to relay immediately (everything before the earliest candidate
// opener, plus any false-positive block that failed to parse) and any
// completed tool calls. Text inside a candidate block is withheld until the
// block closes or the buffer bound is exceeded.
type XMLToolCallExtractor struct {
	buffered   string
	shape      xmlStreamShape
	fenceBrace int // xmlShapeFence: index in buffered just past the opening '{'
}

// Feed processes one content fragment. It returns the fragment's safe text
// (possibly shorter than the input while a candidate block is open) and any
// tool calls that completed within it.
func (x *XMLToolCallExtractor) Feed(fragment string) (string, []*toolCall) {
	if x.buffered == "" && x.findOpener(fragment) < 0 {
		return fragment, nil
	}
	var text strings.Builder
	var calls []*toolCall
	rest := fragment
	for {
		if x.buffered != "" {
			// A candidate block is open: absorb the whole fragment (or the
			// remainder after a block just closed) before looking for its
			// closer — the closer may land in any later fragment.
			x.buffered += rest
			rest = ""
		} else {
			idx := x.findOpener(rest)
			if idx < 0 {
				text.WriteString(rest)
				return text.String(), calls
			}
			text.WriteString(rest[:idx])
			x.buffered = rest[idx:]
			rest = ""
		}
		if end := x.closerEnd(); end >= 0 {
			block := x.buffered[:end]
			remainder := x.buffered[end:]
			x.buffered = ""
			x.shape = xmlShapeNone
			_, parsed := extractXMLToolCalls(block)
			if len(parsed) > 0 {
				calls = append(calls, parsed...)
			} else {
				text.WriteString(block) // false positive: keep as plain text
			}
			rest = remainder
			continue
		}
		if len(x.buffered) > maxStreamXMLBuffer {
			text.WriteString(x.buffered)
			x.buffered = ""
			x.shape = xmlShapeNone
			return text.String(), calls
		}
		return text.String(), calls
	}
}

// Flush releases any still-open candidate block at stream end: complete but
// unclosed blocks are still parsed; the remainder is returned as text with
// dangling tool tags scrubbed (mirroring the accumulator's Finish).
func (x *XMLToolCallExtractor) Flush() (string, []*toolCall) {
	if x.buffered == "" {
		return "", nil
	}
	buffered := x.buffered
	x.buffered = ""
	x.shape = xmlShapeNone
	cleaned, calls := extractXMLToolCalls(buffered)
	if len(calls) > 0 {
		return cleaned, calls
	}
	return danglingToolTagsRe.ReplaceAllString(buffered, ""), nil
}

// findOpener returns the index of the earliest candidate block opener in s,
// or -1. For fenced blocks the opener only counts once a '{' (after optional
// json/tool_call tag and whitespace) is visible in the same fragment.
func (x *XMLToolCallExtractor) findOpener(s string) int {
	best := -1
	// Literal openers.
	for shape, open := range map[xmlStreamShape]string{
		xmlShapeToolCall:     "<tool_call>",
		xmlShapeCodebuff:     "<codebuff_tool_call>",
		xmlShapeFunctionCall: "<function_call>",
	} {
		if i := strings.Index(s, open); i >= 0 && (best < 0 || i < best) {
			best = i
			x.shape = shape
		}
	}
	// Pipe form: <|tool_call_start|> / <tool_call_start>.
	if loc := xmlStreamPipeOpenRe.FindStringIndex(s); loc != nil && (best < 0 || loc[0] < best) {
		best = loc[0]
		x.shape = xmlShapePipe
	}
	// Fenced JSON: ```json {"name"... (opener only counts with a visible '{').
	// Only the earliest qualifying fence can win; a fence that loses to an
	// earlier literal must not clobber the chosen shape.
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], "```")
		if i < 0 {
			break
		}
		i += from
		if brace := xmlStreamFenceBrace(s, i+3); brace >= 0 {
			if best < 0 || i < best {
				best = i
				x.shape = xmlShapeFence
				x.fenceBrace = brace + 1
			}
			break // later fences are later still; the earliest one decided
		}
		from = i + 3
	}
	return best
}

// xmlStreamFenceBrace returns the index of the '{' that follows a fence
// opener token (optional json/tool_call tag + whitespace), or -1 when the
// fragment ends before a '{' is visible. A plain code fence (```go, ```py)
// never counts as a candidate opener.
func xmlStreamFenceBrace(s string, from int) int {
	if from >= len(s) {
		return -1
	}
	pos := from
	for pos < len(s) && (s[pos] == '-' || s[pos] == '_' || s[pos] >= 'a' && s[pos] <= 'z' || s[pos] >= 'A' && s[pos] <= 'Z') {
		pos++ // optional language tag (json, tool_call, tool-call, …)
	}
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n') {
		pos++
	}
	if pos < len(s) && s[pos] == '{' {
		return pos
	}
	return -1
}

// closerEnd returns the index just past the open candidate's closing tag in
// buffered, or -1 while the block is still open.
func (x *XMLToolCallExtractor) closerEnd() int {
	switch x.shape {
	case xmlShapeToolCall, xmlShapeCodebuff, xmlShapeFunctionCall:
		if i := strings.Index(x.buffered, xmlStreamClosers[x.shape]); i >= 0 {
			return i + len(xmlStreamClosers[x.shape])
		}
	case xmlShapePipe:
		if loc := xmlStreamPipeCloseRe.FindStringIndex(x.buffered); loc != nil {
			return loc[1]
		}
	case xmlShapeFence:
		if i := strings.Index(x.buffered[x.fenceBrace:], "```"); i >= 0 {
			return x.fenceBrace + i + 3
		}
	}
	return -1
}

// ToolCallDeltaFragment renders one extracted tool call as a native OpenAI
// streaming delta fragment, ready to append to delta["tool_calls"].
func ToolCallDeltaFragment(index int, tc *toolCall) map[string]any {
	return map[string]any{
		"index": index,
		"id":    tc.ID,
		"type":  tc.Type,
		"function": map[string]any{
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		},
	}
}

// Finish returns the assembled chat.completion response as compact JSON:
// content and reasoning_content are concatenated across chunks, tool_calls
// are stitched by index and sorted, finish_reason is the last non-empty one
// seen ("stop" when none), and usage is the last one seen (zeroed when none).
func (a *Accumulator) Finish() []byte {
	content := strings.Join(a.contentParts, "")
	// Issue #44: fold reasoning into message content for clients that don't
	// render the reasoning channel (same env toggle as the streaming path).
	if tag := reasoningInContentMode(); tag != "" {
		if rc := strings.Join(a.reasoningParts, ""); rc != "" {
			content = "<" + tag + ">" + rc + "</" + tag + ">" + content
		}
	}
	// If native toolCalls are empty, try extracting any inline XML tool calls
	// that were emitted into content (e.g. from Hermes/Qwen/MiMo models).
	if len(a.toolCalls) == 0 {
		var extracted []*toolCall
		content, extracted = extractXMLToolCalls(content)
		if len(extracted) == 0 && len(a.reasoningParts) > 0 {
			// Fallback: model might have emitted tool_call inside reasoning_content (smallcode finding / Tau2 fix)
			reasoningFull := strings.Join(a.reasoningParts, "")
			var cleanedReasoning string
			cleanedReasoning, extracted = extractXMLToolCalls(reasoningFull)
			if len(extracted) > 0 {
				a.reasoningParts = []string{cleanedReasoning}
			}
		}
		for idx, tc := range extracted {
			a.toolCalls[idx] = tc
		}
	}
	// Scrub dangling tool XML tags from content
	content = danglingToolTagsRe.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)

	var msgContent any = content
	if len(a.toolCalls) > 0 && content == "" {
		msgContent = nil
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": msgContent,
		"refusal": nil,
	}
	if len(a.toolCalls) > 0 {
		keys := make([]int, 0, len(a.toolCalls))
		for k := range a.toolCalls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		calls := make([]any, 0, len(keys))
		for _, k := range keys {
			calls = append(calls, a.toolCalls[k])
		}
		msg["tool_calls"] = calls
	}
	if rc := strings.Join(a.reasoningParts, ""); rc != "" {
		msg["reasoning_content"] = rc
	}
	usage := a.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	finish := a.finishReason
	if finish == "" {
		if len(a.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	resp := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"logprobs":      nil,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	if a.systemFingerprint != "" {
		resp["system_fingerprint"] = a.systemFingerprint
	}
	// Values came from encoding/json, so marshaling cannot fail.
	b, _ := json.Marshal(resp)
	return b
}
