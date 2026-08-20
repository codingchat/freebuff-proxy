package server

// Streaming XML tool-call extraction helpers shared by the relay loop
// bodies (chat / responses / Anthropic). Keeping the per-relay wiring
// inline would blow the 1400-line CI budget (chat.go, anthropic.go); the
// extraction logic itself lives here so each relay function stays a thin
// call site. See convert.XMLToolCallExtractor for the incremental parser
// contract (Feed/Flush per stream).

import (
	"bytes"
	"encoding/json"
	"time"

	"freebuff-proxy/internal/convert"
)

// streamChatContentToToolCalls feeds one sanitized chat chunk through the
// stream's XML tool-call extractor and returns the possibly re-encoded
// chunk: withheld block text is removed from delta.content (the key is
// dropped when empty) and completed calls are appended as native tool_calls
// fragments with per-stream sequential indexes so they cannot collide with
// upstream indexes. The proxy-injected end_turn pseudo-tool is never
// relayed (strip parity with the native path). Untouched chunks are
// returned with their exact bytes.
func streamChatContentToToolCalls(clean []byte, xmlExtractor *convert.XMLToolCallExtractor, xmlCallIndex *int, xmlCallsSeen *bool) []byte {
	if !bytes.Contains(clean, []byte(`"content"`)) {
		return clean
	}
	var chunk map[string]any
	if json.Unmarshal(clean, &chunk) != nil {
		return clean
	}
	changed := false
	if rawChoices, ok := chunk["choices"].([]any); ok {
		for _, raw := range rawChoices {
			choice, _ := raw.(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			content, _ := delta["content"].(string)
			if content == "" {
				continue
			}
			text, calls := xmlExtractor.Feed(content)
			if text != content {
				if text == "" {
					delete(delta, "content")
				} else {
					delta["content"] = text
				}
				changed = true
			}
			if len(calls) > 0 {
				tcs, _ := delta["tool_calls"].([]any)
				for _, tc := range calls {
					if tc.Function.Name == "end_turn" {
						continue // strip-parity: never relay the proxy-injected pseudo-tool
					}
					tcs = append(tcs, convert.ToolCallDeltaFragment(*xmlCallIndex, tc))
					*xmlCallIndex++
				}
				if len(tcs) > 0 {
					delta["tool_calls"] = tcs
					*xmlCallsSeen = true
				}
				changed = true
			}
		}
	}
	if !changed {
		return clean
	}
	if reEncoded, err := json.Marshal(chunk); err == nil {
		return reEncoded
	}
	return clean
}

// feedAnthropicXMLToolCalls feeds one upstream content delta through the
// stream's XML tool-call extractor and rewrites the delta in place: withheld
// block text is removed from content (the key is dropped when empty) and any
// completed calls are appended as native tool-call fragments with per-stream
// sequential indexes so they cannot collide with upstream indexes. Existing
// native tool_calls fragments are left untouched. The delta is only rewritten
// when the extractor actually withheld or consumed text.
func feedAnthropicXMLToolCalls(xmlExtractor *convert.XMLToolCallExtractor, chunk map[string]any, xmlCallIndex *int) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok || choice == nil {
		return
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok || delta == nil {
		return
	}
	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return
	}
	text, calls := xmlExtractor.Feed(content)
	if text == content {
		return
	}
	if text == "" {
		delete(delta, "content")
	} else {
		delta["content"] = text
	}
	if len(calls) == 0 {
		return
	}
	tcs, _ := delta["tool_calls"].([]any)
	for _, call := range calls {
		if call.Function.Name == "end_turn" {
			continue // strip-parity: never relay the proxy-injected pseudo-tool
		}
		tcs = append(tcs, convert.ToolCallDeltaFragment(*xmlCallIndex, call))
		*xmlCallIndex++
	}
	if len(tcs) > 0 {
		delta["tool_calls"] = tcs
	}
}

// flushAnthropicXMLToolCalls releases any still-open XML candidate block at
// stream end through the same accumulation path (trailing text and/or native
// tool-call fragments continuing the stream's sequential indexes) so text
// and tool_use blocks emit normally before finalize. No-op when nothing was
// buffered.
func (s *Server) flushAnthropicXMLToolCalls(send func(map[string]any), st *anthropicStreamState, xmlExtractor *convert.XMLToolCallExtractor, xmlCallIndex *int) {
	ft, fc := xmlExtractor.Flush()
	if ft == "" && len(fc) == 0 {
		return
	}
	delta := make(map[string]any)
	if ft != "" {
		delta["content"] = ft
	}
	if len(fc) > 0 {
		tcs := make([]any, 0, len(fc))
		for _, call := range fc {
			if call.Function.Name == "end_turn" {
				continue // strip-parity: never relay the proxy-injected pseudo-tool
			}
			tcs = append(tcs, convert.ToolCallDeltaFragment(*xmlCallIndex, call))
			*xmlCallIndex++
		}
		if len(tcs) > 0 {
			delta["tool_calls"] = tcs
		}
	}
	s.accumulateAnthropicChunk(send, st, map[string]any{
		"id":      "chatcmpl-flush",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   st.model,
		"choices": []any{map[string]any{"delta": delta}},
	})
}
