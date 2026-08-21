package server

// Responses streaming translation: relayResponsesStream converts the upstream
// chat SSE stream into Responses SSE events (response.created ->
// response.completed, output_item add/delta/done) with streaming XML tool-call
// handling, and relayResponsesJSON drains the non-streaming path into one
// completed Responses object.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
)

// responsesItem is one output item being assembled during stream relay:
// either a message (text) or a function_call.
type responsesItem struct {
	id          string
	kind        string // "message" | "function_call"
	outputIndex int
	callID      string
	name        string
	text        string
	args        strings.Builder
	contentIdx  int
	started     bool
}

// responsesStreamState tracks the relayed output items.
type responsesStreamState struct {
	items       []*responsesItem
	nextIndex   int
	toolByUpIdx map[int]*responsesItem
	model       string
	usage       any
}

// relayResponsesStream translates upstream chat SSE chunks into Responses
// SSE events. On an in-band upstream error chunk it emits response.failed
// with the error attached and stops (the client gets a terminal, parseable
// signal instead of a chat-shaped error frame).
func (s *Server) relayResponsesStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, model, respID string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connecting\n\n")
	flusher.Flush()

	createdAt := time.Now().Unix()
	st := &responsesStreamState{toolByUpIdx: make(map[int]*responsesItem), model: model}
	send := func(ev map[string]any) {
		b, _ := json.Marshal(ev)
		_, _ = w.Write(convert.EncodeSSE(b))
		flusher.Flush()
	}
	send(map[string]any{"type": "response.created", "response": responsesBase(model, respID, createdAt, "in_progress")})
	send(map[string]any{"type": "response.in_progress", "response": responsesBase(model, respID, createdAt, "in_progress")})

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	lines := make(chan lineChunk)
	go relayReadLoop(ctx, r, lines)
	// lastWrite tracks the last frame actually written to the CLIENT; the
	// keepalive condition keys on it so a liveness signal is emitted after
	// any client-write silence, regardless of upstream comment/junk dribble
	// (those are dropped and never relayed — #161).
	lastWrite := time.Now()
	first := true
	endTurnCallIndexes := make(map[int]bool)
	// XML tool-call extractor: models such as MiMo/Hermes/Qwen emit tool
	// calls as XML/JSON text blocks inline in delta.content instead of
	// native delta.tool_calls. One instance per stream; Feed every content
	// delta in order; Flush once before the terminal frame.
	xmlExtractor := &convert.XMLToolCallExtractor{}
	xmlCallIndex := 0 // sequential synthetic tool-call indexes for extracted calls
	lastID := ""

	// flushXMLCalls releases any still-open candidate block at stream end:
	// extracted calls become native tool_calls fragments (with sequential
	// synthetic indexes) and any scrubbed text is relayed as a content
	// delta, so accumulateResponsesChunk creates the items and the terminal
	// frame carries complete output.
	flushXMLCalls := func() {
		ft, fc := xmlExtractor.Flush()
		if ft == "" && len(fc) == 0 {
			return
		}
		delta := make(map[string]any, 2)
		if ft != "" {
			delta["content"] = ft
		}
		if len(fc) > 0 {
			frags := make([]any, 0, len(fc))
			for _, call := range fc {
				if call.Function.Name == "end_turn" {
					continue // strip-parity: never relay the proxy-injected pseudo-tool
				}
				frags = append(frags, convert.ToolCallDeltaFragment(xmlCallIndex, call))
				xmlCallIndex++
			}
			if len(frags) > 0 {
				delta["tool_calls"] = frags
			}
		}
		id := "chatcmpl-flush"
		if lastID != "" {
			id = lastID
		}
		mdl := ""
		if st.model != "" {
			mdl = st.model
		}
		synthetic := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   mdl,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
		}
		s.accumulateResponsesChunk(st, synthetic, send)
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
					s.logger.Warn("responses upstream stream error", "err", lc.err)
					flushXMLCalls()
					s.endResponsesStream(w, send, st, model, respID, createdAt, true, nil)
				}
				return
			}
			if lc.done {
				flushXMLCalls()
				s.endResponsesStream(w, send, st, model, respID, createdAt, false, nil)
				return
			}
			clean, drop := convert.SanitizeChunk(lc.line)
			if drop {
				// Dropped upstream lines are never relayed and must not
				// advance the keepalive timer (client sees only real
				// frames — #161).
				continue
			}
			var chunk map[string]any
			if err := json.Unmarshal(clean, &chunk); err != nil {
				continue
			}
			// --- XML tool calls embedded in content ---
			// Feed each content delta through the stream extractor: safe text
			// is relayed as-is, extracted calls become native tool_calls
			// fragments with sequential synthetic indexes (existing native
			// indexes stay untouched). The rest of the pipeline
			// (StripEndTurnToolCalls + accumulateResponsesChunk) translates
			// the mutated chunk as usual.
			if rawChoices, ok := chunk["choices"].([]any); ok && len(rawChoices) > 0 {
				choice, _ := rawChoices[0].(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				if content, ok := delta["content"].(string); ok && content != "" {
					text, calls := xmlExtractor.Feed(content)
					if text != content {
						if text == "" {
							delete(delta, "content")
						} else {
							delta["content"] = text
						}
					}
					if len(calls) > 0 {
						tcs, _ := delta["tool_calls"].([]any)
						if tcs == nil {
							tcs = make([]any, 0, len(calls))
						}
						bumpXMLCallIndex(tcs, &xmlCallIndex)
						for _, call := range calls {
							if call.Function.Name == "end_turn" {
								continue // strip-parity: never relay the proxy-injected pseudo-tool
							}
							tcs = append(tcs, convert.ToolCallDeltaFragment(xmlCallIndex, call))
							xmlCallIndex++
						}
						if len(tcs) > 0 {
							delta["tool_calls"] = tcs
						}
					}
				}
			}
			// --- end_turn pseudo-tool-call filtering ---
			// Record end_turn indexes before stripping to catch continuation fragments.
			foundEndTurn := false
			if rawChoices, ok := chunk["choices"].([]any); ok {
				for _, c := range rawChoices {
					choice, _ := c.(map[string]any)
					if choice == nil {
						continue
					}
					delta, _ := choice["delta"].(map[string]any)
					if rawTCs, ok := delta["tool_calls"].([]any); ok {
						for _, raw := range rawTCs {
							tc, _ := raw.(map[string]any)
							if tc == nil {
								continue
							}
							fn, _ := tc["function"].(map[string]any)
							if name, _ := fn["name"].(string); name == "end_turn" {
								foundEndTurn = true
								if idx, ok := tc["index"].(float64); ok {
									endTurnCallIndexes[int(idx)] = true
								}
							}
						}
					}
				}
			}
			toolCallsRemaining, _ := convert.StripEndTurnToolCalls(chunk)
			// Drop continuation fragments for stripped end_turn indexes.
			if rawChoices, ok := chunk["choices"].([]any); ok && len(endTurnCallIndexes) > 0 {
				for _, c := range rawChoices {
					choice, _ := c.(map[string]any)
					if choice == nil {
						continue
					}
					delta, _ := choice["delta"].(map[string]any)
					raw, _ := delta["tool_calls"].([]any)
					if len(raw) == 0 {
						continue
					}
					filtered := make([]any, 0, len(raw))
					dropped := false
					for _, r := range raw {
						tc, ok := r.(map[string]any)
						if !ok {
							filtered = append(filtered, r)
							continue
						}
						idx, _ := tc["index"].(float64)
						if endTurnCallIndexes[int(idx)] {
							dropped = true
							continue
						}
						filtered = append(filtered, r)
					}
					if dropped {
						if len(filtered) == 0 {
							delete(delta, "tool_calls")
							toolCallsRemaining = false
						} else {
							delta["tool_calls"] = filtered
						}
					}
				}
			}
			// Flip finish_reason only when end_turn calls were actually found
			// in this chunk and no real tool calls remain. Without the
			// foundEndTurn gate, the terminal chunk (finish_reason: "tool_calls",
			// empty delta) would be incorrectly rewritten to "stop" for
			// non-end_turn tool calls.
			if foundEndTurn && !toolCallsRemaining {
				if rawChoices, ok := chunk["choices"].([]any); ok {
					for _, c := range rawChoices {
						choice, _ := c.(map[string]any)
						if choice == nil {
							continue
						}
						if fr, ok := choice["finish_reason"].(string); ok && fr == "tool_calls" {
							choice["finish_reason"] = "stop"
						}
					}
				}
			}
			// Skip chunk if it was emptied by end_turn stripping (delta now
			// empty AND finish_reason is null/absent — a real terminal chunk
			// with finish_reason must never be dropped).
			if foundEndTurn && !toolCallsRemaining {
				if rawChoices, ok := chunk["choices"].([]any); ok {
					for _, c := range rawChoices {
						choice, _ := c.(map[string]any)
						if choice == nil {
							continue
						}
						if delta, ok := choice["delta"].(map[string]any); ok && len(delta) == 0 {
							if fr, ok := choice["finish_reason"]; !ok || fr == nil {
								drop = true
							}
						}
					}
				}
			}
			if drop {
				// Chunk emptied by end_turn stripping: nothing was written
				// to the client, so the keepalive timer must not advance.
				continue
			}
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			if errVal, hasErr := chunk["error"]; hasErr && errVal != nil {
				// In-band upstream failure: mirror the error frame's
				// message/type on the response object and fail the stream.
				var msg, typ string
				if em, ok := errVal.(map[string]any); ok {
					msg, _ = em["message"].(string)
					typ, _ = em["type"].(string)
				} else if es, ok := errVal.(string); ok {
					msg = es
				}
				if msg == "" {
					msg = "upstream error"
				}
				if typ == "" {
					typ = "upstream_error"
				}
				s.endResponsesStream(w, send, st, model, respID, createdAt, true, map[string]any{"message": msg, "type": typ})
				return
			}
			lastWrite = time.Now()
			stats.chunks++
			stats.bytes += len(clean)
			if m, _ := chunk["model"].(string); m != "" {
				st.model = m
			}
			if id, _ := chunk["id"].(string); id != "" {
				lastID = id
			}
			if usage, ok := chunk["usage"]; ok && usage != nil {
				st.usage = usage
				stats.usageTokens = usageTotalTokens(usage) // #122 spend ledger
			}
			s.accumulateResponsesChunk(st, chunk, send)
		}
	}
}

