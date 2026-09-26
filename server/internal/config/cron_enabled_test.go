package config

import "testing"

func TestCronEnabledFlags(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"off", false},
		{"", false},
		{"none", false},
		{"disabled", false},
		{"OFF", false},
		{"0 9 * * *", true},
		{"@every 5s", true},
	}
	for _, c := range cases {
		cfg := &Config{PayrollCron: c.expr, ChatCron: c.expr}
		if got := cfg.PayrollCronEnabled(); got != c.want {
			t.Errorf("PayrollCronEnabled(%q) = %v, want %v", c.expr, got, c.want)
		}
		if got := cfg.ChatCronEnabled(); got != c.want {
			t.Errorf("ChatCronEnabled(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}
