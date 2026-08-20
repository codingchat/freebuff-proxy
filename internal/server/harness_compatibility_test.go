package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// TestAnthropicClaudeCodeStreamingSequence tests the exact Anthropic SSE event sequence
// expected by Claude Code CLI and Anthropic SDK:
// message_start -> content_block_start (thinking) -> content_block_delta (thinking_delta) ->
// signature_delta -> content_block_stop -> content_block_start (tool_use) ->
// content_block_delta (input_json_delta) -> content_block_stop -> message_delta -> message_stop.
// Also verifies end_turn is completely stripped and never leaked as a tool_use block.
func TestAnthropicClaudeCodeStreamingSequence(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 1. Thinking delta
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"reasoning_content":"Thinking about files...","role":"assistant"},"index":0}]}`+"\n\n")
		// 2. Real tool call + injected end_turn pseudo tool call from upstream
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bash_123","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_end_turn_999","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"index":0}]}`+"\n\n")
		// 3. Final chunk with finish_reason and usage
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-1","choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"usage":{"prompt_tokens":150,"completion_tokens":45,"total_tokens":195}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServerCfg(t, []string{"anthropic-test-key"}, nil, mock)

	reqBody := `{
		"model": "z-ai/glm-5.2",
		"messages": [
			{"role": "user", "content": "List files in directory"}
		],
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 2048}
	}`

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-api-key", "anthropic-test-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	verHeader := resp.Header.Get("anthropic-version")
	if verHeader != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", verHeader)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(bodyBytes), "\n")

	var events []string
	var dataLines []map[string]any
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "event:") {
			events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
		if strings.HasPrefix(line, "data:") {
			jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var dataMap map[string]any
			if err := json.Unmarshal([]byte(jsonStr), &dataMap); err == nil {
				dataLines = append(dataLines, dataMap)
			}
		}
	}

	// Verify events sequence
	if len(events) == 0 {
		t.Fatalf("no SSE events emitted: %s", string(bodyBytes))
	}

	// Verify message_start event has input_tokens > 0
	var foundMessageStart, foundToolUse, foundEndTurnToolUse, foundMessageDelta, foundMessageStop bool
	for _, dm := range dataLines {
		evType, _ := dm["type"].(string)
		switch evType {
		case "message_start":
			foundMessageStart = true
			if msg, ok := dm["message"].(map[string]any); ok {
				if usage, ok := msg["usage"].(map[string]any); ok {
					if inToks, ok := usage["input_tokens"].(float64); !ok || inToks <= 0 {
						t.Errorf("message_start usage.input_tokens = %v, want > 0", usage["input_tokens"])
					}
				} else {
					t.Errorf("message_start missing usage map")
				}
			}
		case "content_block_start":
			if cb, ok := dm["content_block"].(map[string]any); ok {
				cbType, _ := cb["type"].(string)
				if cbType == "tool_use" {
					name, _ := cb["name"].(string)
					if name == "end_turn" {
						foundEndTurnToolUse = true
					}
					if name == "Bash" {
						foundToolUse = true
					}
				}
			}
		case "message_delta":
			foundMessageDelta = true
			if delta, ok := dm["delta"].(map[string]any); ok {
				stopReason, _ := delta["stop_reason"].(string)
				if stopReason != "tool_use" {
					t.Errorf("message_delta stop_reason = %q, want tool_use", stopReason)
				}
			}
		case "message_stop":
			foundMessageStop = true
		}
	}

	if !foundMessageStart {
		t.Errorf("message_start event not found")
	}
	if !foundToolUse {
		t.Errorf("tool_use content_block_start for Bash not found")
	}
	if foundEndTurnToolUse {
		t.Errorf("end_turn pseudo-tool was leaked to Anthropic client in content_block_start")
	}
	if !foundMessageDelta {
		t.Errorf("message_delta event not found")
	}
	if !foundMessageStop {
		t.Errorf("message_stop event not found")
	}
}

// TestAnthropicNonStreamingMessage verifies non-streaming /v1/messages formatting,
// including thinking signature, end_turn stripping, and usage integers.
func TestAnthropicNonStreamingMessage(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{"reasoning_content":"Let's check tools.","role":"assistant"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"foo.go\"}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_end_2","type":"function","function":{"name":"end_turn","arguments":"{}"}}]},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-2","choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "z-ai/glm-5.2",
		"messages": [{"role": "user", "content": "Read foo.go"}],
		"stream": false
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}

	if msg["type"] != "message" {
		t.Errorf("type = %v, want message", msg["type"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}

	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content missing or empty: %v", msg["content"])
	}

	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		bType, _ := block["type"].(string)
		if bType == "thinking" {
			if _, hasSig := block["signature"]; !hasSig {
				t.Errorf("thinking block missing required signature field")
			}
		}
		if bType == "tool_use" {
			name, _ := block["name"].(string)
			if name == "end_turn" {
				t.Errorf("end_turn pseudo-tool leaked in non-streaming content block")
			}
			if name != "Read" {
				t.Errorf("unexpected tool name %q, want Read", name)
			}
		}
	}
}

