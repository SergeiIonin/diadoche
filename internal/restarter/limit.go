package restarter

import (
	"time"

	"diadoche/internal/usagelimit"
)

// IsLimitLine reports whether line is a claude usage-limit error; see
// usagelimit.IsLimitLine.
func IsLimitLine(line string) bool { return usagelimit.IsLimitLine(line) }

// ParseResetTime extracts the moment the usage window reopens from a limit
// line (see IsLimitLine); see usagelimit.ParseResetTime.
func ParseResetTime(line string, now time.Time) (time.Time, bool) {
	return usagelimit.ParseResetTime(line, now)
}
