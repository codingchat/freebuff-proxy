package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"freebuff-proxy/internal/stealth"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionState is the parsed result of a free-session create/poll.
type SessionState struct {
	Status             string
	InstanceID         string
	Model              string
	CurrentModel       string
	RequestedModel     string
	ExpiresAt          time.Time
	AdmittedAt         time.Time
	GracePeriodEndsAt  time.Time
	GraceRemainingMs   int64
	Position           int
	QueueDepth         int
	EstimatedWaitMs    int
	PollAt             time.Time
	CountryCode        string
	CountryBlockReason string
	IpPrivacySignals   []string
	ActiveUsersForIP   int
	Limit              float64
	RecentCount        float64
	ResetAt            time.Time
	ResumesAt          time.Time
	RetryAfterMs       int64
	AvailableHours     string
	Message            string
	// UnavailableWindow is the parsed availability window carried by a
	// model_unavailable admission response (issue #158); nil when the
	// response omitted availableHours or the string could not be parsed.
	UnavailableWindow *AvailabilityWindow
	// GlmPromo carries the raw JSON of the upstream glmPromo block
	// ({dailySessions, endsAt}) when the probe/admission response includes
	// it. Kept as a string so callers render the shape without the upstream
	// adding fields; "" when absent.
	GlmPromo string
	// RateLimitsByModel carries the live per-model session quotas from the
	// admission/poll response (key = model id). Absent on compact polls and
	// pre-join (none) responses; never required.
	RateLimitsByModel map[string]ModelQuota
	// Standing is the upstream account standing block (issue #96), parsed
	// from the session response's "standing" field ({level,label,score,
	// nextLevelAt,nextLevel}); nil when the response omits it.
	Standing *SessionStanding
}

// AvailabilityWindow is the parsed daily availability window from a
// model_unavailable admission response's availableHours string (issue #158),
// e.g. "9am ET-5pm PT every day" or "08:00-20:00". Times are normalized to
// minutes since midnight in US Pacific — the reference zone FreeBuff
// sessions and quota windows are Pacific-based — so a skip decision needs no
// DST math at compare time. ET/EST/EDT are converted by subtracting the
// fixed 3-hour ET→PT offset (both observe US DST in lockstep).
type AvailabilityWindow struct {
	// StartMinute/EndMinute bound the daily window, minutes since midnight
	// Pacific. StartMinute == EndMinute means a degenerate/24-7 window:
	// callers must not skip on it.
	StartMinute int
	EndMinute   int
	// Raw is the original availableHours string, for logging.
	Raw string
}

// availableTimeRE matches "H[:MM] [am|pm] [zone]" - "H[:MM] [am|pm] [zone]"
// pairs in an availableHours string. Zones are best-effort 2-3 letter
// tokens (ET/PT/…); unrecognized tails are ignored (the regex is not
// anchored).
var availableTimeRE = regexp.MustCompile(`(?i)(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*([a-z]{2,3})?\s*-\s*(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*([a-z]{2,3})?`)

// ParseAvailabilityWindow parses an upstream availableHours string into a
// daily window. Supported shapes:
//
//	"08:00-20:00"            24-hour window (interpreted in Pacific)
//	"9am ET-5pm PT every day"  12-hour window with US timezone abbreviations
//
// ok is false when no start/end time pair can be extracted, so callers fall
// back to the plain cache TTL instead of deriving a skip bound.
func ParseAvailabilityWindow(s string) (AvailabilityWindow, bool) {
	m := availableTimeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return AvailabilityWindow{}, false
	}
	start, ok1 := parseAvailableTime(m[1], m[2], m[3], m[4])
	end, ok2 := parseAvailableTime(m[5], m[6], m[7], m[8])
	if !ok1 || !ok2 {
		return AvailabilityWindow{}, false
	}
	return AvailabilityWindow{StartMinute: start, EndMinute: end, Raw: s}, true
}

