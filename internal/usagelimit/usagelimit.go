// Package usagelimit recognizes the claude CLI's "account usage window
// exhausted" and rate-limit errors. diadoche-restarter uses it to decide
// when to restart diadoche; diadoche itself uses Detect to pause a task
// in place (without spending an iteration) instead of failing it, and
// ParseResetTime to learn when the window reopens.
package usagelimit

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// limitRe matches the claude CLI's usage-limit messages, in both the
// current wording ("You've hit your session limit · resets 7pm
// (Europe/Moscow)", "You've hit your weekly limit · resets Oct 7, 5pm
// (...)") and the legacy one ("Claude AI usage limit reached|1759770000").
var limitRe = regexp.MustCompile(`(?i)you[’']ve hit your (?:[\w’'-]+ )*?limit|usage limit reached|session limit reached|weekly limit reached`)

// Mentions reports whether any line of text carries a usage-limit message.
// Spend limits and shared budgets are excluded: waiting doesn't lift them.
func Mentions(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if lineMentions(line) {
			return true
		}
	}
	return false
}

// IsLimitLine reports whether line is a claude usage-limit error. The line
// must also mention "error" (diadoche's own "task X: error: ..." log line,
// or the claude CLI's `"is_error":true` JSON), so an agent that merely
// talks about limits in a successful answer never triggers a restart.
func IsLimitLine(line string) bool {
	return lineMentions(line) && strings.Contains(strings.ToLower(line), "error")
}

// Kind is the kind of limit a failed claude call ran into.
type Kind int

const (
	// KindSubscription is the subscription's usage window being exhausted.
	KindSubscription Kind = iota + 1
	// KindRateLimit is the API rate limiting the caller (HTTP 429).
	KindRateLimit
)

func (k Kind) String() string {
	switch k {
	case KindSubscription:
		return "usage window exhausted"
	case KindRateLimit:
		return "rate limited"
	}
	return "unknown"
}

// Hit describes a limit refusal recognized by Detect.
type Hit struct {
	Kind Kind
	// ResetAt is when the limit lifts; valid only when HasReset.
	ResetAt  time.Time
	HasReset bool
}

// rateRe matches rate-limit refusals: "rate limit", "rate_limit_error",
// "rate-limited", "Too Many Requests", HTTP status 429.
var rateRe = regexp.MustCompile(`(?i)rate[ _-]?limit|too many requests|api error:\s*429|"status"\s*:\s*429|status code 429`)

// isErrorRe matches the claude CLI's JSON failure marker.
var isErrorRe = regexp.MustCompile(`"is_error"\s*:\s*true`)

// Detect reports whether text (the error and output of a claude call that
// exited with exitCode) is a refusal by a usage or rate limit, as opposed
// to any other agent failure. Only failed calls qualify: exitCode must be
// non-zero or text must carry the CLI's `"is_error":true`, so an answer
// that merely talks about limits is never taken for one. Spend limits and
// shared budgets are excluded: waiting doesn't lift them. now anchors
// relative reset times ("resets 7pm").
func Detect(text string, exitCode int, now time.Time) (Hit, bool) {
	if exitCode == 0 && !isErrorRe.MatchString(text) {
		return Hit{}, false
	}
	var hit Hit
	var matched []string
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case lineMentions(line):
			if hit.Kind != KindSubscription {
				hit.Kind = KindSubscription
				matched = nil
			}
			matched = append(matched, line)
		case hit.Kind != KindSubscription && rateLineMentions(line):
			hit.Kind = KindRateLimit
			matched = append(matched, line)
		}
	}
	if hit.Kind == 0 {
		return Hit{}, false
	}
	for _, line := range matched {
		if t, ok := ParseResetTime(line, now); ok {
			hit.ResetAt, hit.HasReset = t, true
			break
		}
	}
	return hit, true
}

func rateLineMentions(line string) bool {
	if !rateRe.MatchString(line) {
		return false
	}
	lower := strings.ToLower(line)
	return !strings.Contains(lower, "spend") && !strings.Contains(lower, "budget")
}

func lineMentions(line string) bool {
	m := limitRe.FindString(line)
	if m == "" {
		return false
	}
	lower := strings.ToLower(m)
	return !strings.Contains(lower, "spend") && !strings.Contains(lower, "budget")
}

// epochRe extracts a reset time given as a unix timestamp: the legacy
// "usage limit reached|<epoch>" format, or a resets_at/resetsAt field.
var epochRe = regexp.MustCompile(`(?i)(?:limit reached\||resets_?at"?\s*[:=]\s*)(\d{10})`)

// tzRe splits a trailing "(Europe/Moscow)" off a reset time expression.
var tzRe = regexp.MustCompile(`^(.*?)\s*\(([^)]*)\)\s*$`)

// inRe matches a relative reset time: "in 2h 30m", "in 45m", "in 3h".
var inRe = regexp.MustCompile(`^in\s+(?:(\d+)\s*d)?\s*(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?\s*(?:(\d+)\s*s)?$`)

// ParseResetTime extracts the moment the usage window reopens from a limit
// text (a limit line, see IsLimitLine). ok is false when it carries no reset
// time this package understands.
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
