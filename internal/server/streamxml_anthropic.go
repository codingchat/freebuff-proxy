package server

// Anthropic-half of the streaming XML tool-call extraction (issue #151):
// feed/flushAnthropicXMLToolCalls drive convert.XMLToolCallExtractor from
// relayAnthropicStream chunks and emit synthetic Anthropic tool_use
// content-block events. The OpenAI half lives in streamxml_openai.go
// (streamChatContentToToolCalls). See convert.XMLToolCallExtractor for the
// incremental parser contract (Feed/Flush per stream).

import (
	"time"

	"freebuff-proxy/internal/convert"
)

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
	// Synthetic fragment indexes must never collide with the chunk's native
	// tool_calls indexes (parity with the OpenAI half): raise the per-stream
	// counter past the max native index present before appending.
	bumpXMLCallIndex(tcs, xmlCallIndex)
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