// endResponsesStream emits the per-item done events and the terminal
// response.completed (or response.failed) event.
func (s *Server) endResponsesStream(w http.ResponseWriter, send func(map[string]any), st *responsesStreamState, model, respID string, createdAt int64, failed bool, errObj map[string]any) {
	// Ensure at least one output item so output is never empty.
	if len(st.items) == 0 {
		item := &responsesItem{id: "msg_" + randHexString(12), kind: "message", outputIndex: st.nextIndex}
		st.nextIndex++
		st.items = append(st.items, item)
	}
	for _, item := range st.items {
		if item.kind == "message" {
			if !item.started {
				sendResponsesItemAdded(send, item)
			}
			part := map[string]any{"type": "output_text", "text": item.text, "annotations": []any{}}
			send(map[string]any{"type": "response.output_text.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "text": item.text})
			send(map[string]any{"type": "response.content_part.done", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "part": part})
			send(map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}})
		} else {
			send(map[string]any{"type": "response.output_item.done", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "completed", "call_id": item.callID, "name": item.name, "arguments": item.args.String()}})
		}
	}
	resp := responsesBase(model, respID, createdAt, "completed")
	resp["model"] = st.model
	out := make([]any, 0, len(st.items))
	for _, item := range st.items {
		if item.kind == "message" {
			out = append(out, map[string]any{
				"id": item.id, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": item.text, "annotations": []any{}}},
			})
		} else {
			out = append(out, map[string]any{
				"id": item.id, "type": "function_call", "status": "completed",
				"call_id": item.callID, "name": item.name, "arguments": item.args.String(),
			})
		}
	}
	resp["output"] = out
	if st.usage != nil {
		resp["usage"] = responsesUsage(st.usage)
	}
	if failed {
		resp["status"] = "failed"
		if errObj != nil {
			resp["error"] = errObj
		}
		send(map[string]any{"type": "response.failed", "response": resp})
		return
	}
	send(map[string]any{"type": "response.completed", "response": resp})
}

