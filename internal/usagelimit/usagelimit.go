// Package usagelimit recognizes the claude CLI's "account usage window
// exhausted" errors. diadoche-restarter uses it to decide when to restart
// diadoche; diadoche itself uses it to tell a usage-limit interruption
// (resumed after the restart) apart from a genuinely failed task.
package usagelimit

import (
	"regexp"
	"strings"
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

func lineMentions(line string) bool {
	m := limitRe.FindString(line)
	if m == "" {
		return false
	}
	lower := strings.ToLower(m)
	return !strings.Contains(lower, "spend") && !strings.Contains(lower, "budget")
}
