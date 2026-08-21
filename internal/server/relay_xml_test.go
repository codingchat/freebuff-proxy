package server

// Streaming XML tool-call extraction tests (Accumulator.Finish parity, but
// for the streaming relay): models like MiMo/Hermes/Qwen emit tool calls
// as <tool_call>/<codebuff_tool_call>/<function_call>/pipe/fenced blocks
// inside delta.content instead of native delta.tool_calls. The relay must
// convert them into native tool_calls fragments, withhold the XML from the
// client, and flip the terminal finish_reason to "tool_calls". These drive
// relayStream directly with scripted SSE readers like the other
// relay_internal tests — no network/timing flakiness.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRelayStreamXMLToolCallSplitBlock feeds a <tool_call> block SPLIT
// across three SSE content deltas: the relay must relay the surrounding
// text, withhold the XML block, emit one native tool_calls fragment (index
// 0, function bash, arguments containing "pwd"), and end with
// finish_reason "tool_calls" (flipped from the upstream's "stop").
func TestRelayStreamXMLToolCallSplitBlock(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-xm","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Let me check:"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-xm","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call>"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-xm","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<function=bash><parameter=command>pwd</parameter></function>"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-xm","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"</tool_call>"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-xm","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"done\n"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-xm","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	// The XML must never reach the client (withheld while the block is open).
	if strings.Contains(body, "<tool_call>") {
		t.Errorf("response leaks XML tool-call tag: %q", truncateStr(body, 400))
	}
	// Surrounding text survives verbatim.
	if !strings.Contains(body, "Let me check:") {
		t.Errorf("response missing leading text: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `done\n`) {
		t.Errorf("response missing trailing text: %q", truncateStr(body, 400))
	}
	// Native tool_calls fragment carrying the extracted call (index 0,
	// sequential synthetic index, function bash, arguments with pwd).
	if !strings.Contains(body, `"tool_calls"`) {
		t.Errorf("response missing native tool_calls fragment: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"index":0`) {
		t.Errorf("response tool_calls missing synthetic index 0: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"name":"bash"`) {
		t.Errorf("response tool_calls missing function name bash: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "pwd") {
		t.Errorf("response tool_calls arguments missing pwd: %q", truncateStr(body, 400))
	}
	// Terminal finish_reason flipped from upstream "stop" to "tool_calls".
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("response missing finish_reason tool_calls: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("terminal finish_reason not flipped from stop: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("response missing [DONE] terminator")
	}
}

// TestRelayStreamXMLToolCallFlushUnclosed verifies the end-of-stream Flush:
// a content fragment that opens but never closes a <tool_call> block still
// reaches the client through the synthetic flush chunk, with dangling tags
// scrubbed and no XML leaking into the wire.
func TestRelayStreamXMLToolCallFlushUnclosed(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := testutilSSE(`{"id":"chatcmpl-fl","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call><function=bash><parameter=command>pwd</parameter></function>"},"finish_reason":null}]}`)

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if strings.Contains(body, "<tool_call>") {
		t.Errorf("response leaks XML tool-call tag: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, "<function") || strings.Contains(body, "<parameter") {
		t.Errorf("response leaks dangling XML tags: %q", truncateStr(body, 400))
	}
	// The flushed text (tags scrubbed) is relayed as a synthetic chunk.
	if !strings.Contains(body, `"content":"pwd"`) {
		t.Errorf("response missing flushed content with scrubbed tags: %q", truncateStr(body, 400))
	}
	// The flush chunk reuses the stream's last seen id.
	if !strings.Contains(body, `"id":"chatcmpl-fl"`) {
		t.Errorf("response missing synthetic chunk with stream id: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("response missing [DONE] terminator")
	}
}

// TestRelayStreamXMLToolCallEndTurnGuard pins the strip-parity guard: an
// XML block that extracts a call named end_turn (the proxy-injected
// pseudo-tool that clients never declare) must NOT be relayed as a native
// tool_calls fragment, and the terminal finish_reason must stay "stop".
func TestRelayStreamXMLToolCallEndTurnGuard(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-et","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call><function=end_turn></function></tool_call>"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-et","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if strings.Contains(body, `"name":"end_turn"`) {
		t.Errorf("end_turn leaked to client: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Errorf("end_turn-only stream emitted tool_calls: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason should stay stop: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, "<tool_call>") {
		t.Errorf("response leaks XML tool-call tag: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("response missing [DONE] terminator")
	}
}

// TestRelayStreamEndTurnOnlyNative pins the end_turn-only translation for
// NATIVE tool calls: an upstream stream whose only tool call is the
// proxy-injected end_turn pseudo-tool (name-bearing fragment + terminal
// finish_reason "tool_calls") must reach the client with zero tool_calls and
// a terminal finish_reason "stop". Regression: index tracking previously ran
// AFTER StripEndTurnToolCalls, so endTurnCallIndexes stayed empty and the
// finish_reason rewrite never fired.
func TestRelayStreamEndTurnOnlyNative(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-et","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_et","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-et","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if strings.Contains(body, `"name":"end_turn"`) {
		t.Errorf("end_turn leaked to client: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Errorf("end_turn-only stream emitted tool_calls: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("terminal finish_reason not rewritten to stop: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("terminal finish_reason still tool_calls: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("response missing [DONE] terminator")
	}
}

// TestRelayStreamRealToolCallWithEndTurn pins mixed-stream behavior: a real
// native tool call alongside the injected end_turn must be relayed with
// finish_reason "tool_calls" — the end_turn-only rewrite must not fire once
// any real call was seen (seenRealToolCalls is tracked from every chunk,
// not just chunks containing the end_turn name).
func TestRelayStreamRealToolCallWithEndTurn(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-mx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_w","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-mx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_et","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-mx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if !strings.Contains(body, `"name":"get_weather"`) {
		t.Errorf("real tool call not relayed: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"name":"end_turn"`) {
		t.Errorf("end_turn leaked to client: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason should stay tool_calls: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason incorrectly rewritten to stop: %q", truncateStr(body, 400))
	}
}

// TestRelayStreamXMLCallBeatsEndTurnOrdering pins the rewrite ORDER: the
// end_turn-only flip runs before the XML-extracted-call flip, so a stream
// with a real extracted XML call AND a native end_turn terminates with
// finish_reason "tool_calls" (the extracted call wins), not "stop".
func TestRelayStreamXMLCallBeatsEndTurnOrdering(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-od","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call><function=bash><parameter=command>pwd</parameter></function></tool_call>"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-od","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_et","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-od","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if !strings.Contains(body, `"name":"bash"`) {
		t.Errorf("extracted XML call not relayed: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"name":"end_turn"`) {
		t.Errorf("end_turn leaked to client: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("extracted call should win the finish_reason flip: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason rewritten to stop despite extracted call: %q", truncateStr(body, 400))
	}
}

// TestRelayStreamXMLIndexPastNative pins synthetic-index collision avoidance:
// when a chunk carries both a native tool call (index 3) and a completed XML
// block, the extracted fragment must be appended at index 4 — never reusing
// a native index (and the floor persists for later synthetics via the
// per-stream pointer).
func TestRelayStreamXMLIndexPastNative(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-ix","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call><function=bash><parameter=command>pwd</parameter></function></tool_call>","tool_calls":[{"index":3,"id":"call_n","type":"function","function":{"name":"search","arguments":"{}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-ix","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if !strings.Contains(body, `"index":3`) {
		t.Errorf("native tool call index 3 missing: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"index":4`) {
		t.Errorf("synthetic index should start at 4 past native 3: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"name":"bash"`) {
		t.Errorf("extracted XML call not relayed: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"name":"search"`) {
		t.Errorf("native tool call not relayed: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason should be tool_calls (extracted call): %q", truncateStr(body, 400))
	}
}

// TestRelayJSONEndTurnOnly pins non-streaming parity: an upstream response
// whose only tool call is end_turn must come back with finish_reason "stop"
// and no tool_calls (the same end_turn-only translation as the streaming
// path).
func TestRelayJSONEndTurnOnly(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-nj","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_et","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-nj","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		testutilSSE(`{"id":"chatcmpl-nj","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
	}, "")

	s.relayJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())

	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, truncateStr(rec.Body.String(), 400))
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if len(out.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("tool_calls = %d, want 0 (end_turn stripped)", len(out.Choices[0].Message.ToolCalls))
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
}

// TestRelayJSONXMLExtractedCallFinishFlip pins non-streaming finish_reason
// parity for extracted XML calls: the accumulator extracts the call into
// message.tool_calls with upstream finish "stop", and the relay must flip it
// to "tool_calls" so clients see a complete tool-call turn (mirroring the
// streaming XML flip).
func TestRelayJSONXMLExtractedCallFinishFlip(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-nx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"<tool_call><function=bash><parameter=command>pwd</parameter></function></tool_call>"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-nx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		testutilSSE(`{"id":"chatcmpl-nx","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
	}, "")

	s.relayJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())

	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, truncateStr(rec.Body.String(), 400))
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	tcs := out.Choices[0].Message.ToolCalls
	if len(tcs) != 1 || tcs[0].Function.Name != "bash" {
		t.Errorf("tool_calls = %+v, want one extracted bash call", tcs)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls (extracted call)", out.Choices[0].FinishReason)
	}
}
