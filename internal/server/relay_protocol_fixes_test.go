package server

// Regression tests for the P2 relay protocol fixes (review 2026-08-21):
//   1. OpenAI end_turn continuation-fragment drop must run on EVERY
//      tool-bearing chunk, not only chunks containing the "end_turn" string.
//   2. Anthropic streaming finalize must preserve max_tokens (and other)
//      stop reasons instead of unconditionally overriding to tool_use.
//   3. Anthropic streaming block state machine must keep the sequential
//      block lifecycle across interleaved tools/text/thinking fragments.
//   4. Responses relays must surface upstream finish_reason "length" as
//      status "incomplete" with incomplete_details {reason:
//      max_output_tokens} (streaming + non-streaming).
// Plus: Responses SSE frames carry the documented event: field, and the
// transport-error path emits response.failed with the error attached
// (skipping per-item done events). These drive the relays directly with
// scripted SSE readers — no network/timing flakiness.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseFrame is one parsed SSE event (event: field + data JSON).
type sseFrame struct {
	event string
	data  map[string]any
}

// collectSSEFrames parses an SSE body into ordered frames. Comment lines
// (": ...") are ignored; "data: [DONE]" is ignored.
func collectSSEFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	var out []sseFrame
	curEvent := ""
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if jsonStr == "[DONE]" || !strings.HasPrefix(jsonStr, "{") {
				curEvent = ""
				continue
			}
			var dm map[string]any
			if err := json.Unmarshal([]byte(jsonStr), &dm); err != nil {
				t.Fatalf("invalid SSE data line %q: %v", jsonStr, err)
			}
			out = append(out, sseFrame{event: curEvent, data: dm})
			curEvent = ""
		}
	}
	return out
}

