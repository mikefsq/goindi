package client

import (
	"math"
	"testing"
)

// TestParseNumber checks the decimal and sexagesimal spellings ParseNumber
// accepts and rejects.
func TestParseNumber(t *testing.T) {
	almost := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	good := map[string]float64{
		"12:34:56.7": 12 + 34.0/60 + 56.7/3600,
		"-5 30 00":   -5.5,
		"3.14":       3.14,
		"  1:30  ":   1.5,
		"12:30:00":   12.5,
		"12 30 00":   12.5,
		"-12:30:00":  -12.5,
		"-0:30":      -0.5,
		"5.25":       5.25,
		"12:45":      12.75,
		"-12 45 3.6": -(12 + 45.0/60 + 3.6/3600),
	}
	for in, want := range good {
		got, ok := ParseNumber(in)
		if !ok || !almost(got, want) {
			t.Errorf("ParseNumber(%q) = %v,%v want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "not a number", "1:2:3:4", "12:", "12:xx", "--5 30", "1 2 zz"} {
		if got, ok := ParseNumber(in); ok {
			t.Errorf("ParseNumber(%q) = %v, want rejection", in, got)
		}
	}
}
