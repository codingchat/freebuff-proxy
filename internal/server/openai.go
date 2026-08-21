package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
)

// registerOpenAIRoutes wires the OpenAI-compatible surface (chat
// completions, Responses, embeddings, model catalog) onto the mux. The
// Anthropic-compatible surface registers separately (registerAnthropicRoutes,
// anthropic.go); shared routes (healthz/metrics/admin) live in
// server.go's Handler.
func (s *Server) registerOpenAIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", s.requireAuth(s.handleChat))
	mux.HandleFunc("POST /v1/responses", s.requireAuth(s.handleResponses))
	mux.HandleFunc("POST /v1/embeddings", s.requireAuth(s.handleEmbeddings))
	mux.HandleFunc("GET /v1/models", s.requireAuth(s.handleModels))
	mux.HandleFunc("GET /v1/models/{model...}", s.requireAuth(s.handleModelRetrieve))
}

// handleChat is the OpenAI chat-completions entry point: sanitize the
// request, then route through chatCore with the chat wire format.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "invalid_request_error", "content_too_large", 0)
		} else {
			s.writeJSONError(w, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		}
		return
	}

	// The raw map decides the response mode (stream) before sanitization.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeJSONError(w, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.servedModels(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	if !s.modelAllowed(model) {
		// MODELS_ALLOW: the resolved model (alias + -max upgrade applied)
		// is outside the operator allowlist — reject like an unknown model.
		s.writeJSONError(w, http.StatusNotFound,
			"model not allowed by MODELS_ALLOW", "invalid_request_error", "model_not_found", 0)
		return
	}
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	normalized, err := convert.NormalizeRequest(body, model)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var relay relayFunc
	if stream {
		relay = s.relayStream
	} else {
		relay = s.relayJSON
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "chat", relay)
}