// sendResponsesItemAdded emits the output_item.added + content_part.added
// pair for a message item.
func sendResponsesItemAdded(send func(map[string]any), item *responsesItem) {
	send(map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
	send(map[string]any{"type": "response.content_part.added", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}

// accumulateResponsesChunk translates one upstream chat chunk into
// Responses events: text deltas and tool-call argument deltas, creating
// output items on first use.
func (s *Server) accumulateResponsesChunk(st *responsesStreamState, chunk map[string]any, send func(map[string]any)) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok || choice == nil {
		return
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return
	}
	// Tool-call fragments: one output item per upstream tool index.
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			upIdx := 0
			if i, ok := numFloat64(tc["index"]); ok {
				upIdx = int(i)
			}
			item := st.toolByUpIdx[upIdx]
			if item == nil {
				item = &responsesItem{id: "fc_" + randHexString(12), kind: "function_call", outputIndex: st.nextIndex}
				st.nextIndex++
				st.toolByUpIdx[upIdx] = item
				st.items = append(st.items, item)
				send(map[string]any{"type": "response.output_item.added", "output_index": item.outputIndex, "item": map[string]any{"id": item.id, "type": "function_call", "status": "in_progress", "call_id": "", "name": "", "arguments": ""}})
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" && item.name == "" {
					item.name = name
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					item.args.WriteString(args)
					send(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item.id, "output_index": item.outputIndex, "delta": args})
				}
			}
			if id, ok := tc["id"].(string); ok && id != "" && item.callID == "" {
				item.callID = id
			}
		}
	}
	// Text deltas.
	if content, ok := delta["content"].(string); ok && content != "" {
		var item *responsesItem
		for _, it := range st.items {
			if it.kind == "message" {
				item = it
				break
			}
		}
		if item == nil {
			item = &responsesItem{id: "msg_" + randHexString(12), kind: "message", outputIndex: st.nextIndex}
			st.nextIndex++
			st.items = append(st.items, item)
		}
		if !item.started {
			item.started = true
			sendResponsesItemAdded(send, item)
		}
		item.text += content
		send(map[string]any{"type": "response.output_text.delta", "item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIdx, "delta": content})
	}
}

// relayResponsesJSON drains the upstream stream and writes one completed
// Responses object. On any decode/stream error a 502 is returned.
func (s *Server) relayResponsesJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time, model, respID string) {
	acc := convert.NewAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	first := true
	for scanner.Scan() {
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
	// Accumulate into a Responses output list.
	var completion map[string]any
	if err := json.Unmarshal(acc.Finish(), &completion); err != nil {
		s.writeJSONError(w, http.StatusBadGateway,
			"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
		return
	}
	convert.StripEndTurnToolCalls(completion)
	resp := responsesBase(model, respID, time.Now().Unix(), "completed")
	if m, _ := completion["model"].(string); m != "" {
		resp["model"] = m
	}
	out := make([]any, 0, 2)
	choices, _ := completion["choices"].([]any)
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				text, _ := msg["content"].(string)
				if text != "" {
					item := map[string]any{
						"id": "msg_" + randHexString(12), "type": "message", "status": "completed", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
					}
					out = append(out, item)
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, raw := range tcs {
						tc, ok := raw.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tc["function"].(map[string]any)
						name, _ := fn["name"].(string)
						args, _ := fn["arguments"].(string)
						id, _ := tc["id"].(string)
						if id == "" {
							id = "call_" + randHexString(12)
						}
						out = append(out, map[string]any{
							"id": "fc_" + randHexString(12), "type": "function_call", "status": "completed",
							"call_id": id, "name": name, "arguments": args,
						})
					}
				}
			}
		}
	}
	resp["output"] = out
	if usage, ok := completion["usage"]; ok && usage != nil {
		resp["usage"] = responsesUsage(usage)
		stats.usageTokens = usageTotalTokens(usage) // #122 spend ledger
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
