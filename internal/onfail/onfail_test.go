package onfail

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{DefaultChain, []string{Escalate, Block}},
		{"", nil},
		{"   ", nil},
		{"block", []string{Block}},
		{"escalate", []string{Escalate}},
		{" escalate , block ", []string{Escalate, Block}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		in       string
		contains []string
	}{
		{"bogus", []string{`"bogus"`, "accepted: escalate, block"}},
		{"escalate,bogus", []string{`"bogus"`, "escalate", "block"}},
		{"escalate,,block", []string{"empty"}},
		{"escalate,escalate", []string{"more than once"}},
		{"block,escalate", []string{"last"}},
		{"block,block", []string{"more than once"}},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q): no error", c.in)
			continue
		}
		for _, s := range c.contains {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("Parse(%q) error %q does not mention %q", c.in, err, s)
			}
		}
	}
}

func TestDefaultChainParsesToEveryName(t *testing.T) {
	got, err := Parse(DefaultChain)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, Names) {
		t.Errorf("default chain %v, names %v", got, Names)
	}
	if !IsTerminal(Block) || IsTerminal(Escalate) {
		t.Error("only block is terminal")
	}
}