// parseAvailableTime converts one time token to minutes since midnight
// Pacific. hour/minute may be 12-hour (with meridiem) or 24-hour; the zone
// token converts ET-family zones to Pacific minutes. Unknown/absent zones
// are interpreted directly as Pacific (best-effort — the caller's TTL cap
// bounds any misparse).
func parseAvailableTime(hour, minute, meridiem, zone string) (int, bool) {
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	min := 0
	if minute != "" {
		m, err := strconv.Atoi(minute)
		if err != nil || m < 0 || m > 59 {
			return 0, false
		}
		min = m
	}
	switch strings.ToLower(meridiem) {
	case "am":
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 12 {
			h += 12
		}
	case "":
	default:
		return 0, false
	}
	total := h*60 + min
	switch strings.ToUpper(zone) {
	case "ET", "EST", "EDT":
		total -= 3 * 60
	case "PT", "PST", "PDT", "":
	default:
		// Unknown zone: keep as Pacific (best-effort).
	}
	total %= 1440
	if total < 0 {
		total += 1440
	}
	return total, true
}

// pacificLoc returns America/Los_Angeles, falling back to a fixed PDT zone
// when the tz database is unavailable (mirrors pool/spend.go's loc helper).
var pacificLoc = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return loc
	}
	return time.FixedZone("PDT", -7*60*60)
})

// AvailableAt reports whether minute-of-day (Pacific) falls inside the
// window. A degenerate window (StartMinute == EndMinute) is always
// available: the caller must not skip on it.
func (w AvailabilityWindow) AvailableAt(now time.Time) bool {
	if w.StartMinute == w.EndMinute {
		return true
	}
	m := now.In(pacificLoc()).Hour()*60 + now.In(pacificLoc()).Minute()
	if w.StartMinute < w.EndMinute {
		return m >= w.StartMinute && m < w.EndMinute
	}
	// Overnight window (e.g. 22:00-06:00): open from start until midnight,
	// then from midnight until end.
	return m >= w.StartMinute || m < w.EndMinute
}

// NextStart returns the next wall-clock instant the window opens, strictly
// after now. For a degenerate window it returns now (no future opening to
// wait for).
func (w AvailabilityWindow) NextStart(now time.Time) time.Time {
	loc := pacificLoc()
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	if w.StartMinute == w.EndMinute {
		return now
	}
	start := time.Date(t.Year(), t.Month(), t.Day(), w.StartMinute/60, w.StartMinute%60, 0, 0, loc)
	if m < w.StartMinute {
		return start
	}
	// The window already opened today (and we are outside it — a refusal
	// was cached, so the upstream disagrees with our parse): the next
	// opening is tomorrow.
	return start.AddDate(0, 0, 1)
}

// SessionStanding is the upstream account standing block (issue #96): the
// pre-join/session response's "standing" field. NextLevelAt is parsed with
// parseFlexTime; zero when the server omits it.
type SessionStanding struct {
	Level       string
	Label       string
	Score       float64
	NextLevelAt time.Time
	NextLevel   string
}

// ModelQuota is one model's live session quota from the upstream
// rateLimitsByModel map, per the official CLI wire shape
// (reference/freebuff/common/src/types/freebuff-session.ts).
// Entitlement holds the per-period breakdown (base/referral/streak/promo;
// promo is omitted by default) that sums to Limit when the server emits it.
type ModelQuota struct {
	Model       string
	Limit       float64
	RecentCount float64
	ResetAt     time.Time
	Period      string // "pacific_day" | "pacific_week" (empty when absent)
	Entitlement map[string]float64
}

// rawModelQuota mirrors one rateLimitsByModel entry on the wire. resetAt is
// parsed with parseFlexTime (RFC3339, unix seconds, or unix ms); windowHours
// (deprecated) is deliberately not surfaced.
type rawModelQuota struct {
	Model                string             `json:"model"`
	Limit                float64            `json:"limit"`
	RecentCount          float64            `json:"recentCount"`
	Period               string             `json:"period"`
	ResetAt              any                `json:"resetAt"`
	EntitlementBreakdown map[string]float64 `json:"entitlementBreakdown"`
}

