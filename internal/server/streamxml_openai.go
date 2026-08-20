package server

// OpenAI-half of the streaming XML tool-call extraction (issue #151):
// streamChatContentToToolCalls feeds relayStream chunks through
// convert.XMLToolCallExtractor and rewrites them as native tool_calls
// fragments. The Anthropic half lives in streamxml_anthropic.go
// (feed/flushAnthropicXMLToolCalls). See convert.XMLToolCallExtractor for
// the incremental parser contract (Feed/Flush per stream).

import (
	"bytes"
	"encoding/json"

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
