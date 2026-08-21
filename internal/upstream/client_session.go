// Shared wire/response helpers for the session, rate-limit, and chat
// response paths: JSON field extraction (getNumber/getTime), error-detail
// formatting (retryDetail/containsAny/unixFrom), body draining and
// truncation (drainBody/truncate/truncateRunes), and the debug dump writer
// (dump/sanitizeName).
package upstream

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func getNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				return n, true
			case int:
				return float64(n), true
			case int64:
				return float64(n), true
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case string:
				val = strings.TrimSpace(val)
				if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
					return t, true
				}
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					return t, true
				}
			case float64:
				if val > 1e11 { // milliseconds
					return time.UnixMilli(int64(val)).UTC(), true
				} else if val > 0 {
					return time.Unix(int64(val), 0).UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}

func retryDetail(retryAfter time.Duration) string {
	if retryAfter > 0 {
		return fmt.Sprintf(" (Retry-After %s)", retryAfter)
	}
	return ""
}

func containsAny(lower string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func unixFrom(secs int64) time.Time {
	// Heuristic: milliseconds if 10^12 or larger, else seconds.
	if secs >= 100_000_000_000 {
		return time.Unix(0, secs*int64(time.Millisecond))
	}
	return time.Unix(secs, 0)
}

func drainBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxDumpRead))
	return string(data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateRunes truncates s to at most max runes without an ellipsis. The
// CLI's FINISH errorMessage cap is 5000 chars (truncateString in
// reference/freebuff/sdk/src/impl/database.ts), applied on the whole
// payload — a full Go stack trace must not blow the cap.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// dump writes a debug record to dump/ when enabled.
func (c *Client) dump(kind string, req *http.Request, status int, body string) {
	if !c.debugDump {
		return
	}
	name := fmt.Sprintf("%s-%d-%s.dump", kind, time.Now().UnixNano(), sanitizeName(req.URL.Path))
	path := filepath.Join("dump", name)
	_ = os.MkdirAll("dump", 0o755)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s\n", req.Method, req.URL.String())
	for k, vs := range req.Header {
		for _, v := range vs {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "x-codebuff-api-key") {
				v = "[redacted]"
			}
			fmt.Fprintf(&buf, "%s: %s\n", k, v)
		}
	}
	fmt.Fprintf(&buf, "\n[status %d]\n%s\n", status, truncate(body, 20000))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		// T18: the write was previously swallowed (`_ = os.WriteFile`) —
		// surface the failure so a broken dump dir is not silent.
		slog.Warn("debug dump write failed", "path", path, "err", err)
	}
}

func sanitizeName(p string) string {
	p = strings.ReplaceAll(p, "/", "_")
	p = strings.ReplaceAll(p, ".", "_")
	if len(p) > 60 {
		p = p[:60]
	}
	return p
}
