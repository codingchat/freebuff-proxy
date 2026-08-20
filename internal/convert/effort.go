package convert

import (
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ExtractReasoningEffort extracts the requested thinking/reasoning effort from
// OpenAI reasoning_effort, Codex/Anthropic reasoning.effort, thinking flags,
// or model name suffixes (e.g. "model(high)" or "model:max").
func ExtractReasoningEffort(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload["reasoning_effort"].(string); ok && v != "" {
		return strings.ToLower(strings.TrimSpace(v))
	}
	if rObj, ok := payload["reasoning"].(map[string]any); ok {
		if v, ok := rObj["effort"].(string); ok && v != "" {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	if tObj, ok := payload["thinking"].(map[string]any); ok {
		if budget, ok := numInt64(tObj["budget_tokens"]); ok && budget > 0 {
			switch {
			case budget <= 512:
				return "minimal"
			case budget <= 1024:
				return "low"
			case budget <= 8192:
				return "medium"
			case budget <= 24576:
				return "high"
			default:
				return "xhigh"
			}
		}
		if v, ok := tObj["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	if m, ok := payload["model"].(string); ok && m != "" {
		m = strings.TrimSpace(m)
		if strings.HasSuffix(m, ")") {
			if idx := strings.LastIndex(m, "("); idx > 0 {
				tag := strings.ToLower(strings.TrimSpace(m[idx+1 : len(m)-1]))
				switch tag {
				case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
					return tag
				}
			}
		} else if idx := strings.LastIndex(m, ":"); idx > 0 {
			tag := strings.ToLower(strings.TrimSpace(m[idx+1:]))
			switch tag {
			case "max", "high", "medium", "low", "minimal", "xhigh", "ultra":
				return tag
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Reasoning effort normalization (issue #65 clamp + #111 plain wire field).
// Ported from freebuff/common/src/constants/reasoning-effort.ts
// (clampReasoningEffort). The DeepSeek thinking translation is server-side
// (codebuff-fork/web/src/llm-api/deepseek-request-body.ts), so the proxy
// never emits a thinking block — plain reasoning_effort goes on the wire.
// ---------------------------------------------------------------------------

// reasoningLadder is the one reasoning-effort vocabulary, strictly ascending.
// Its ORDER is load-bearing: clamping does index arithmetic on it.
var reasoningLadder = [...]string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// defaultReasoningEffort is used when a request asks for an effort that is
// not on the ladder (mirrors DEFAULT_REASONING_EFFORT = 'high').
const defaultReasoningEffort = "high"

// modelReasoningEfforts is the per-model effort allowance, mirroring
// reference/freebuff/common/src/constants/freebuff-models.ts (08/20 catalog).
// Rows, per model:
//
//	deepseek-v4-flash / deepseek-v4-pro  DEEPSEEK_V4_REASONING_EFFORTS
//	                                     ['low','high','max'] — medium is not a
//	                                     distinct level and rewrites to high
//	                                     (see normalizeReasoning).
//	gpt-5.6-luna / claude-fable-5        EFFORTS_THROUGH_MAX low..max (xhigh
//	                                     included, OpenRouter metadata).
//	muse-spark-1.2-contributor           EFFORTS_THROUGH_XHIGH minimal..xhigh
//	                                     (measured ladder, 08/06).
//	mimo-v2.5                            {'high'} — Xiaomi exposes only
//	                                     disabled/high (enabled); there is no
//	                                     depth ladder, so the source comments
//	                                     call low/medium/max "compatibility
//	                                     aliases".
//	minimax-m3                           {'high'} — "supports adaptive thinking
//	                                     or disabled thinking, but no effort
//	                                     levels"; a depth picker is cosmetic.
//	z-ai/glm-5.2, crof/kimi-k3-eco        absent — the CrofAI routes accept but
//	                                     ignore reasoning_effort (including
//	                                     invalid values), so no clamp.
//	google/*-flash-lite rows             absent — helper models, no upstream
//	                                     restriction.
//
// Models absent from the table get the full ladder (no clamping). The
// provisioned -max variants are NOT listed: they are blocked at the
// ServedModels gate (issue #153) before conversion ever runs. The table is
// refreshable at runtime via SetModelEffortLookup.
var modelReasoningEfforts = map[string][]string{
	"deepseek/deepseek-v4-flash":      {"low", "high", "max"},
	"deepseek/deepseek-v4-pro":        {"low", "high", "max"},
	"mimo/mimo-v2.5":                  {"high"},
	"minimax/minimax-m3":              {"high"},
	"anthropic/claude-fable-5":        {"low", "medium", "high", "xhigh", "max"},
	"openai/gpt-5.6-luna":             {"low", "medium", "high", "xhigh", "max"},
	"meta/muse-spark-1.2-contributor": {"minimal", "low", "medium", "high", "xhigh"},
}

// ReasoningLookupFn looks up cached reasoning content by tool ID or by content + toolCalls JSON.
type ReasoningLookupFn func(toolID string, content, toolCallsJSON string) (reasoning, signature string, ok bool)

var globalReasoningLookup atomic.Pointer[ReasoningLookupFn]

// SetReasoningLookup installs a global reasoning lookup hook used to restore
// missing reasoning_content on assistant messages in multi-turn requests.
// Passing nil clears the hook.
func SetReasoningLookup(fn ReasoningLookupFn) {
	if fn == nil {
		globalReasoningLookup.Store(nil)
		return
	}
	globalReasoningLookup.Store(&fn)
}

// modelEffortLookup, when set, overrides modelReasoningEfforts for the model
// in question. Main wires this to registry data when the registry exposes
// per-model effort arrays; the hardcoded table is the fallback.
var modelEffortLookup atomic.Pointer[func(string) []string]

// SetModelEffortLookup installs a per-model effort lookup used by the
// reasoning clamp. The function receives a model id and returns the allowed
// effort rungs (ascending ladder order), or nil to fall back to the
// hardcoded table. Passing nil removes the override.
func SetModelEffortLookup(fn func(string) []string) {
	if fn == nil {
		modelEffortLookup.Store(nil)
		return
	}
	modelEffortLookup.Store(&fn)
}

// effortsForModel returns the allowed effort rungs for a model: the override
// lookup when it answers, else the hardcoded table (with the full ladder for
// unlisted models).
func effortsForModel(model string) []string {
	if p := modelEffortLookup.Load(); p != nil {
		if allowed := (*p)(model); allowed != nil {
			return allowed
		}
	}
	if allowed, ok := modelReasoningEfforts[model]; ok {
		return allowed
	}
	return reasoningLadder[:]
}

// reasoningRank returns the position of effort on the ladder, or -1 when the
// effort is not a rung.
func reasoningRank(effort string) int {
	for i, rung := range reasoningLadder {
		if rung == effort {
			return i
		}
	}
	return -1
}

// clampReasoningEffort clamps requested DOWN to the nearest allowed rung no
// higher than requested (official clampReasoningEffort semantics): when every
// allowed rung is above what was asked for, the lowest allowed rung wins;
// when requested is absent or unrecognized, fallback wins. It never rejects
// and never changes the model.
func clampReasoningEffort(requested string, allowed []string, fallback string) string {
	if len(allowed) == 0 {
		return fallback
	}
	wanted := reasoningRank(requested)
	if wanted < 0 {
		return fallback
	}
	best := -1
	for _, candidate := range allowed {
		rank := reasoningRank(candidate)
		if rank < 0 || rank > wanted {
			continue
		}
		if rank > best {
			best = rank
		}
	}
	if best >= 0 {
		return reasoningLadder[best]
	}
	// Everything on offer is above what was asked for: give the least of them.
	lowest := allowed[0]
	for _, candidate := range allowed[1:] {
		if reasoningRank(candidate) < reasoningRank(lowest) {
			lowest = candidate
		}
	}
	return lowest
}

// isDeepSeekModel reports whether the route is one of the DeepSeek V4 models.
// DeepSeek routes accept prompt-cache hints (#84) and rewrite requested
// "medium" to "high" (#112). Tolerates both the registry's full ids and bare
// model ids. The provisioned -max variants are blocked at the ServedModels
// gate (issue #153) and never reach conversion.
func isDeepSeekModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasSuffix(m, "deepseek-v4-flash") ||
		strings.HasSuffix(m, "deepseek-v4-pro")
}

// isStrictReasoningModel reports whether the model requires an explicit reasoning_content
// field on assistant messages with tool calls (e.g. MiMo, DeepSeek-V4, Kimi).
func isStrictReasoningModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "mimo") || strings.Contains(m, "deepseek-v4") || strings.Contains(m, "kimi")
}

var (
	leakedThinkTagsRe   = regexp.MustCompile(`(?is)<think>(.*?)</think>|<thinking>(.*?)</thinking>|<reasoning>(.*?)</reasoning>|<antml:thinking>(.*?)</antml:thinking>`)
	unclosedThinkTagsRe = regexp.MustCompile(`(?is)<(?:think|thinking|reasoning|antml:thinking)>(.*)$`)
)

// extractLeakedThinkTags parses leaked reasoning tags (<think>, <thinking>,
// <reasoning>, <antml:thinking>) from text content. It returns the concatenated
// reasoning string and the cleaned content with tags stripped.
func extractLeakedThinkTags(content string) (string, string) {
	matches := leakedThinkTagsRe.FindAllStringSubmatch(content, -1)
	var reasoningParts []string
	for _, match := range matches {
		for _, part := range match[1:] {
			if part != "" {
				reasoningParts = append(reasoningParts, part)
				break
			}
		}
	}
	cleaned := leakedThinkTagsRe.ReplaceAllString(content, "")
	// If an unclosed think tag exists at the end of the text (e.g. generation
	// cutoff at max_tokens), extract the partial thinking and strip the tag.
	if unclosed := unclosedThinkTagsRe.FindStringSubmatch(cleaned); len(unclosed) > 1 {
		if partial := strings.TrimSpace(unclosed[1]); partial != "" {
			reasoningParts = append(reasoningParts, partial)
		}
		cleaned = unclosedThinkTagsRe.ReplaceAllString(cleaned, "")
	}
	if len(reasoningParts) == 0 {
		return "", content
	}
	return strings.Join(reasoningParts, "\n"), strings.TrimSpace(cleaned)
}

// normalizeReasoning rewrites the outbound reasoning_effort for the target
// model:
//
//   - reasoning_effort is always sent PLAIN, clamped to the model's allowed
//     rungs (down-nearest, never rejected; unknown efforts fall back to
//     "high"). The DeepSeek thinking translation is server-side per issue
//     #111 — the proxy never emits a thinking block.
//   - "medium" on a DeepSeek route rewrites to "high" first (CLI
//     resolveFreebuffReasoningEffort: medium → high on Flash/Pro, #112).
//   - reasoning.enabled === false or thinking.type "disabled" suppresses the
//     effort entirely (no reasoning_effort, no thinking field).
func normalizeReasoning(payload, out map[string]any) {
	model, _ := out["model"].(string)
	eff := ""
	if v, ok := out["reasoning_effort"].(string); ok && v != "" {
		eff = strings.ToLower(strings.TrimSpace(v))
	}
	if eff == "" {
		eff = ExtractReasoningEffort(payload)
	}

	disabled := false
	if rObj, ok := payload["reasoning"].(map[string]any); ok {
		if en, ok := rObj["enabled"].(bool); ok && !en {
			disabled = true
		}
	}
	// A client that explicitly disables thinking (Anthropic-style
	// thinking:{type:"disabled"}) must win even when reasoning_effort is
	// also present (review P2): re-enabling thinking the client turned off
	// is a silent behavioral override.
	if tObj, ok := payload["thinking"].(map[string]any); ok {
		if tt, ok := tObj["type"].(string); ok && strings.EqualFold(strings.TrimSpace(tt), "disabled") {
			disabled = true
		}
	}

	switch {
	case disabled:
		delete(out, "reasoning_effort")
	case eff == "" || eff == "none" || eff == "disabled":
		// No effort requested: leave the body untouched.
	default:
		var clamped string
		if isDeepSeekModel(model) && eff == "medium" {
			// CLI resolveFreebuffReasoningEffort: medium is intentionally
			// absent from the DeepSeek ladders and rewrites to high — never
			// down to low (the generic clamp would say low).
			clamped = "high"
		} else {
			clamped = clampReasoningEffort(eff, effortsForModel(model), defaultReasoningEffort)
		}
		out["reasoning_effort"] = clamped
	}
}

// ---------------------------------------------------------------------------
// Reasoning-in-content injection (issue #44).
//
// Some legacy clients never render a reasoning channel; when
// REASONING_IN_CONTENT is set (default off), reasoning_content is folded into
// delta.content as "<tag>...</tag>" text so those clients still see the
// reasoning. The reasoning channel is preserved alongside, and
// reasoning_details is never folded (it is replayed verbatim).
// ---------------------------------------------------------------------------

// reasoningInContentTag is the default think-tag label when the env var is a
// bare boolean.
const reasoningInContentTag = "think"

// reasoningInContentMode returns the think-tag label when reasoning folding is
// enabled, or "" when off. The env var REASONING_IN_CONTENT may be a boolean
// (true → "think") or an explicit tag word ("thinking" → "thinking").
func reasoningInContentMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("REASONING_IN_CONTENT")))
	switch v {
	case "", "0", "false", "off", "no", "disabled":
		return ""
	case "1", "true", "yes", "on":
		return reasoningInContentTag
	}
	return v
}

// foldReasoningIntoContent appends the <tag>reasoning</tag> text to the
// delta's content when folding is enabled. Reasoning precedes text (the
// model layer enqueues reasoning deltas before text deltas); non-string
// content is left untouched.
func foldReasoningIntoContent(delta map[string]any, reasoning string) {
	tag := reasoningInContentMode()
	if tag == "" || reasoning == "" {
		return
	}
	switch c := delta["content"].(type) {
	case string:
		delta["content"] = "<" + tag + ">" + reasoning + "</" + tag + ">" + c
	case nil:
		delta["content"] = "<" + tag + ">" + reasoning + "</" + tag + ">"
	}
}
