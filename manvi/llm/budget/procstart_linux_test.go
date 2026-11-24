//go:build linux

package budget

import "testing"

func TestParseStartTicksCountsFromAfterTheLastParenthesis(t *testing.T) {
	stat := []byte("123 (a b) c) S 1 123 123 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615")
	got, ok := parseStartTicks(stat)
	if !ok || got != 987654 {
		t.Fatalf("parseStartTicks = %d, %v; want 987654", got, ok)
	}
	if _, ok := parseStartTicks([]byte("123 (short) S 1")); ok {
		t.Fatal("a truncated stat line produced a start time")
	}
	if _, ok := parseStartTicks([]byte("no parenthesis")); ok {
		t.Fatal("a malformed stat line produced a start time")
	}
}

func TestParseBootTime(t *testing.T) {
	got, ok := parseBootTime([]byte("cpu 1 2 3\nbtime 1700000000\nprocesses 5\n"))
	if !ok || got != 1_700_000_000 {
		t.Fatalf("parseBootTime = %d, %v", got, ok)
	}
	if _, ok := parseBootTime([]byte("cpu 1 2 3\n")); ok {
		t.Fatal("no btime line produced a boot time")
	}
}
