//go:build linux

package codingagent

import "testing"

func TestParseProcStatReadsFieldsFiveAndTwentyTwoPastACommWithParens(t *testing.T) {
	line := "4242 (we ird) name) S 1 777 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	pgrp, start, ok := parseProcStat(line)
	if !ok || pgrp != 777 || start != 987654 {
		t.Fatalf("parseProcStat = %d, %d, %v, want 777, 987654, true", pgrp, start, ok)
	}
	if _, _, ok := parseProcStat("4242 (x) S 1"); ok {
		t.Fatal("a truncated stat line parsed")
	}
}