// TestOpenAIMultiTurnSchemaCompliance tests OpenAI chat completions non-streaming schema:
// logprobs: null, refusal: null, tool_calls format.
func TestOpenAIMultiTurnSchemaCompliance(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-3","choices":[{"delta":{"content":"Hello world!","role":"assistant"},"index":0}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-3","choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}

	ts, _ := newTestServer(t, nil, mock)

	reqBody := `{
		"model": "z-ai/glm-5.2",
		"messages": [{"role": "user", "content": "Hi"}],
		"functions": [{"name": "get_weather", "parameters": {"type": "object", "properties": {"loc": {"type": "string"}}}}],
		"function_call": "auto"
	}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, string(body))
	}

	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}

	choices, ok := res["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("choices missing: %v", res)
	}
	choice := choices[0].(map[string]any)
	if _, hasLogprobs := choice["logprobs"]; !hasLogprobs {
		t.Errorf("choices[0] missing required logprobs field")
	}
	msg := choice["message"].(map[string]any)
	if _, hasRefusal := msg["refusal"]; !hasRefusal {
		t.Errorf("choices[0].message missing required refusal field")
	}
}

// TestOpenAIModelRetrieveEndpoint tests GET /v1/models/{model}
func TestOpenAIModelRetrieveEndpoint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ts, _ := newTestServer(t, nil, mock)

	// Existing model
	resp, err := http.Get(ts.URL + "/v1/models/z-ai/glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var modelObj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&modelObj); err != nil {
		t.Fatal(err)
	}
	if modelObj["id"] != "z-ai/glm-5.2" || modelObj["object"] != "model" {
		t.Errorf("modelObj = %v, want id=z-ai/glm-5.2, object=model", modelObj)
	}

	// Unknown model -> 404
	resp404, err := http.Get(ts.URL + "/v1/models/unknown-model-xyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp404.Body.Close() }()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp404.StatusCode)
	}
}

// TestAnthropicErrorEnvelopeStructure tests error response formatting for Anthropic requests.
func TestAnthropicErrorEnvelopeStructure(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ts, _ := newTestServer(t, nil, mock)

	// Empty messages array -> 400 with Anthropic error envelope
	reqBody := `{"model": "z-ai/glm-5.2", "messages": []}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ver := resp.Header.Get("anthropic-version"); ver != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", ver)
	}

	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Type != "error" {
		t.Errorf("envelope type = %q, want error", errResp.Type)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("inner error type = %q, want invalid_request_error", errResp.Error.Type)
	}
}

// TestBridgeModeAnthropicAndOpenAI verifies that when AUTH_TOKENS is empty (pure bridge mode),
// clients can authenticate using anthropic-api-key, x-api-key, or Authorization: Bearer,
// and the client's token is dynamically leased and relayed upstream.
func TestBridgeModeAnthropicAndOpenAI(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var seenTokens []string
	mock.ChatHandler = func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seenTokens = append(seenTokens, strings.TrimPrefix(auth, "Bearer "))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+`{"id":"cmpl-b","choices":[{"delta":{"content":"ok"},"index":0,"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	ts, _ := newBridgeTestServer(t, mock)

	// 1. Anthropic endpoint with anthropic-api-key
	req1, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("anthropic-api-key", "token-anthropic-key")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp1.Body.Close() }()
	if resp1.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("anthropic-api-key status = %d, want 200: %s", resp1.StatusCode, string(body))
	}

	// 2. Anthropic endpoint with x-api-key
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", "token-x-key")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("x-api-key status = %d, want 200: %s", resp2.StatusCode, string(body))
	}

	// 3. OpenAI endpoint with Authorization: Bearer
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer token-bearer-key")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("bearer token status = %d, want 200: %s", resp3.StatusCode, string(body))
	}

	// 4. Missing token -> 401
	req4, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req4.Header.Set("Content-Type", "application/json")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp4.Body.Close() }()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp4.StatusCode)
	}

	// Verify tokens were relayed upstream
	if len(seenTokens) != 3 {
		t.Fatalf("seen tokens count = %d, want 3", len(seenTokens))
	}
	if seenTokens[0] != "token-anthropic-key" || seenTokens[1] != "token-x-key" || seenTokens[2] != "token-bearer-key" {
		t.Errorf("seen tokens = %v, want [token-anthropic-key, token-x-key, token-bearer-key]", seenTokens)
	}
}