// relayStream forwards sanitized upstream SSE lines to the client with
// per-chunk flushing, a ": connecting" grace-flush comment, a keepalive
// comment every keepaliveInterval of relay silence, a [DONE] terminator,
// and an error chunk (then DONE) when the upstream stream dies while the
// client context is still live.
func (s *Server) relayStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	w.WriteHeader(http.StatusOK)

	// The official CLI client treats a ": connecting" comment as the signal
	// that headers have flushed and the stream is live (grace flush): write
	// it before relaying anything so a client-side timeout can never fire
	// during a long upstream admission pause. Comment frames are ignored by
	// SSE parsers.
	_, _ = io.WriteString(w, ": connecting\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	lines := make(chan lineChunk)
	go relayReadLoop(ctx, r, lines)

	// lastWrite is when the relay last wrote a frame to the CLIENT. The
	// keepalive condition keys on it so a liveness signal is emitted after
	// any client-write silence, even when upstream comment/junk lines keep
	// arriving (they are dropped, never relayed, and must not count as
	// liveness — #161).
	lastWrite := time.Now()
	first := true
	var reasoningParts []string
	var contentParts []string
	var streamModel string
	toolIDsMap := make(map[string]bool)
	var toolIDs []string
	endTurnCallIndexes := make(map[int]bool)
	seenRealToolCalls := false
	// Streaming XML tool-call extraction: models like MiMo/Hermes/Qwen
	// emit tool calls as XML/fenced blocks inside delta.content instead of
	// native delta.tool_calls. One extractor per stream (not concurrency
	// safe); extracted calls are relayed as native fragments with
	// sequential synthetic indexes so they can never collide with native
	// tool_calls fragments.
	xmlExtractor := &convert.XMLToolCallExtractor{}
	xmlCallIndex := 0
	xmlCallsSeen := false
	xmlStreamID := ""

	// emitXMLFlush releases anything the XML extractor still holds at
	// stream end (Flush) as one synthetic chunk through the normal frame
	// path: completed calls inside a never-closed block still become native
	// fragments, and dangling tags are scrubbed from the released text.
	emitXMLFlush := func() {
		ft, fc := xmlExtractor.Flush()
		if len(ft) == 0 && len(fc) == 0 {
			return
		}
		delta := make(map[string]any, 2)
		if len(ft) > 0 {
			delta["content"] = ft
			contentParts = append(contentParts, ft)
		}
		if len(fc) > 0 {
			tcs := make([]any, 0, len(fc))
			for _, tc := range fc {
				if tc.Function.Name == "end_turn" {
					continue // never relay the proxy-injected pseudo-tool
				}
				tcs = append(tcs, convert.ToolCallDeltaFragment(xmlCallIndex, tc))
				xmlCallIndex++
			}
			if len(tcs) > 0 {
				delta["tool_calls"] = tcs
				xmlCallsSeen = true
			}
		}
		streamID := xmlStreamID
		if streamID == "" {
			streamID = "chatcmpl-flush"
		}
		chunk := map[string]any{
			"id":      streamID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   streamModel,
			"choices": []any{map[string]any{"index": 0, "delta": delta}},
		}
		if b, err := json.Marshal(chunk); err == nil {
			frame := convert.EncodeSSE(b)
			if _, err := w.Write(frame); err != nil {
				s.logger.Debug("stream write failed", "err", err)
				return
			}
			stats.chunks++
			stats.bytes += len(frame)
			lastWrite = time.Now()
			flusher.Flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if time.Since(lastWrite) >= keepaliveInterval {
				_, _ = io.WriteString(w, ": keepalive\n\n")
				lastWrite = time.Now()
				flusher.Flush()
			}
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					emitXMLFlush()
					s.logger.Warn("upstream stream error", "err", lc.err)
					_, _ = w.Write(convert.ErrorChunk("upstream stream interrupted: "+lc.err.Error(), "upstream_stream_error"))
					_, _ = w.Write(convert.DONE)
					flusher.Flush()
				}
				s.ingestStreamReasoning(streamModel, reasoningParts, contentParts, toolIDs)
				return
			}
			if lc.done {
				// Clean end of stream (EOF is not a scanner error).
				emitXMLFlush()
				_, _ = w.Write(convert.DONE)
				flusher.Flush()
				s.ingestStreamReasoning(streamModel, reasoningParts, contentParts, toolIDs)
				return
			}
			clean, drop := convert.SanitizeChunk(lc.line)
			if drop {
				// Non-chunk lines (upstream comments, junk frames) are never
				// relayed and must NOT advance the keepalive timer: the
				// client sees only real frames, so a steady dribble of
				// upstream comments would starve it of liveness signals
				// indefinitely (#161).
				continue
			}
			// Track tool-call indexes BEFORE any strip: StripEndTurnToolCalls
			// deletes end_turn entries, so tracking after the strip would
			// never see the name (the recorded indexes feed the
			// continuation-fragment drop below, and any real named call
			// flips seenRealToolCalls so the terminal finish_reason rewrite
			// never downgrades a genuine tool-call turn).
			trackToolCallIndexes(clean, endTurnCallIndexes, &seenRealToolCalls)
			// Strip Codebuff end_turn pseudo-tool-calls (issue #140).
			// The proxy injects end_turn into every upstream request to pass
			// foreign_toolset validation; the model calls it when done. We
			// must not relay it to clients that never declared it.
			if bytes.Contains(clean, []byte(`"end_turn"`)) {
				var chunk map[string]any
				if json.Unmarshal(clean, &chunk) == nil {
					convert.StripEndTurnToolCalls(chunk)
					// Drop continuation fragments for already-stripped end_turn indexes.
					if len(endTurnCallIndexes) > 0 {
						if choices, ok := chunk["choices"].([]any); ok {
							for _, raw := range choices {
								choice, _ := raw.(map[string]any)
								if choice == nil {
									continue
								}
								delta, _ := choice["delta"].(map[string]any)
								if delta == nil {
									continue
								}
								tcs, _ := delta["tool_calls"].([]any)
								if len(tcs) == 0 {
									continue
								}
								filtered := make([]any, 0, len(tcs))
								for _, tc := range tcs {
									tcMap, _ := tc.(map[string]any)
									if tcMap == nil {
										filtered = append(filtered, tc)
										continue
									}
									idx := 0
									if i, ok := tcMap["index"].(float64); ok {
										idx = int(i)
									}
									if endTurnCallIndexes[idx] {
										continue // drop continuation fragment
									}
									filtered = append(filtered, tc)
								}
								if len(filtered) == 0 {
									delete(delta, "tool_calls")
								} else {
									delta["tool_calls"] = filtered
								}
							}
						}
					}
					reEncoded, err := json.Marshal(chunk)
					if err == nil {
						clean = reEncoded
					}
				}
			}
			// Extract XML-embedded tool calls from delta.content (streaming
			// parity with the accumulator's Finish): feed each content
			// fragment through the extractor, withhold text inside a
			// candidate block, and relay completed calls as native
			// tool_calls fragments appended after any native ones. Only
			// re-marshal when the chunk actually changed so untouched
			// frames keep their exact bytes.
			clean = streamChatContentToToolCalls(clean, xmlExtractor, &xmlCallIndex, &xmlCallsSeen)
			// Rewrite finish_reason for the terminal chunk when ALL tool calls
			// in this stream were end_turn. The terminal chunk carries no
			// "end_turn" string (only finish_reason: "tool_calls"), so the
			// block above is skipped. Without this, finish_reason: "tool_calls"
			// leaks to clients that never declared end_turn.
			if !seenRealToolCalls && len(endTurnCallIndexes) > 0 {
				if bytes.Contains(clean, []byte(`"finish_reason":"tool_calls"`)) {
					var chunk map[string]any
					if json.Unmarshal(clean, &chunk) == nil {
						if rawChoices, ok := chunk["choices"].([]any); ok {
							for _, raw := range rawChoices {
								if choice, ok := raw.(map[string]any); ok {
									if fr, ok := choice["finish_reason"].(string); ok && fr == "tool_calls" {
										choice["finish_reason"] = "stop"
									}
								}
							}
						}
						if b, err := json.Marshal(chunk); err == nil {
							clean = b
						}
					}
				}
			}
			// finish_reason parity for extracted XML calls: upstream models
			// that emit XML tool calls in content terminate with
			// finish_reason: "stop" (they never emit native tool_calls).
			// Flip it so clients see a complete tool-call turn. This runs
			// after the end_turn rewrite above, so an extracted call wins
			// over the end_turn-only flip (a "tool_calls" rewritten to
			// "stop" becomes "tool_calls" again).
			if xmlCallsSeen && bytes.Contains(clean, []byte(`"finish_reason":"stop"`)) {
				var chunk map[string]any
				if json.Unmarshal(clean, &chunk) == nil {
					if rawChoices, ok := chunk["choices"].([]any); ok {
						for _, raw := range rawChoices {
							if choice, ok := raw.(map[string]any); ok {
								if fr, ok := choice["finish_reason"].(string); ok && fr == "stop" {
									choice["finish_reason"] = "tool_calls"
								}
							}
						}
					}
					if b, err := json.Marshal(chunk); err == nil {
						clean = b
					}
				}
			}
			// The final chunk carries the usage block (or a usage-only
			// chunk when stream_options.include_usage is set); capture its
			// total for the spend ledger (#122). Cheap substring probe, so
			// the per-chunk path only pays for an unmarshal on the usage
			// chunk itself.
			if bytes.Contains(clean, []byte(`"usage"`)) {
				var u struct {
					Usage any `json:"usage"`
				}
				// Only adopt the total when the chunk actually carries a
				// usage block: a trailing "usage":null or a content chunk
				// merely mentioning the word must not zero the ledger.
				if json.Unmarshal(clean, &u) == nil && u.Usage != nil {
					stats.usageTokens = usageTotalTokens(u.Usage)
				}
			}
			if bytes.Contains(clean, []byte(`"choices"`)) {
				var chunk struct {
					Model   string `json:"model"`
					ID      string `json:"id"`
					Choices []struct {
						Delta struct {
							Content          *string `json:"content"`
							ReasoningContent *string `json:"reasoning_content"`
							Reasoning        *string `json:"reasoning"`
							Thinking         *string `json:"thinking"`
							ToolCalls        []struct {
								ID string `json:"id"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal(clean, &chunk) == nil {
					if chunk.Model != "" {
						streamModel = chunk.Model
					}
					if chunk.ID != "" {
						xmlStreamID = chunk.ID
					}
					if len(chunk.Choices) > 0 {
						delta := chunk.Choices[0].Delta
						if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
							reasoningParts = append(reasoningParts, *delta.ReasoningContent)
						} else if delta.Reasoning != nil && *delta.Reasoning != "" {
							reasoningParts = append(reasoningParts, *delta.Reasoning)
						} else if delta.Thinking != nil && *delta.Thinking != "" {
							reasoningParts = append(reasoningParts, *delta.Thinking)
						}
						if delta.Content != nil && *delta.Content != "" {
							contentParts = append(contentParts, *delta.Content)
						}
						for _, tc := range delta.ToolCalls {
							if tc.ID != "" && !toolIDsMap[tc.ID] {
								toolIDsMap[tc.ID] = true
								toolIDs = append(toolIDs, tc.ID)
							}
						}
					}
				}
			}
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			frame := convert.EncodeSSE(clean)
			if _, err := w.Write(frame); err != nil {
				s.logger.Debug("stream write failed", "err", err)
				return
			}
			stats.chunks++
			stats.bytes += len(frame)
			lastWrite = time.Now()
			flusher.Flush()
		}
	}
}

// trackToolCallIndexes records per-stream tool-call state on every chunk
// BEFORE StripEndTurnToolCalls deletes the end_turn entries (tracking after
// the strip would never see them). end_turn indexes feed the downstream
// continuation-fragment drop — later argument fragments carry the same index
// but an empty name, so they are only recognizable by index — and any real
// named call flips seenRealToolCalls so the terminal finish_reason rewrite
// never downgrades a genuine tool-call turn to "stop". The cheap substring
// probe keeps non-tool chunks unmarshaled.
func trackToolCallIndexes(clean []byte, endTurnCallIndexes map[int]bool, seenRealToolCalls *bool) {
	if !bytes.Contains(clean, []byte(`"tool_calls"`)) {
		return
	}
	var chunk map[string]any
	if json.Unmarshal(clean, &chunk) != nil {
		return
	}
	if choices, ok := chunk["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			tcs, _ := delta["tool_calls"].([]any)
			for _, tc := range tcs {
				tcMap, _ := tc.(map[string]any)
				if tcMap == nil {
					continue
				}
				fn, _ := tcMap["function"].(map[string]any)
				if name, _ := fn["name"].(string); name == "end_turn" {
					if i, ok := tcMap["index"].(float64); ok {
						endTurnCallIndexes[int(i)] = true
					}
				} else if name != "" {
					*seenRealToolCalls = true
				}
			}
		}
	}
}

// bumpXMLCallIndex raises the per-stream synthetic tool-call index floor
// past the largest native tool-call index in tcs (the chunk's existing
// delta.tool_calls entries) so extracted XML fragments can never collide
// with upstream indexes. The floor persists across chunks via the pointer,
// so a floor established in one chunk applies to all later synthetics.
func bumpXMLCallIndex(tcs []any, xmlCallIndex *int) {
	for _, raw := range tcs {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		if i, ok := tc["index"].(float64); ok && int(i) >= *xmlCallIndex {
			*xmlCallIndex = int(i) + 1
		}
	}
}

func (s *Server) ingestStreamReasoning(model string, reasoningParts, contentParts, toolIDs []string) {
	if s.reasoningCache == nil || len(reasoningParts) == 0 {
		return
	}
	rc := strings.Join(reasoningParts, "")
	if rc == "" {
		return
	}
	cStr := strings.Join(contentParts, "")
	s.reasoningCache.Put(toolIDs, cStr, "", rc, "", model)
}

// relayJSON drains the upstream SSE stream through the accumulator and
// writes one chat.completion JSON response. On any decode or stream error
// nothing is written and a 502 is returned (the client asked for a single
// response; a partial one would be worse than none).
func (s *Server) relayJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	acc := convert.NewAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	first := true
	// Track whether the upstream stream carried any native delta.tool_calls
	// fragment: when it did, the accumulator skips XML extraction, so the
	// delivered tool_calls are native and an upstream "stop" is deliberate.
	// The XML-extraction finish flip below must then stay off — a native
	// tool-call response keeps its upstream finish_reason (pinned by
	// TestChatNonStream). When NO native fragment was seen, delivered calls
	// can only be extracted ones, which pair with an upstream "stop".
	sawNativeToolCalls := false
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), []byte(`"tool_calls":[{`)) {
			sawNativeToolCalls = true
		}
		if ctx.Err() != nil {
			return
		}
		if first {
			first = false
			phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
		}
		if err := acc.Add(scanner.Bytes()); err != nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
			return
		}
		stats.chunks++
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"upstream stream error: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
		}
		return
	}
	out := acc.Finish()
	// Strip Codebuff end_turn pseudo-tool-calls from the non-streaming
	// response, then align finish_reason with the tool_calls actually
	// delivered: a response whose calls were all end_turn (zero calls after
	// stripping) must read "stop", while XML-extracted calls carried in
	// message.tool_calls with an upstream "stop" (accumulator Finish) must
	// read "tool_calls" — parity with the streaming path's two flips.
	var comp map[string]any
	if bytes.Contains(out, []byte(`"tool_calls"`)) || bytes.Contains(out, []byte(`"finish_reason":"tool_calls"`)) {
		if json.Unmarshal(out, &comp) == nil {
			changed := false
			if bytes.Contains(out, []byte(`"end_turn"`)) {
				convert.StripEndTurnToolCalls(comp)
				changed = true
			}
			if rawChoices, ok := comp["choices"].([]any); ok {
				for _, raw := range rawChoices {
					choice, _ := raw.(map[string]any)
					if choice == nil {
						continue
					}
					fr, _ := choice["finish_reason"].(string)
					msg, _ := choice["message"].(map[string]any)
					if msg == nil {
						continue
					}
					tcs, _ := msg["tool_calls"].([]any)
					switch {
					case len(tcs) == 0 && fr == "tool_calls":
						choice["finish_reason"] = "stop"
						changed = true
					case len(tcs) > 0 && fr == "stop" && !sawNativeToolCalls:
						choice["finish_reason"] = "tool_calls"
						changed = true
					}
				}
			}
			if changed {
				if b, err := json.Marshal(comp); err == nil {
					out = b
				}
			}
		}
	}
	stats.bytes = len(out)
	// Capture the accumulated usage total for the spend ledger (#122);
	// only adopt when the response actually carries a usage block.
	var usageObj struct {
		Usage any `json:"usage"`
	}
	if json.Unmarshal(out, &usageObj) == nil && usageObj.Usage != nil {
		stats.usageTokens = usageTotalTokens(usageObj.Usage)
	}
	if s.reasoningCache != nil {
		var comp map[string]any
		if json.Unmarshal(out, &comp) == nil {
			model, _ := comp["model"].(string)
			if choices, ok := comp["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						rc, _ := msg["reasoning_content"].(string)
						if rc == "" {
							rc, _ = msg["reasoning"].(string)
						}
						if rc != "" {
							var toolIDs []string
							var tcJSON string
							if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
								for _, raw := range tcs {
									if tc, ok := raw.(map[string]any); ok {
										if id, ok := tc["id"].(string); ok && id != "" {
											toolIDs = append(toolIDs, id)
										}
									}
								}
								if b, err := json.Marshal(tcs); err == nil {
									tcJSON = string(b)
								}
							}
							cStr, _ := msg["content"].(string)
							s.reasoningCache.Put(toolIDs, cStr, tcJSON, rc, "", model)
						}
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
