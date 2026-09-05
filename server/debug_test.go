package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestDebugGating(t *testing.T) {
	for _, tc := range []struct {
		debug       bool
		wantTraffic bool
	}{
		{debug: false, wantTraffic: false},
		{debug: true, wantTraffic: true},
	} {
		var lines []string
		capture := func(f string, a ...any) { lines = append(lines, strings.TrimSpace(fmt.Sprintf(f, a...))) }
		s := New(":0", WithLogger(capture), WithDebug(tc.debug))

		s.log("indi: lifecycle event")
		s.dlog("indi: <- traffic message")

		gotLifecycle := containsPrefix(lines, "indi: lifecycle")
		gotTraffic := containsPrefix(lines, "indi: <- traffic")

		if !gotLifecycle {
			t.Errorf("debug=%v: lifecycle log missing (got %v)", tc.debug, lines)
		}
		if gotTraffic != tc.wantTraffic {
			t.Errorf("debug=%v: traffic logged=%v, want %v (got %v)", tc.debug, gotTraffic, tc.wantTraffic, lines)
		}
	}
}

func containsPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
