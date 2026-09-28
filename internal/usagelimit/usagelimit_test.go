package usagelimit

import "testing"

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
