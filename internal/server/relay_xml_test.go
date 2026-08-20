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