// rawStanding mirrors the session response's "standing" block (issue #96).
// nextLevelAt is parsed with parseFlexTime.
type rawStanding struct {
	Level       string  `json:"level"`
	Label       string  `json:"label"`
	Score       float64 `json:"score"`
	NextLevelAt any     `json:"nextLevelAt"`
	NextLevel   string  `json:"nextLevel"`
}

// CreateSession POSTs /api/v1/freebuff/session with no body.
func (c *Client) CreateSession(ctx context.Context) (*SessionState, error) {
	return c.CreateSessionForModel(ctx, "")
}

// CreateSessionForModel POSTs /api/v1/freebuff/session with the requested
// model header. The POST carries NO body and therefore no Content-Type
// (#120): the CLI's session POST is a bare fetch with Authorization + the
// optional x-freebuff-model header only (reference/freebuff
// freebuff-session-api.ts callFreebuffSession, codebuff-api.ts sets
// Content-Type only when body !== undefined).
func (c *Client) CreateSessionForModel(ctx context.Context, model string) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if model != "" {
		req.Header.Set("x-freebuff-model", model)
	}
	return c.sessionCall(req)
}

// GetSession polls /api/v1/freebuff/session for the given instance. A poll
// 404 maps to Status "ended" (the session vanished upstream; the session
// manager re-creates it). Only a CREATE 404 maps to "disabled".
func (c *Client) GetSession(ctx context.Context, instanceID string) (*SessionState, error) {
	return c.GetSessionWithOpts(ctx, instanceID, false)
}

// GetSessionWithOpts polls /api/v1/freebuff/session with an optional compact
// response header. There is deliberately NO heartbeat option: the CLI never
// sends x-freebuff-heartbeat (Desktop-only, reference/freebuff
// freebuff-models.ts:1212-1215); liveness comes from the recurring compact
// GET itself (gap #2).
func (c *Client) GetSessionWithOpts(ctx context.Context, instanceID string, compact bool) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	if instanceID != "" {
		req.Header.Set("x-freebuff-instance-id", instanceID)
	}
	if compact {
		req.Header.Set("x-freebuff-compact-session", "1")
	}
	return c.sessionCall(req)
}

// ProbeAccount validates the token with a zero-cost GET /api/v1/freebuff/session
// that carries NO x-freebuff-instance-id header, so unlike CreateSession it
// claims no session slot and burns none of the daily session allowance. The
// response carries the live per-model quota (RateLimitsByModel) plus the
// account/session state, which callers surface for token checks and doctor
// diagnostics.
//
// A probe 404 maps (via sessionCall) to Status "ended"; that — or a 200 with
// status "ended" — means the token has no active session, returned as
// (nil, ErrNoActiveSession). Terminal refusal statuses the upstream returns
// as session states (403 {"status":"banned"}/{"status":"country_blocked"})
// are converted to the same typed errors the session manager surfaces
// (ErrBanned / ErrCountryBlocked), so probe callers can distinguish a dead
// account from a healthy idle one. All other classifications pass through
// unchanged: 401 → ErrAuthRejected, 429 → ErrRateLimited, transport
// failures as-is. A 200 with any other status (active/queued/disabled/…)
// returns the full *SessionState.
func (c *Client) ProbeAccount(ctx context.Context) (*SessionState, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/freebuff/session", nil)
	if err != nil {
		return nil, err
	}
	state, err := c.sessionCall(req)
	if err != nil {
		return nil, err
	}
	switch state.Status {
	case "ended":
		return nil, ErrNoActiveSession
	case "banned":
		return nil, &BanError{ResumesAt: state.ResumesAt, Body: state.Message}
	case "country_blocked":
		return nil, &CountryBlockedError{
			CountryCode:        state.CountryCode,
			CountryBlockReason: state.CountryBlockReason,
			IpPrivacySignals:   state.IpPrivacySignals,
		}
	}
	return state, nil
}

