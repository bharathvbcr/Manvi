//go:build linux

package codingagent

import "testing"

// Pure-parser edge cases for parseProcStat. Compile-checked here only
// (GOOS=linux go vet); the logic runs in CI.
func TestAdvW18ProcgroupParseProcStatEdges(t *testing.T) {
	tail := " S 1 777 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10"
	cases := []struct {
		name  string
		line  string
		pgrp  int
		start int64
		ok    bool
	}{
		{"comm with newline", "4242 (a\nb)" + tail, 777, 987654, true},
		{"comm with close paren and spaces", "4242 (x) y) z)" + tail, 777, 987654, true},
		{"comm empty", "4242 ()" + tail, 777, 987654, true},
		{"no paren", "4242 x S 1 777", 0, 0, false},
		{"exactly 19 fields after comm", "4242 (x) S 1 777 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1", 0, 0, false},
		{"start field non-numeric", "4242 (x) S 1 777 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 abc", 0, 0, false},
		{"pgrp non-numeric", "4242 (x) S 1 zz 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 5", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tc := range cases {
		pgrp, start, ok := parseProcStat(tc.line)
		if ok != tc.ok || (ok && (pgrp != tc.pgrp || start != tc.start)) {
			t.Errorf("%s: got %d, %d, %v want %d, %d, %v", tc.name, pgrp, start, ok, tc.pgrp, tc.start, tc.ok)
		}
	}
}
