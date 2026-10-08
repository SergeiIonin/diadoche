package restarter

import (
	"testing"
	"time"
)

func TestIsLimitLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`2026/09/24 10:00:00 task 001_x.md: error: planner: agent planner: exit status 1: {"type":"result","is_error":true,"result":"You've hit your session limit · resets 7pm (Europe/Moscow)"}`, true},
		{`{"type":"result","is_error":true,"result":"Claude AI usage limit reached|1759770000"}`, true},
		{`task 001_x.md: error: worker: You've hit your weekly limit · resets Oct 7, 5pm`, true},
		{`task 001_x.md: error: worker: You've hit your limit · resets 3am`, true},
		// Talking about limits in a successful answer is not a limit hit.
		{`the agent said: You've hit your session limit · resets 7pm`, false},
		// Waiting doesn't lift spend limits / budgets.
		{`error: You've hit your monthly spend limit.`, false},
		{`error: You've hit your team's shared budget. Switch to another model`, false},
		{`task 001_x.md: error: planner: exit status 1: overloaded`, false},
		// diadoche's own in-loop pause log lines (orchestrator.runAgentPausing,
		// processTask) are not limit lines: a pausing diadoche is left alone.
		{`2026/09/24 10:00:00 001_x.md iter 1/3: [planner] paused: claude usage window exhausted (until 2026-09-24T19:00:00Z), retrying the same call in 4h0m0s`, false},
		{`2026/09/24 10:00:00 001_x.md iter 1/3: [worker] paused: claude rate limited (backoff), retrying the same call in 5m0s`, false},
		{`2026/09/24 10:00:00 task 001_x.md: interrupted while paused; it stays PAUSED and is resumed by the next run`, false},
		{`2026/09/24 10:00:00 task 001_x.md: error: interrupted while paused: context canceled`, false},
	}
	for _, c := range cases {
		if got := IsLimitLine(c.line); got != c.want {
			t.Errorf("IsLimitLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestParseResetTime(t *testing.T) {
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	now := time.Date(2026, 9, 24, 14, 10, 0, 0, moscow)

	cases := []struct {
		name string
		line string
		want time.Time
	}{
		{"time with tz", `"result":"You've hit your session limit · resets 7pm (Europe/Moscow)"`, time.Date(2026, 9, 24, 19, 0, 0, 0, moscow)},
		{"minutes, progress saved", `You've hit your session limit · resets 7:30pm (Europe/Moscow) · progress saved"`, time.Date(2026, 9, 24, 19, 30, 0, 0, moscow)},
		{"earlier today means tomorrow", `You've hit your session limit · resets 2am (Europe/Moscow)`, time.Date(2026, 9, 25, 2, 0, 0, 0, moscow)},
		{"just passed is still today", `You've hit your session limit · resets 2:08pm (Europe/Moscow)`, time.Date(2026, 9, 24, 14, 8, 0, 0, moscow)},
		{"other tz", `You've hit your session limit · resets 5pm (Europe/Berlin)`, time.Date(2026, 9, 24, 18, 0, 0, 0, moscow)},
		{"no tz uses local", `You've hit your limit · resets 6pm`, time.Date(2026, 9, 24, 18, 0, 0, 0, moscow)},
		{"date", `You've hit your weekly limit · resets Oct 7, 5pm (Europe/Moscow)`, time.Date(2026, 10, 7, 17, 0, 0, 0, moscow)},
		{"date with year", `You've hit your weekly limit · resets Jan 3, 2027, 9:15am (Europe/Moscow)`, time.Date(2027, 1, 3, 9, 15, 0, 0, moscow)},
		{"relative", `You've hit your fast limit · resets in 2h 30m`, now.Add(150 * time.Minute)},
		{"legacy epoch", `Claude AI usage limit reached|1790000000`, time.Unix(1790000000, 0)},
		{"escaped separator", `resets 8pm (Europe/Moscow) · progress saved`, time.Date(2026, 9, 24, 20, 0, 0, 0, moscow)},
	}
	for _, c := range cases {
		got, ok := ParseResetTime(c.line, now)
		if !ok {
			t.Errorf("%s: ParseResetTime(%q) not ok", c.name, c.line)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%s: ParseResetTime(%q) = %s, want %s", c.name, c.line, got, c.want)
		}
	}

	if _, ok := ParseResetTime(`You've hit your session limit`, now); ok {
		t.Error("expected no reset time in a message without one")
	}
}

func TestParseResetTimeNextYearDate(t *testing.T) {
	now := time.Date(2026, 12, 30, 12, 0, 0, 0, time.UTC)
	got, ok := ParseResetTime(`You've hit your weekly limit · resets Jan 2, 10am (UTC)`, now)
	want := time.Date(2027, 1, 2, 10, 0, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Errorf("got %s ok=%v, want %s", got, ok, want)
	}
}
