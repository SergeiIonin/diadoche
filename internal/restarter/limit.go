package restarter

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// limitRe matches the claude CLI's "account usage window exhausted"
// messages, in both the current wording ("You've hit your session limit ·
// resets 7pm (Europe/Moscow)", "You've hit your weekly limit · resets
// Oct 7, 5pm (...)") and the legacy one ("Claude AI usage limit
// reached|1759770000").
var limitRe = regexp.MustCompile(`(?i)you[’']ve hit your (?:[\w’'-]+ )*?limit|usage limit reached|session limit reached|weekly limit reached`)

// epochRe extracts a reset time given as a unix timestamp: the legacy
// "usage limit reached|<epoch>" format, or a resets_at/resetsAt field.
var epochRe = regexp.MustCompile(`(?i)(?:limit reached\||resets_?at"?\s*[:=]\s*)(\d{10})`)

// tzRe splits a trailing "(Europe/Moscow)" off a reset time expression.
var tzRe = regexp.MustCompile(`^(.*?)\s*\(([^)]*)\)\s*$`)

// inRe matches a relative reset time: "in 2h 30m", "in 45m", "in 3h".
var inRe = regexp.MustCompile(`^in\s+(?:(\d+)\s*d)?\s*(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?\s*(?:(\d+)\s*s)?$`)

// IsLimitLine reports whether line is a claude usage-limit error. The line
// must also mention "error" (diadoche's own "task X: error: ..." log line,
// or the claude CLI's `"is_error":true` JSON), so an agent that merely
// talks about limits in a successful answer never triggers a restart.
// Spend limits and shared budgets are excluded: waiting doesn't lift them.
func IsLimitLine(line string) bool {
	m := limitRe.FindString(line)
	if m == "" {
		return false
	}
	lower := strings.ToLower(m)
	if strings.Contains(lower, "spend") || strings.Contains(lower, "budget") {
		return false
	}
	return strings.Contains(strings.ToLower(line), "error")
}

// ParseResetTime extracts the moment the usage window reopens from a limit
// line (see IsLimitLine). ok is false when the line carries no reset time
// diadoche-restarter understands.
func ParseResetTime(line string, now time.Time) (t time.Time, ok bool) {
	if m := epochRe.FindStringSubmatch(line); m != nil {
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil {
			return time.Unix(sec, 0), true
		}
	}

	idx := strings.Index(strings.ToLower(line), "resets ")
	if idx < 0 {
		return time.Time{}, false
	}
	expr := line[idx+len("resets "):]
	// The reset expression ends at the next "·" separator (e.g. before
	// "· progress saved"), the end of the JSON string, or the end of line.
	for _, stop := range []string{"·", `·`, `"`, `\n`, "\n"} {
		if i := strings.Index(expr, stop); i >= 0 {
			expr = expr[:i]
		}
	}
	expr = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(expr), "at "))
	expr = strings.TrimRight(expr, ".")

	loc := now.Location()
	if m := tzRe.FindStringSubmatch(expr); m != nil {
		expr = m[1]
		if l, err := time.LoadLocation(strings.TrimSpace(m[2])); err == nil {
			loc = l
		}
	}
	return parseResetExpr(strings.ToLower(strings.TrimSpace(expr)), now, loc)
}

// Layouts the claude CLI uses for reset times (after lowercasing (month names still match: Go parses them case-insensitively) and
// removing the space before am/pm): "7pm", "7:30pm" within the next 24h,
// "oct 7, 5pm" beyond that, with the year added when it differs.
var (
	timeLayouts     = []string{"3pm", "3:04pm", "15:04"}
	dateLayouts     = []string{"Jan 2, 3pm", "Jan 2, 3:04pm", "Jan 2 3pm", "Jan 2 3:04pm", "Jan 2"}
	fullDateLayouts = []string{"Jan 2, 2006, 3pm", "Jan 2, 2006, 3:04pm", "Jan 2, 2006", "2006-01-02 15:04", "2006-01-02"}
)

func parseResetExpr(expr string, now time.Time, loc *time.Location) (time.Time, bool) {
	if m := inRe.FindStringSubmatch(expr); m != nil {
		var d time.Duration
		for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
			if m[i+1] != "" {
				n, _ := strconv.Atoi(m[i+1])
				d += time.Duration(n) * unit
			}
		}
		if d > 0 {
			return now.Add(d), true
		}
	}

	expr = strings.NewReplacer(" am", "am", " pm", "pm", " ", "").Replace(expr)
	// A trailing zone abbreviation ("00:00 utc") is dropped: the wall clock
	// is then read in loc, which is the best that can be done without it.
	if i := strings.LastIndex(expr, " "); i >= 0 {
		if l, err := time.LoadLocation(strings.ToUpper(expr[i+1:])); err == nil {
			loc = l
			expr = expr[:i]
		}
	}
	nowIn := now.In(loc)

	for _, layout := range fullDateLayouts {
		if t, err := time.ParseInLocation(layout, expr, loc); err == nil {
			return t, true
		}
	}
	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, expr, loc); err == nil {
			t = time.Date(nowIn.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			// "jan 3" seen on dec 30 is next year's jan 3.
			if t.Before(now.Add(-24 * time.Hour)) {
				t = t.AddDate(1, 0, 0)
			}
			return t, true
		}
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, expr, loc); err == nil {
			t = time.Date(nowIn.Year(), nowIn.Month(), nowIn.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			// A bare time is always the next occurrence of it. A few
			// minutes' slack keeps "resets 7pm" read at 7:00:30pm (clock
			// skew, minutes rounded away) from turning into tomorrow.
			if t.Before(now.Add(-5 * time.Minute)) {
				t = t.AddDate(0, 0, 1)
			}
			return t, true
		}
	}
	return time.Time{}, false
}