// EndSession DELETE /api/v1/freebuff/session; 404 is tolerated. The DELETE
// is keyed on the user, not the instance: the CLI releases its slot with
// Authorization only, no x-freebuff-instance-id header (#120,
// reference/freebuff freebuff-session-api.ts releaseFreebuffSlot → DELETE).
func (c *Client) EndSession(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/api/v1/freebuff/session", nil)
	if err != nil {
		return err
	}

	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	bodyStr := drainBody(resp.Body)
	if resp.StatusCode == 404 {
		return nil // nothing to end
	}
	if resp.StatusCode >= 400 {
		return c.classify(resp.StatusCode, bodyStr, resp.Header)
	}
	return nil
}

// StartRun POSTs /api/v1/agent-runs with action START and returns the run id.
func (c *Client) StartRun(ctx context.Context, agentID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        agentID,
		"ancestorRunIds": []string{},
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", payload)
	if err != nil {
		return "", err
	}
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return "", err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return "", c.classify(resp.StatusCode, body, resp.Header)
	}
	var parsed struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", fmt.Errorf("upstream: parse START response %q: %w", truncate(body, 200), err)
	}
	if parsed.RunID == "" {
		return "", fmt.Errorf("upstream: START response missing runId: %q", truncate(body, 200))
	}
	return parsed.RunID, nil
}

// RunStep is one agent-run step, batched in memory and sent WITH FINISH
// (issue #114, CLI parity: reference/freebuff/sdk/src/impl/database.ts
// pendingAgentStepSchema — the CLI has NO /steps endpoint, so steps ride
// the FINISH payload). The proxy records one step per completed chat call.
type RunStep struct {
	// ID is a per-step UUID minted at record time.
	ID string `json:"id"`
	// StepNumber is the 1-based per-run step index (sequential 1,2,3…).
	StepNumber int `json:"stepNumber"`
	// Credits is always 0 for the proxy (the upstream account owns spend).
	Credits int `json:"credits,omitempty"`
	// ChildRunIDs is empty for proxy-recorded steps (child runs are
	// separate runs, not steps).
	ChildRunIDs []string `json:"childRunIds,omitempty"`
	// MessageID is the completed chat response id; null when the stream
	// never carried one (the CLI schema allows a null messageId).
	MessageID *string `json:"messageId"`
	// Status mirrors the CLI step lifecycle; proxy-recorded steps are
	// always "completed" (recorded only after a successful chat).
	Status string `json:"status,omitempty"`
	// StartTime is the step start instant, RFC3339Nano UTC.
	StartTime string `json:"startTime"`
}

// FinishRun POSTs /api/v1/agent-runs with action FINISH, reporting the
// run's honest terminal status and its completed steps (issue #114, CLI
// parity: reference/freebuff/sdk/src/impl/database.ts finishAgentRun — the
// full payload is sent in ONE request; there is no /steps endpoint).
// totalSteps is the step count the manager reports (len(steps) preferred,
// falling back to the request count when no steps were recorded);
// errorMessage is omitted when empty and truncated to 5000 runes otherwise,
// exactly like the CLI's truncateString(errorMessage, 5000).
func (c *Client) FinishRun(ctx context.Context, runID, status string, totalSteps int, steps []RunStep, errorMessage string) error {
	if steps == nil {
		steps = []RunStep{}
	}
	payload := map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        status,
		"totalSteps":    totalSteps,
		"directCredits": 0,
		"totalCredits":  0,
		"steps":         steps,
	}
	if errorMessage != "" {
		payload["errorMessage"] = truncateRunes(errorMessage, 5000)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", body)
	if err != nil {
		return err
	}

	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	bodyStr := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return c.classify(resp.StatusCode, bodyStr, resp.Header)
	}
	return nil
}

