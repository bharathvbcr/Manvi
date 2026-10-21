package main

import "testing"

func TestNoticeOnceReportsAChangedSetAndARecurrence(t *testing.T) {
	var once noticeOnce
	a := []string{"x", "y"}
	if got := once.fresh(a); len(got) != 2 {
		t.Fatalf("first report: %v", got)
	}
	if got := once.fresh([]string{"y", "x"}); got != nil {
		t.Fatalf("the same set in another order was reported again: %v", got)
	}
	if got := once.fresh([]string{"x", "y", "z"}); len(got) != 3 {
		t.Fatalf("a changed set was not reported: %v", got)
	}
	if got := once.fresh(nil); len(got) != 0 {
		t.Fatalf("a cleared set reported: %v", got)
	}
	if got := once.fresh(a); len(got) != 2 {
		t.Fatalf("a recurrence after clearing was not reported: %v", got)
	}
}