// TestRelayStreamEndTurnContinuationDrop pins Fix 1: an arguments-only
// continuation fragment for an already-stripped end_turn index must be
// dropped even though the chunk carries no "end_turn" string. Regression:
// the continuation drop was gated inside the `bytes.Contains(clean,
// "end_turn")` block, so a later nameless fragment (recognizable only by
// index) leaked as a native tool_calls entry.
func TestRelayStreamEndTurnContinuationDrop(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-cd","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_et","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"finish_reason":null}]}`),
		// Arguments-only continuation for index 0: NO "end_turn" string.
		testutilSSE(`{"id":"chatcmpl-cd","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"key\":\"leak\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-cd","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now())
	body := rec.Body.String()

	if strings.Contains(body, `"name":"end_turn"`) {
		t.Errorf("end_turn leaked to client: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, `"tool_calls"`) {
		t.Errorf("continuation fragment for stripped end_turn index leaked as tool_calls: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, "leak") {
		t.Errorf("stripped call's arguments leaked to client: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("terminal finish_reason not rewritten to stop: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("response missing [DONE] terminator")
	}
}

// TestAnthropicStreamMaxTokensStopReasonPreserved pins Fix 2: the streaming
// finalize must mirror the non-streaming path (anthropicMessageFromCompletion)
// — only "end_turn" is promoted to "tool_use" when real tool fragments were
// relayed, and a "tool_use" with zero relayed blocks demotes to "end_turn".
// A max_tokens-truncated stream with partial tool fragments must report
// "max_tokens", NOT "tool_use".
func TestAnthropicStreamMaxTokensStopReasonPreserved(t *testing.T) {
	cases := []struct {
		name         string
		fragments    []string
		finishReason string
		want         string
	}{
		{
			name: "truncated with tool fragments",
			fragments: []string{
				`{"tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}`,
			},
			finishReason: "length",
			want:         "max_tokens",
		},
		{
			name: "stop with tool fragments",
			fragments: []string{
				`{"tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}`,
			},
			finishReason: "stop",
			want:         "tool_use",
		},
		{
			name: "end_turn-only stripped",
			fragments: []string{
				`{"tool_calls":[{"index":0,"id":"call_e","type":"function","function":{"name":"end_turn","arguments":"{}"}}]}`,
			},
			finishReason: "tool_calls",
			want:         "end_turn",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testRelayServer()
			rec := httptest.NewRecorder()
			var sb strings.Builder
			for _, f := range tc.fragments {
				sb.WriteString(testutilSSE(`{"id":"cmpl-l","choices":[{"index":0,"delta":` + f + `,"finish_reason":null}]}`))
			}
			sb.WriteString(testutilSSE(`{"id":"cmpl-l","choices":[{"index":0,"delta":{},"finish_reason":"` + tc.finishReason + `"}]}`))
			s.relayAnthropicStream(context.Background(), rec, strings.NewReader(sb.String()), &relayStats{}, time.Now(), "m", 0)

			stopReason := ""
			for _, ev := range collectSSEFrames(t, rec.Body.String()) {
				if ev.data["type"] == "message_delta" {
					if delta, ok := ev.data["delta"].(map[string]any); ok {
						stopReason, _ = delta["stop_reason"].(string)
					}
				}
			}
			if stopReason != tc.want {
				t.Errorf("message_delta stop_reason = %q, want %q", stopReason, tc.want)
			}
		})
	}
}

// TestAnthropicStreamToolTextToolInterleave pins Fix 3b: when content
// interleaves between tool-call fragments (tools → text → tools-args for the
// same upstream index), the trailing arguments fragment must REOPEN the
// tool_use block at a FRESH index (never a delta against the closed index),
// and the whole block lifecycle must stay strictly sequential.
func TestAnthropicStreamToolTextToolInterleave(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"cmpl-ii","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_bash_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\""}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-ii","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-ii","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-ii","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)
	events := collectSSEFrames(t, rec.Body.String())

	// Walk the events tracking the open block: every start must find no
	// other block open (strictly sequential lifecycle), every input_json_delta
	// must target a started block, and every stop must close a started one.
	open := map[int]bool{}
	var toolStarts []int
	var toolStartData []map[string]any
	var jsonDeltas []struct {
		idx     int
		partial string
	}
	stopReason := ""
	for _, ev := range events {
		idx := -1
		if f, ok := ev.data["index"].(float64); ok {
			idx = int(f)
		}
		switch ev.data["type"] {
		case "content_block_start":
			if len(open) > 0 {
				t.Errorf("content_block_start %d while blocks %v still open (non-sequential)", idx, open)
			}
			if cb, ok := ev.data["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				toolStarts = append(toolStarts, idx)
				toolStartData = append(toolStartData, cb)
			}
			open[idx] = true
		case "content_block_delta":
			d, _ := ev.data["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				if !open[idx] {
					t.Errorf("input_json_delta against closed/unstarted block index %d", idx)
				}
				pj, _ := d["partial_json"].(string)
				jsonDeltas = append(jsonDeltas, struct {
					idx     int
					partial string
				}{idx, pj})
			}
		case "content_block_stop":
			if !open[idx] {
				t.Errorf("content_block_stop for unopen block index %d", idx)
			}
			delete(open, idx)
		case "message_delta":
			if delta, ok := ev.data["delta"].(map[string]any); ok {
				stopReason, _ = delta["stop_reason"].(string)
			}
		}
	}

	if len(toolStarts) != 2 {
		t.Fatalf("tool_use content_block_start count = %d, want 2; starts %v", len(toolStarts), toolStarts)
	}
	first, reopened := toolStarts[0], toolStarts[1]
	if reopened <= first {
		t.Errorf("reopened tool block index %d must be a fresh index above the first %d", reopened, first)
	}
	if len(jsonDeltas) != 2 {
		t.Fatalf("input_json_delta count = %d, want 2; %+v", len(jsonDeltas), jsonDeltas)
	}
	if jsonDeltas[0].idx != first || jsonDeltas[0].partial != `{"cmd":"` {
		t.Errorf("first input_json_delta = %+v, want index %d with the opening args", jsonDeltas[0], first)
	}
	// The trailing args must land in the REOPENED block, preserving them.
	if jsonDeltas[1].idx != reopened || jsonDeltas[1].partial != `ls"}` {
		t.Errorf("trailing args delta = %+v, want index %d with the closing args", jsonDeltas[1], reopened)
	}
	// The reopened block carries the accumulated id/name.
	if name, _ := toolStartData[1]["name"].(string); name != "Bash" {
		t.Errorf("reopened block name = %q, want Bash", name)
	}
	if id, _ := toolStartData[1]["id"].(string); id != "call_bash_1" {
		t.Errorf("reopened block id = %q, want call_bash_1", id)
	}
	if stopReason != "tool_use" {
		t.Errorf("message_delta stop_reason = %q, want tool_use", stopReason)
	}
}