// StartChildRun POSTs /api/v1/agent-runs with action START for the
// context-pruner child of parentRunID (issue #91, CLI parity:
// reference/freebuff-reverse .../http.go createChildRun — agentId
// "context-pruner", ancestorRunIds [parent]). The child is created after a
// parent run is STARTed and FINISHed once the parent's session work closes,
// so the upstream run tree stays balanced. Returns the child run id.
func (c *Client) StartChildRun(ctx context.Context, parentRunID string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"action":         "START",
		"agentId":        "context-pruner",
		"ancestorRunIds": []string{parentRunID},
	})
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/agent-runs", payload)
	if err != nil {
		return "", err
	}
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return "", err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if resp.StatusCode >= 400 {
		return "", c.classify(resp.StatusCode, body, resp.Header)
	}
	var parsed struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", fmt.Errorf("upstream: parse child START response %q: %w", truncate(body, 200), err)
	}
	if parsed.RunID == "" {
		return "", fmt.Errorf("upstream: child START response missing runId: %q", truncate(body, 200))
	}
	return parsed.RunID, nil
}

// --- internals ---

// sessionCall performs a session control call: parse the JSON body into a
// SessionState; errors are classified through the standard matrix.
func (c *Client) sessionCall(req *http.Request) (*SessionState, error) {
	resp, cancel, err := c.do(req, c.sessionCallTimeout)
	if err != nil {
		return nil, err
	}
	defer releaseCancel(cancel)
	defer func() { _ = resp.Body.Close() }()
	body := drainBody(resp.Body)
	if resp.StatusCode == 404 {
		if req.Method == http.MethodPost {
			// A create 404 means no session slot exists upstream.
			return &SessionState{Status: "disabled"}, nil
		}
		// A poll 404 means the session no longer exists upstream (expired or
		// evicted). Treat it as ended so the session manager re-creates it,
		// instead of caching a permanent "disabled" with no expiry.
		return &SessionState{Status: "ended"}, nil
	}

	c.dump("session", req, resp.StatusCode, body)

	var raw struct {
		Status                 string                   `json:"status"`
		InstanceID             string                   `json:"instanceId"`
		Model                  string                   `json:"model"`
		CurrentModel           string                   `json:"currentModel"`
		RequestedModel         string                   `json:"requestedModel"`
		ExpiresAt              any                      `json:"expiresAt"`
		AdmittedAt             any                      `json:"admittedAt"`
		GracePeriodEndsAt      any                      `json:"gracePeriodEndsAt"`
		GracePeriodRemainingMs int64                    `json:"gracePeriodRemainingMs"`
		Position               int                      `json:"position"`
		QueueDepth             int                      `json:"queueDepth"`
		EstimatedWaitMs        int                      `json:"estimatedWaitMs"`
		PollAt                 any                      `json:"pollAt"`
		CountryCode            string                   `json:"countryCode"`
		CountryBlockReason     string                   `json:"countryBlockReason"`
		IpPrivacySignals       []string                 `json:"ipPrivacySignals"`
		ActiveUsersForIP       int                      `json:"activeUsersForIp"`
		Limit                  float64                  `json:"limit"`
		RecentCount            float64                  `json:"recentCount"`
		ResetAt                any                      `json:"resetAt"`
		ResumesAt              any                      `json:"resumes_at"`
		RetryAfterMs           int64                    `json:"retryAfterMs"`
		AvailableHours         string                   `json:"availableHours"`
		Message                string                   `json:"message"`
		GlmPromo               json.RawMessage          `json:"glmPromo"`
		RateLimitsByModel      map[string]rawModelQuota `json:"rateLimitsByModel"`
		Standing               *rawStanding             `json:"standing"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err == nil && raw.Status != "" {
		state := &SessionState{
			Status:             raw.Status,
			InstanceID:         raw.InstanceID,
			Model:              raw.Model,
			CurrentModel:       raw.CurrentModel,
			RequestedModel:     raw.RequestedModel,
			GraceRemainingMs:   raw.GracePeriodRemainingMs,
			Position:           raw.Position,
			QueueDepth:         raw.QueueDepth,
			EstimatedWaitMs:    raw.EstimatedWaitMs,
			CountryCode:        raw.CountryCode,
			CountryBlockReason: raw.CountryBlockReason,
			IpPrivacySignals:   raw.IpPrivacySignals,
			ActiveUsersForIP:   raw.ActiveUsersForIP,
			Limit:              raw.Limit,
			RecentCount:        raw.RecentCount,
			RetryAfterMs:       raw.RetryAfterMs,
			AvailableHours:     raw.AvailableHours,
			Message:            raw.Message,
			GlmPromo:           string(raw.GlmPromo),
		}
		if raw.Status == "model_unavailable" && raw.AvailableHours != "" {
			if w, ok := ParseAvailabilityWindow(raw.AvailableHours); ok {
				state.UnavailableWindow = &w
			}
		}
		if raw.Standing != nil {
			standing := &SessionStanding{
				Level:     raw.Standing.Level,
				Label:     raw.Standing.Label,
				Score:     raw.Standing.Score,
				NextLevel: raw.Standing.NextLevel,
			}
			if standing.NextLevelAt, err = parseFlexTime(raw.Standing.NextLevelAt); err != nil {
				standing.NextLevelAt = time.Time{}
			}
			state.Standing = standing
		}
		if state.ExpiresAt, err = parseFlexTime(raw.ExpiresAt); err != nil {
			state.ExpiresAt = time.Time{}
		}
		if state.AdmittedAt, err = parseFlexTime(raw.AdmittedAt); err != nil {
			state.AdmittedAt = time.Time{}
		}
		if state.GracePeriodEndsAt, err = parseFlexTime(raw.GracePeriodEndsAt); err != nil {
			state.GracePeriodEndsAt = time.Time{}
		}
		if state.PollAt, err = parseFlexTime(raw.PollAt); err != nil {
			state.PollAt = time.Time{}
		}
		if state.ResetAt, err = parseFlexTime(raw.ResetAt); err != nil {
			state.ResetAt = time.Time{}
		}
		if state.ResumesAt, err = parseFlexTime(raw.ResumesAt); err != nil {
			state.ResumesAt = time.Time{}
		}
		if len(raw.RateLimitsByModel) > 0 {
			state.RateLimitsByModel = make(map[string]ModelQuota, len(raw.RateLimitsByModel))
			for modelID, q := range raw.RateLimitsByModel {
				mq := ModelQuota{
					Model:       q.Model,
					Limit:       q.Limit,
					RecentCount: q.RecentCount,
					Period:      q.Period,
					Entitlement: q.EntitlementBreakdown,
				}
				if mq.Model == "" {
					mq.Model = modelID
				}
				if resetAt, perr := parseFlexTime(q.ResetAt); perr == nil {
					mq.ResetAt = resetAt
				}
				state.RateLimitsByModel[modelID] = mq
			}
		}
		// Feed the passive ban-risk engine (#64): ipPrivacySignals and the
		// ip_capped activeUsersForIp/limit arrive on the session admission
		// and probe responses. Read-only — the engine only warns.
		if c.risk != nil && (len(state.IpPrivacySignals) > 0 ||
			state.ActiveUsersForIP > 0 || state.Limit > 0 || state.CountryCode != "") {
			c.risk.Observe(stealth.RiskSample{
				At:               time.Now(),
				Country:          state.CountryCode,
				IPPrivacySignals: state.IpPrivacySignals,
				ActiveUsersForIP: state.ActiveUsersForIP,
				Limit:            state.Limit,
			})
		}
		return state, nil
	}

	if resp.StatusCode >= 400 {
		return nil, c.classify(resp.StatusCode, body, resp.Header)
	}

	return nil, fmt.Errorf("upstream: unparseable session response %q", truncate(body, 200))
}
