package upstream

import (
	"testing"
	"time"
)

func TestParseAvailabilityWindow(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		wantOK bool
		start  int
		end    int
	}{
		{"24h window", "08:00-20:00", true, 8 * 60, 20 * 60},
		{"12h ET/PT window", "9am ET-5pm PT every day", true, 6 * 60, 17 * 60},
		{"12h no zone", "9am-5pm", true, 9 * 60, 17 * 60},
		{"overnight 24h", "22:00-06:00", true, 22 * 60, 6 * 60},
		{"midnight meridiem", "12am-12pm", true, 0, 12 * 60},
		{"minutes included", "9:30am ET-5:15pm PT", true, 6*60 + 30, 17*60 + 15},
		{"garbage", "not a window", false, 0, 0},
		{"empty", "", false, 0, 0},
		{"single time", "9am", false, 0, 0},
		{"hour out of range", "25:00-26:00", false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, ok := ParseAvailabilityWindow(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if w.StartMinute != tt.start || w.EndMinute != tt.end {
				t.Errorf("window = [%d,%d), want [%d,%d)", w.StartMinute, w.EndMinute, tt.start, tt.end)
			}
			if w.Raw != tt.in {
				t.Errorf("Raw = %q, want %q", w.Raw, tt.in)
			}
		})
	}
}

func TestAvailabilityWindowAvailability(t *testing.T) {
	// 9am ET-5pm PT = 06:00-17:00 Pacific. August → PDT (UTC-7).
	w, ok := ParseAvailabilityWindow("9am ET-5pm PT every day")
	if !ok {
		t.Fatal("window did not parse")
	}
	cases := []struct {
		utc  string
		want bool
	}{
		{"2026-08-21T12:00:00Z", false}, // 05:00 PDT: before open
		{"2026-08-21T13:00:00Z", true},  // 06:00 PDT: window open
		{"2026-08-21T23:30:00Z", true},  // 16:30 PDT: window open
		{"2026-08-22T00:00:00Z", false}, // 17:00 PDT: window closed
		{"2026-08-22T11:30:00Z", false}, // 04:30 PDT: pre-dawn, closed
	}
	for _, c := range cases {
		now, err := time.Parse(time.RFC3339, c.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.AvailableAt(now); got != c.want {
			t.Errorf("AvailableAt(%s) = %v, want %v", c.utc, got, c.want)
		}
	}
}

func TestAvailabilityWindowNextStart(t *testing.T) {
	w, ok := ParseAvailabilityWindow("9am ET-5pm PT every day") // 06:00-17:00 PDT
	if !ok {
		t.Fatal("window did not parse")
	}
	cases := []struct {
		utc  string
		want string
	}{
		// 05:30 PDT, before the daily open: today's 06:00 PDT.
		{"2026-08-21T12:30:00Z", "2026-08-21T13:00:00Z"},
		// 17:30 PDT, after the close: tomorrow's 06:00 PDT.
		{"2026-08-22T00:30:00Z", "2026-08-22T13:00:00Z"},
	}
	for _, c := range cases {
		now, err := time.Parse(time.RFC3339, c.utc)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse(time.RFC3339, c.want)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.NextStart(now); !got.Equal(want) {
			t.Errorf("NextStart(%s) = %s, want %s", c.utc, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}

	// Overnight window: 22:00-06:00 Pacific. 2026-08-21T03:00:00Z is Aug 20
	// 20:00 PDT — the next opening is Aug 20 22:00 PDT; 2026-08-21T14:00:00Z
	// is Aug 21 07:00 PDT (just after the 06:00 close) — the next opening is
	// Aug 21 22:00 PDT.
	ow, ok := ParseAvailabilityWindow("22:00-06:00")
	if !ok {
		t.Fatal("overnight window did not parse")
	}
	if got := ow.NextStart(time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 8, 20, 22, 0, 0, 0, pacificLoc())) {
		t.Errorf("overnight NextStart(20:00 PDT Aug 20) = %s, want Aug 20 22:00 PDT", got.Format(time.RFC3339))
	}
	if got := ow.NextStart(time.Date(2026, 8, 21, 14, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 8, 21, 22, 0, 0, 0, pacificLoc())) {
		t.Errorf("overnight NextStart(07:00 PDT Aug 21) = %s, want Aug 21 22:00 PDT", got.Format(time.RFC3339))
	}

	// Degenerate window (24-7): never skips, no future opening.
	dw := AvailabilityWindow{StartMinute: 0, EndMinute: 0}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if !dw.AvailableAt(now) {
		t.Error("degenerate window must always be available")
	}
	if !dw.NextStart(now).Equal(now) {
		t.Error("degenerate window NextStart must return now")
	}
}
