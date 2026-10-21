package lessons

import (
	"errors"
	"strings"
)

// Scrub passes every text field of an accepted result through clean, the
// session's credential scrubber, and reports whether it changed anything.
//
// A lesson is written to disk in a directory people commit and copy, and
// printed to a terminal, from text a model composed out of a log. The log was
// scrubbed as it was written, but the model is free to reassemble a secret from
// pieces or to copy one out of a file it was shown, so the answer is scrubbed
// again at the last point before it persists. The scrubbed text is what is
// stored and printed; the raw text goes nowhere. The entry's name is derived
// from the cue, so scrubbing happens before Save and a secret cannot reach a
// file name either.
//
// A field the scrubber empties is an error: a lesson whose cue was the secret
// has no cue left to file it under.
func Scrub(r Result, clean func(string) string) (Result, bool, error) {
	if clean == nil || (r.Experience == nil && len(r.Edits) == 0) {
		return r, false, nil
	}
	changed := false
	apply := func(s string) string {
		out := clean(s)
		if out != s {
			changed = true
		}
		return out
	}
	out := Result{NoEntry: r.NoEntry}
	for _, e := range r.Edits {
		out.Edits = append(out.Edits, Edit{Change: apply(e.Change), Verdict: e.Verdict, Note: apply(e.Note)})
	}
	if r.Experience != nil {
		e := *r.Experience
		e.Cue, e.Lesson, e.Evidence = apply(e.Cue), apply(e.Lesson), apply(e.Evidence)
		for _, f := range []string{e.Cue, e.Lesson, e.Evidence} {
			if strings.TrimSpace(f) == "" {
				return Result{}, false, errors.New("lessons: scrubbing left a field empty, so there is no lesson to store")
			}
		}
		out.Experience = &e
	}
	for _, e := range out.Edits {
		if strings.TrimSpace(e.Change) == "" {
			return Result{}, false, errors.New("lessons: scrubbing left an attribution row with no change")
		}
	}
	return out, changed, nil
}
