package usagelimit

import (
	"testing"
	"time"
)

func TestMentions(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"agent worker: exit status 1: ...\n{\"is_error\":true,\"result\":\"You've hit your session limit · resets 7pm (UTC)\"}", true},
		{"Claude AI usage limit reached|1759770000", true},
		{"You've hit your spend limit", false},
		{"agent worker: exit status 1: panic: nil pointer", false},
	}
	for _, c := range cases {
		if got := Mentions(c.text); got != c.want {
			t.Errorf("Mentions(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestDetect(t *testing.T) {
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		text     string
		exitCode int
		ok       bool
		kind     Kind
		reset    time.Time // zero = no reset time expected
	}{
		{
			name:     "session limit with reset",
			text:     `agent planner: exit status 1: {"type":"result","is_error":true,"result":"You've hit your session limit · resets 7pm (UTC)"}`,
			exitCode: 1, ok: true, kind: KindSubscription,
			reset: time.Date(2026, 10, 8, 19, 0, 0, 0, time.UTC),
		},
		{
			name:     "legacy epoch",
			text:     "agent worker: exit status 1: Claude AI usage limit reached|1790000000",
			exitCode: 1, ok: true, kind: KindSubscription,
			reset: time.Unix(1790000000, 0),
		},
		{
			name:     "rate limit 429",
			text:     `agent tester: exit status 1: API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`,
			exitCode: 1, ok: true, kind: KindRateLimit,
		},
		{
			name:     "too many requests",
			text:     "agent worker: exit status 1: 429 Too Many Requests",
			exitCode: 1, ok: true, kind: KindRateLimit,
		},
		{
			name:     "is_error with exit 0",
			text:     `{"type":"result","is_error": true,"result":"You've hit your weekly limit"}`,
			exitCode: 0, ok: true, kind: KindSubscription,
		},
		{
			name:     "subscription wins over rate limit",
			text:     "rate limited\nYou've hit your session limit · resets in 2h",
			exitCode: 1, ok: true, kind: KindSubscription,
			reset: now.Add(2 * time.Hour),
		},
		{
			name:     "successful answer talking about limits",
			text:     "You've hit your session limit · resets 7pm (UTC)",
			exitCode: 0, ok: false,
		},
		{name: "spend limit", text: "You've hit your monthly spend limit.", exitCode: 1, ok: false},
		{name: "shared budget rate limit", text: "rate limit of your team's shared budget", exitCode: 1, ok: false},
		{name: "ordinary failure", text: "agent worker: exit status 1: panic: nil pointer", exitCode: 1, ok: false},
	}
	for _, c := range cases {
		hit, ok := Detect(c.text, c.exitCode, now)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if hit.Kind != c.kind {
			t.Errorf("%s: kind = %v, want %v", c.name, hit.Kind, c.kind)
		}
		if c.reset.IsZero() {
			if hit.HasReset {
				t.Errorf("%s: unexpected reset time %s", c.name, hit.ResetAt)
			}
		} else if !hit.HasReset || !hit.ResetAt.Equal(c.reset) {
			t.Errorf("%s: reset = %s (has=%v), want %s", c.name, hit.ResetAt, hit.HasReset, c.reset)
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
		{"date", `You've hit your weekly limit · resets Oct 7, 5pm (Europe/Moscow)`, time.Date(2026, 10, 7, 17, 0, 0, 0, moscow)},
		{"date with year", `You've hit your weekly limit · resets Jan 3, 2027, 9:15am (Europe/Moscow)`, time.Date(2027, 1, 3, 9, 15, 0, 0, moscow)},
		{"relative", `You've hit your fast limit · resets in 2h 30m`, now.Add(150 * time.Minute)},
		{"legacy epoch", `Claude AI usage limit reached|1790000000`, time.Unix(1790000000, 0)},
	}
	for _, c := range cases {
		got, ok := ParseResetTime(c.line, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("%s: ParseResetTime(%q) = %s ok=%v, want %s", c.name, c.line, got, ok, c.want)
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
