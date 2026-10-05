package config

import "testing"

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"1024", 1024, true},
		{"8MiB", 8 << 20, true},
		{"20G", 20 << 30, true},
		{"5GB", 5 << 30, true},
		{"-1", 0, false},
		{"abc", 0, false},
		// Overflow must be rejected, not wrapped to a negative (= "unlimited") budget.
		{"9999999999T", 0, false},
		{"9223372036854775807K", 0, false},
	}
	for _, c := range cases {
		got, err := parseBytes(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("parseBytes(%q) = %d, %v; want %d ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestEnvDurationRejectsOverflow(t *testing.T) {
	t.Setenv("X_TEST_DURATION", "99999999999999999")
	if got := envDuration("X_TEST_DURATION", 7); got != 7 {
		t.Fatalf("overflowing seconds should fall back to the default, got %v", got)
	}
}