// TestAnthropicStreamThinkingAfterToolClosesToolBlock pins Fix 3a: reasoning
// arriving while a tool_use block is open must close the tool block BEFORE
// the thinking block starts (mirror of ensureText), so a later arguments
// fragment reopens the tool block at a fresh index — never straddling blocks.
func TestAnthropicStreamThinkingAfterToolClosesToolBlock(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()

	ss := strings.Join([]string{
		testutilSSE(`{"id":"cmpl-tt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\""}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-tt","choices":[{"index":0,"delta":{"reasoning_content":"hmm"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-tt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls\"}"}}]},"finish_reason":null}]}`),
		testutilSSE(`{"id":"cmpl-tt","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}, "")

	s.relayAnthropicStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", 0)
	events := collectSSEFrames(t, rec.Body.String())

	open := map[int]string{}
	var seq []string
	var toolStarts []int
	var jsonDeltas []struct {
		idx     int
		partial string
	}
	for _, ev := range events {
		idx := -1
		if f, ok := ev.data["index"].(float64); ok {
			idx = int(f)
		}
		switch ev.data["type"] {
		case "content_block_start":
			if len(open) > 0 {
				t.Errorf("content_block_start %d while blocks %v still open (non-sequential)", idx, open)
			}
			cb, _ := ev.data["content_block"].(map[string]any)
			typ, _ := cb["type"].(string)
			seq = append(seq, typ+"-start-"+fmt.Sprint(idx))
			if typ == "tool_use" {
				toolStarts = append(toolStarts, idx)
			}
			open[idx] = typ
		case "content_block_delta":
			d, _ := ev.data["delta"].(map[string]any)
			if d != nil && d["type"] == "input_json_delta" {
				if open[idx] == "" {
					t.Errorf("input_json_delta against closed/unstarted block index %d", idx)
				}
				pj, _ := d["partial_json"].(string)
				jsonDeltas = append(jsonDeltas, struct {
					idx     int
					partial string
				}{idx, pj})
			}
		case "content_block_stop":
			if open[idx] == "" {
				t.Errorf("content_block_stop for unopen block index %d", idx)
			}
			seq = append(seq, open[idx]+"-stop-"+fmt.Sprint(idx))
			delete(open, idx)
		}
	}

	// tool block closed (stop) before the thinking block started (start).
	tsPos, tnPos := -1, -1
	for i, s := range seq {
		if strings.HasPrefix(s, "tool_use-stop") && tsPos < 0 {
			tsPos = i
		}
		if strings.HasPrefix(s, "thinking-start") && tnPos < 0 {
			tnPos = i
		}
	}
	if tsPos < 0 || tnPos < 0 {
		t.Fatalf("missing lifecycle markers in %v", seq)
	}
	if tsPos > tnPos {
		t.Errorf("tool_use block stopped at %d after thinking started at %d: %v", tsPos, tnPos, seq)
	}
	if len(toolStarts) != 2 {
		t.Fatalf("tool_use content_block_start count = %d, want 2 (reopen); starts %v", len(toolStarts), toolStarts)
	}
	if toolStarts[1] <= toolStarts[0] {
		t.Errorf("reopened tool block index %d must exceed the first %d", toolStarts[1], toolStarts[0])
	}
	if len(jsonDeltas) != 2 || jsonDeltas[1].idx != toolStarts[1] || jsonDeltas[1].partial != `ls"}` {
		t.Errorf("trailing args delta = %+v, want index %d with the closing args", jsonDeltas, toolStarts[1])
	}
}

// TestResponsesStreamIncompleteOnLength pins Fix 4 (streaming): an upstream
// finish_reason "length" must surface as status "incomplete" with
// incomplete_details {reason: max_output_tokens} on the terminal
// response.completed; "stop"/"tool_calls" stay "completed".
func TestResponsesStreamIncompleteOnLength(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		wantStatus   string
		wantIncompl  bool
	}{
		{"truncated", "length", "incomplete", true},
		{"stop", "stop", "completed", false},
		{"tool_calls", "tool_calls", "completed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testRelayServer()
			rec := httptest.NewRecorder()
			ss := strings.Join([]string{
				testutilSSE(`{"id":"chatcmpl-ln","choices":[{"index":0,"delta":{"content":"partial output"},"finish_reason":null}]}`),
				testutilSSE(`{"id":"chatcmpl-ln","choices":[{"index":0,"delta":{},"finish_reason":"` + tc.finishReason + `"}]}`),
			}, "")
			s.relayResponsesStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_test")

			var resp map[string]any
			found := false
			for _, ev := range collectSSEFrames(t, rec.Body.String()) {
				if ev.data["type"] == "response.completed" {
					resp, _ = ev.data["response"].(map[string]any)
					found = true
				}
			}
			if !found {
				t.Fatal("response.completed not emitted")
			}
			if got, _ := resp["status"].(string); got != tc.wantStatus {
				t.Errorf("status = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantIncompl {
				details, _ := resp["incomplete_details"].(map[string]any)
				if details == nil || details["reason"] != "max_output_tokens" {
					t.Errorf("incomplete_details = %v, want {reason: max_output_tokens}", resp["incomplete_details"])
				}
			} else if resp["incomplete_details"] != nil {
				t.Errorf("incomplete_details = %v, want nil for completed", resp["incomplete_details"])
			}
		})
	}
}

// TestResponsesJSONLengthIncomplete pins Fix 4 (non-streaming): the JSON
// builder must mirror the streaming path — finish_reason "length" → status
// "incomplete" + incomplete_details.
func TestResponsesJSONLengthIncomplete(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := strings.Join([]string{
		testutilSSE(`{"id":"chatcmpl-jl","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`),
		testutilSSE(`{"id":"chatcmpl-jl","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
	}, "")
	s.relayResponsesJSON(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_jl")

	var out struct {
		Status            string         `json:"status"`
		IncompleteDetails map[string]any `json:"incomplete_details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, truncateStr(rec.Body.String(), 400))
	}
	if out.Status != "incomplete" {
		t.Errorf("status = %q, want incomplete", out.Status)
	}
	if out.IncompleteDetails == nil || out.IncompleteDetails["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details = %v, want {reason: max_output_tokens}", out.IncompleteDetails)
	}
}

// TestResponsesStreamEventField pins the SSE frame shape: every Responses
// frame carries the documented `event: <type>` field (like the Anthropic
// relay) ahead of the data line.
func TestResponsesStreamEventField(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	ss := testutilSSE(`{"id":"chatcmpl-ev","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		testutilSSE(`{"id":"chatcmpl-ev","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	s.relayResponsesStream(context.Background(), rec, strings.NewReader(ss), &relayStats{}, time.Now(), "m", "resp_ev")
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created\n",
		"event: response.in_progress\n",
		"event: response.output_text.delta\n",
		"event: response.completed\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q frame", want)
		}
	}
}

// TestResponsesStreamTransportErrorFailed pins the transport-error path: the
// upstream error is threaded into response.failed's error object (type +
// message) and NO per-item done events are emitted on the failure path (the
// partial item stays in_progress).
func TestResponsesStreamTransportErrorFailed(t *testing.T) {
	s := testRelayServer()
	rec := httptest.NewRecorder()
	s.relayResponsesStream(context.Background(), rec, &errAfterLineReader{}, &relayStats{}, time.Now(), "m", "resp_err")
	body := rec.Body.String()

	if !strings.Contains(body, "event: response.failed\n") {
		t.Errorf("stream missing response.failed frame: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"type":"upstream_stream_error"`) {
		t.Errorf("response.failed missing error type: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, "connection reset by peer") {
		t.Errorf("response.failed missing upstream error message: %q", truncateStr(body, 400))
	}
	if !strings.Contains(body, `"status":"failed"`) {
		t.Errorf("response.failed missing status failed: %q", truncateStr(body, 400))
	}
	// The partial item was started (output_item.added) but must NOT be
	// completed: done events are skipped on the failure path.
	if !strings.Contains(body, "response.output_item.added") {
		t.Errorf("partial item not started: %q", truncateStr(body, 400))
	}
	if strings.Contains(body, "response.output_item.done") {
		t.Errorf("done events emitted on the failure path: %q", truncateStr(body, 400))
	}
}
